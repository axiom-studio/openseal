package runtime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// hostConnectorCatalog registers a connector Skill whose bindings the host
// supplies per run, as connected apps are: only runs in allowed get the
// linear binding.
func hostConnectorCatalog(t *testing.T, scope Scope, allowed map[string]bool) *skill.Catalog {
	t.Helper()
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "mcp", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		},
	}); err != nil {
		t.Fatal(err)
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"title": map[string]interface{}{"type": "string"}}}
	catalog.SetHostBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if skillScope.ID != scope.ID || !allowed[actor.RunID] {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@1", Services: map[string][]string{"mcp:linear": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:linear", Revision: 1, Scope: skillScope, DeploymentID: deploymentID, SkillID: "openseal.connector", SkillVersion: "1.0.0",
			AllowedActions: []string{"read", "call"}, MaximumRisk: skill.RiskLevelWrite, Slug: "linear",
			Tools: []skill.BindingTool{
				{Name: "create_issue", Description: "Create a Linear issue", Effect: skill.BindingToolEffectWrite, InputSchema: schema, Arguments: map[string]interface{}{"tool": "create_issue", "hash": "linear-create_issue"}},
				{Name: "search", Description: "Search Linear issues", Effect: skill.BindingToolEffectRead, InputSchema: schema, Arguments: map[string]interface{}{"tool": "search", "hash": "linear-search"}},
			},
		}}}, nil
	})
	return catalog
}

// durableCatalog makes the coordinator fence proposals as a store-backed
// catalog does.
type durableCatalog struct{ *skill.Catalog }

func (durableCatalog) UsesDurableBindings() bool { return true }

// A connected app's tool is proposed, fenced and executed for the run it was
// supplied to, with no stored binding row; another user's run cannot reach it.
func TestHostBindingToolRunsForItsRunOnly(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	claimed := claimedOwnedActionRun(t, store, scope, time.Now().UTC(), "worker", ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"})
	// Another run in the workspace, e.g. another user's chat.
	other := claimedOwnedActionRun(t, store, scope, time.Now().UTC(), "worker", ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"})
	catalog := hostConnectorCatalog(t, scope, map[string]bool{claimed.ID: true})
	allow := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "read-only"}, nil
	})
	coordinator := NewActionCoordinator(store, store, durableCatalog{catalog}, allow)
	propose := func(run *AgentRun, key string) (*ActionProposalResult, error) {
		return coordinator.Propose(t.Context(), loopProposeRequest(ProposeActionRequest{
			Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", BindingID: "mcp:linear", BindingRevision: 1,
			SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: "read", Summary: "Search Linear",
			Arguments: map[string]interface{}{"tool": "search", "hash": "linear-search", "arguments": map[string]interface{}{"title": "mcp bug"}},
			Actor:     ActivityActor{Type: "worker", ID: "worker"},
		}, key))
	}
	result, err := propose(claimed, "search")
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.BindingID != "mcp:linear" || result.Call.Status != ActionCallStatusReady {
		t.Fatalf("call = %#v", result.Call)
	}

	dispatched := 0
	worker := NewActionWorker(store, catalog, nil, ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
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
	if dispatched != 1 || executed.Call.Status != ActionCallStatusSucceeded {
		t.Fatalf("executed = %#v (dispatched %d)", executed.Call, dispatched)
	}

	// The other run gets no connection from the host and cannot name it.
	if _, err := propose(other, "other"); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("a run the host supplied nothing to reached the connected app: %v", err)
	}
}

