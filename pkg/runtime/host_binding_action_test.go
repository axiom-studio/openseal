package runtime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// durableCatalog makes the coordinator fence proposals as a store-backed
// catalog does.
type durableCatalog struct{ *skill.Catalog }

func (durableCatalog) UsesDurableBindings() bool { return true }

// A connected app's tool is proposed, fenced and executed for the run it was
// supplied to, with no stored binding row; another user's run cannot reach it.
func TestHostBindingToolRunsForItsRunOnly(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	claimed := actiontest.ClaimedOwnedRun(t, store, scope, time.Now().UTC(), "worker", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"})
	// Another run in the workspace, e.g. another user's chat.
	other := actiontest.ClaimedOwnedRun(t, store, scope, time.Now().UTC(), "worker", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"})
	catalog := skillbindingtest.HostConnectorCatalog(t, scope, map[string]bool{claimed.ID: true})
	allow := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow, Reason: "read-only"}, nil
	})
	coordinator := action.NewActionCoordinator(store, store, durableCatalog{catalog}, allow, nil)
	propose := func(run *models.AgentRun, key string) (*models.ActionProposalResult, error) {
		return coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{
			Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", BindingID: "mcp:linear", BindingRevision: 1,
			SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: "read", Summary: "Search Linear",
			Arguments: map[string]interface{}{"tool": "search", "hash": "linear-search", "arguments": map[string]interface{}{"title": "mcp bug"}},
			Actor:     models.ActivityActor{Type: "worker", ID: "worker"},
		}, key))
	}
	result, err := propose(claimed, "search")
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.BindingID != "mcp:linear" || result.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("call = %#v", result.Call)
	}

	dispatched := 0
	worker := action.NewActionWorker(store, catalog, nil, action.ActionDispatcherFunc(func(_ context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatched++
		if !input.Bound.Host || input.Bound.Binding.ID != "mcp:linear" || input.Arguments["tool"] != "search" {
			t.Fatalf("dispatch = %#v", input.Bound)
		}
		return map[string]interface{}{"ok": true}, nil
	}))
	// The run loop executes its governed call inline under the Run's lease.
	executed, err := worker.ExecuteOwnLoopCall(t.Context(), scope, result.Call.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 1 || executed.Call.Status != models.ActionCallStatusSucceeded {
		t.Fatalf("executed = %#v (dispatched %d)", executed.Call, dispatched)
	}

	// The other run gets no connection from the host and cannot name it.
	if _, err := propose(other, "other"); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("a run the host supplied nothing to reached the connected app: %v", err)
	}
}

// Credential redemption of a host binding uses the binding the host
// re-resolved for the lease's run, and fails closed when it is missing,
// belongs to another Agent, or collides with a stored row.
func TestSQLiteCredentialRedemptionOfHostBindings(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		keepRow   bool
		host      func(*skill.Binding) *skill.Binding
		wantError bool
	}{
		{name: "host binding", host: func(b *skill.Binding) *skill.Binding { return b }},
		{name: "no host binding", host: func(*skill.Binding) *skill.Binding { return nil }, wantError: true},
		{name: "another Agent's binding", host: func(b *skill.Binding) *skill.Binding {
			other := *b
			other.DeploymentID = "other-agent"
			return &other
		}, wantError: true},
		{name: "stale revision", host: func(b *skill.Binding) *skill.Binding {
			other := *b
			other.Revision++
			return &other
		}, wantError: true},
		{name: "collides with a stored row", keepRow: true, host: func(b *skill.Binding) *skill.Binding { return b }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "host-redemption.db")
			store, err := sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			sqlDB := openTestSQLiteDB(t, path)
			defer store.Close()
			request, binding := storetest.ActionCredentialLeaseFixture(t, ctx, store, "nonce-host-binding-0001")
			if !test.keepRow {
				if _, err := sqlDB.ExecContext(ctx, `DELETE FROM skill_bindings WHERE id = ?`, binding.ID); err != nil {
					t.Fatal(err)
				}
			}
			request.HostBinding = test.host(binding)
			err = store.RedeemActionCredentialLease(ctx, request)
			if test.wantError != (err != nil) {
				t.Fatalf("redeem error = %v, want error %v", err, test.wantError)
			}
			var consumed int
			if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions`).Scan(&consumed); err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			if (consumed == 1) == test.wantError {
				t.Fatalf("consumed nonces = %d", consumed)
			}
		})
	}
}

// A routine grants a connected app's tool by its own name; the routine's runs
// then use it unasked through the binding the host supplies to them.
func TestRoutineGrantsCoverHostBindingTools(t *testing.T) {
	f := routineGrantFixture(t)
	allowed := map[string]bool{f.run.ID: true}
	hosted := skillbindingtest.HostConnectorCatalog(t, f.run.Scope, allowed)
	provider := func(ctx context.Context, scope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		set, err := hosted.HostBindings(ctx, scope, deploymentID, actor)
		return set, err
	}
	// The fixture's catalog also needs the connector definition.
	input := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"}, "arguments": map[string]interface{}{"type": "object"}}, "required": []interface{}{"tool", "hash", "arguments"}}
	if err := f.catalog.Register(t.Context(), &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "mcp", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.catalog.SetHostBindingProvider(provider)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "mcp.linear.create_issue"})
	f.reviewEverything(t, activation)
	if pin := activation.Task.Grants[0].ToolArguments; pin["hash"] != "linear-create_issue" {
		t.Fatalf("pin = %#v", pin)
	}
	coordinator := action.NewActionCoordinator(f.store, f.store, f.catalog, action.NewDefaultActionPolicy(), nil)
	occurrence := f.routineOccurrence(t, activation, "host-occurrence")
	allowed[occurrence.ID] = true
	proposeCreateIssue := func(run *models.AgentRun, toolCallID string) (*models.ActionProposalResult, error) {
		return coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{
			Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, BindingID: "mcp:linear", BindingRevision: 1,
			SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: "call",
			Arguments: map[string]interface{}{"tool": "create_issue", "hash": "linear-create_issue", "arguments": map[string]interface{}{"title": "Forecast"}},
		}, toolCallID))
	}
	if result, err := proposeCreateIssue(occurrence, "granted"); err != nil || result.Approval != nil || result.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("the granted connected tool asked: %#v, %v", result, err)
	}
	// The occurrence ends before the routine's next one is admitted.
	done := models.CloneAgentRun(occurrence)
	finished := time.Now().UTC()
	done.Status, done.LeaseOwner, done.LeaseExpiresAt, done.CompletedAt = models.AgentRunStatusCompleted, "", nil, &finished
	done.Revision++
	done.UpdatedAt = finished
	if _, err := f.store.UpdateAgentRunWithEvent(t.Context(), done, occurrence.Revision, &models.ActivityEvent{
		ID: "occurrence-done", Scope: done.Scope, RunID: done.ID, EventType: "run.completed", Summary: "Completed", CreatedAt: finished,
	}, &models.AgentRunLeaseGuard{WorkerID: "worker", Now: finished}); err != nil {
		t.Fatal(err)
	}
	// Disconnected for this run: the routine's tool is no longer available.
	later := f.routineOccurrence(t, activation, "host-occurrence-disconnected")
	if _, err := proposeCreateIssue(later, "disconnected"); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("a run without the connection used it: %v", err)
	}
}