// The durable fence admits a host binding only without a stored row of the
// same identity, and only while the call matches it exactly.
func TestActionProposalFenceChecksHostBindings(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	claimed := claimedOwnedActionRun(t, store, scope, time.Now().UTC(), "worker", ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"})
	catalog := hostConnectorCatalog(t, scope, map[string]bool{claimed.ID: true})
	bound, err := catalog.Resolve(skill.WithRunActor(t.Context(), skill.RunActor{RunID: claimed.ID, AgentID: "release-agent", Umbrella: skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "release-agent"}}),
		skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent", "openseal.connector", "1.0.0", "read", skill.BindingReference{ID: "mcp:linear", Revision: 1})
	if err != nil || !bound.Host {
		t.Fatalf("resolve = %#v, %v", bound, err)
	}
	call := &ActionCall{Scope: scope, DeploymentID: "release-agent", BindingID: "mcp:linear", BindingRevision: 1, SkillID: "openseal.connector", SkillVersion: "1.0.0"}
	if err := validateActionSubmissionBinding(call, hostActionBinding(bound)); err != nil {
		t.Fatalf("matching host binding refused: %v", err)
	}
	call.BindingRevision = 2
	if err := validateActionSubmissionBinding(call, hostActionBinding(bound)); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("stale host binding admitted: %v", err)
	}
	if hostActionBinding(&skill.BoundAction{Binding: bound.Binding}) != nil {
		t.Fatal("a stored binding was treated as host supplied")
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
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "host-redemption.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			request, binding := createSQLiteActionCredentialLeaseFixture(t, ctx, store, "nonce-host-binding-0001")
			if !test.keepRow {
				if _, err := store.db.ExecContext(ctx, `DELETE FROM skill_bindings WHERE id = ?`, binding.ID); err != nil {
					t.Fatal(err)
				}
			}
			request.HostBinding = test.host(binding)
			err = store.RedeemActionCredentialLease(ctx, request)
			if test.wantError != (err != nil) {
				t.Fatalf("redeem error = %v, want error %v", err, test.wantError)
			}
			var consumed int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions`).Scan(&consumed); err != nil && !errors.Is(err, sql.ErrNoRows) {
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
	hosted := hostConnectorCatalog(t, f.run.Scope, allowed)
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
	coordinator := NewActionCoordinator(f.store, f.store, f.catalog, NewDefaultActionPolicy())
	occurrence := f.routineOccurrence(t, activation, "host-occurrence")
	allowed[occurrence.ID] = true
	proposeCreateIssue := func(run *AgentRun, toolCallID string) (*ActionProposalResult, error) {
		return coordinator.Propose(t.Context(), loopProposeRequest(ProposeActionRequest{
			Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, BindingID: "mcp:linear", BindingRevision: 1,
			SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: "call",
			Arguments: map[string]interface{}{"tool": "create_issue", "hash": "linear-create_issue", "arguments": map[string]interface{}{"title": "Forecast"}},
		}, toolCallID))
	}
	if result, err := proposeCreateIssue(occurrence, "granted"); err != nil || result.Approval != nil || result.Call.Status != ActionCallStatusReady {
		t.Fatalf("the granted connected tool asked: %#v, %v", result, err)
	}
	// The occurrence ends before the routine's next one is admitted.
	done := cloneAgentRun(occurrence)
	finished := time.Now().UTC()
	done.Status, done.LeaseOwner, done.LeaseExpiresAt, done.CompletedAt = AgentRunStatusCompleted, "", nil, &finished
	done.Revision++
	done.UpdatedAt = finished
	if _, err := f.store.UpdateAgentRunWithEvent(t.Context(), done, occurrence.Revision, &ActivityEvent{
		ID: "occurrence-done", Scope: done.Scope, RunID: done.ID, EventType: "run.completed", Summary: "Completed", CreatedAt: finished,
	}, &AgentRunLeaseGuard{WorkerID: "worker", Now: finished}); err != nil {
		t.Fatal(err)
	}
	// Disconnected for this run: the routine's tool is no longer available.
	later := f.routineOccurrence(t, activation, "host-occurrence-disconnected")
	if _, err := proposeCreateIssue(later, "disconnected"); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("a run without the connection used it: %v", err)
	}
}

// Skill setup is refused for a service a connected app already covers for
// this run, and the refusal names the connected tools to use instead.
func TestSkillSetupDefersToAConnectedApp(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "a"}
	catalog := skillActionCatalog(t, ctx, scope, "agent")
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	hosted := hostConnectorCatalog(t, scope, map[string]bool{run.ID: true})
	if err := catalog.Register(ctx, mustDefinition(t, hosted, "openseal.connector")); err != nil {
		t.Fatal(err)
	}
	catalog.SetHostBindingProvider(func(ctx context.Context, scope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		return hosted.HostBindings(ctx, scope, deploymentID, actor)
	})
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{
			{ID: "linear", Version: "1.0.0", Name: "Linear", Readiness: skill.DiscoveryReadinessBindable},
			{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessBindable},
		}}, nil
	})
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	setup := func(callID, skillID string, run *AgentRun) (map[string]interface{}, error) {
		return dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: callID, Scope: scope, RunID: run.ID},
			Arguments: map[string]interface{}{"kind": "configure", "skillId": skillID, "skillVersion": "latest", "reason": "File a ticket"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	}
	_, err = setup("call-linear", "linear", run)
	var refusal *interactionRequestRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "Linear is already connected") ||
		!strings.Contains(refusal.message, "mcp.linear.create_issue") || !strings.Contains(refusal.message, "mcp.linear.search") {
		t.Fatalf("setup of a connected service = %v", err)
	}
	if requests, _ := store.ListSkillSetupRequests(ctx, scope, "agent", "chat"); len(requests) != 0 {
		t.Fatalf("a setup form was created: %#v", requests)
	}
	// Other services, and the same service for a run without the connection,
	// still get a setup form.
	if result, err := setup("call-reddit", "reddit.reader", run); err != nil || result["setupRequest"] == nil {
		t.Fatalf("unrelated setup = %#v, %v", result, err)
	}
	other := cloneAgentRun(run)
	other.ID = "other-users-run"
	if result, err := setup("call-other", "linear", other); err != nil || result["setupRequest"] == nil {
		t.Fatalf("setup for a run without the connection = %#v, %v", result, err)
	}
}

func mustDefinition(t *testing.T, catalog *skill.Catalog, id string) *skill.Definition {
	t.Helper()
	definition, err := catalog.GetDefinition(t.Context(), id, "1.0.0")
	if err != nil || definition == nil {
		t.Fatalf("definition %s: %v", id, err)
	}
	return definition
}

func TestServiceKey(t *testing.T) {
	for value, want := range map[string]string{
		"Linear": "linear", "linear-mcp": "linear", "skill-linear": "linear", "neondatabase/mcp-server-neon": "neondatabase-neon",
		"Google Drive": "google-drive", "MCP Server": "",
	} {
		if got := serviceKey(value); got != want {
			t.Fatalf("serviceKey(%q) = %q, want %q", value, got, want)
		}
	}
}
