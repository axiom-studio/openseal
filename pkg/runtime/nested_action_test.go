package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type nestedActionFixture struct {
	store       KernelStore
	scope       Scope
	now         time.Time
	run         *AgentRun
	turn        *AgentTurn
	catalog     *skill.Catalog
	coordinator *ActionCoordinator
	binding     *TurnRunnerBinding
	closeTurn   func()
}

func nestedActionStores(t *testing.T) []struct {
	name string
	open func(*testing.T) KernelStore
} {
	stores := []struct {
		name string
		open func(*testing.T) KernelStore
	}{
		{name: "memory", open: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", open: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "nested.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		stores = append(stores, struct {
			name string
			open func(*testing.T) KernelStore
		}{name: "postgres", open: func(t *testing.T) KernelStore {
			store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema("openseal_nested_"+uuid.NewString()[:8]))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
				_ = store.Close()
			})
			return store
		}})
	}
	return stores
}

// newNestedActionFixture opens a running Run whose Turn is in flight and
// offers two actions: a read and an external write that needs approval.
func newNestedActionFixture(t *testing.T, store KernelStore) *nestedActionFixture {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	catalog := skill.NewCatalog()
	object := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}}
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "repo", Version: "1.0.0", Name: "Repo", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://repo.invalid"},
		Actions: map[string]skill.Action{
			"read_file":  {Name: "read_file", Description: "Read a file", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: object, Idempotency: skill.IdempotencySupported},
			"push":       {Name: "push", Description: "Push a branch", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal, InputSchema: object, Idempotency: skill.IdempotencySupported},
			"delete_all": {Name: "delete_all", Description: "Delete everything", SideEffect: skill.SideEffectDestructive, Risk: skill.RiskLevelDestructive, InputSchema: object, Idempotency: skill.IdempotencySupported},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "repo-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", AllowedActions: []string{"read_file", "push", "delete_all"}, MaximumRisk: skill.RiskLevelDestructive, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	offered, err := catalog.ListModelActions(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	// The Turn offers only the read and the push.
	binding := &TurnRunnerBinding{DeploymentID: "agent"}
	for _, action := range offered {
		if action.Action != "delete_all" {
			binding.ModelActions = append(binding.ModelActions, action)
		}
	}
	if len(binding.ModelActions) != 2 {
		t.Fatalf("offered actions = %#v", binding.ModelActions)
	}
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "check the repo", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	expires := now.Add(time.Hour)
	turn, err := store.CreateAgentTurn(ctx, &AgentTurn{
		ID: "turn-" + uuid.NewString(), Scope: scope, RunID: run.ID, Status: AgentTurnStatusRunning, LeaseOwner: "agent-worker", LeaseExpiresAt: &expires,
		Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := ActionPolicyEvaluatorFunc(func(_ context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		if input.Bound.Action.SideEffect == skill.SideEffectRead {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		}
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "writes need review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	coordinator := NewActionCoordinator(store, store, catalog, policy)
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	fixture := &nestedActionFixture{store: store, scope: scope, now: now, run: run, turn: turn, catalog: catalog, coordinator: coordinator, binding: binding}
	fixture.closeTurn = coordinator.OpenNestedActionTurn(run, turn, binding)
	t.Cleanup(fixture.closeTurn)
	return fixture
}

func (f *nestedActionFixture) request(capability, key string) NestedActionRequest {
	return NestedActionRequest{
		Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, Capability: capability, Origin: ActionOriginVM,
		Arguments: map[string]interface{}{"path": "README.md"}, OriginRef: "command-1", IdempotencyKey: key,
	}
}

func (f *nestedActionFixture) worker(t *testing.T) *ActionWorker {
	worker := NewActionWorker(f.store, f.catalog, nil, ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		if input.Call.Origin != ActionOriginVM || input.Run == nil || input.Run.Status != AgentRunStatusRunning {
			t.Fatalf("nested dispatch input = %#v", input)
		}
		return map[string]interface{}{"content": "hello", "path": input.Arguments["path"]}, nil
	}))
	worker.now = func() time.Time { return f.now.Add(2 * time.Second) }
	return worker
}

func (f *nestedActionFixture) assertRunUnchanged(t *testing.T) {
	t.Helper()
	current, err := f.store.GetAgentRun(context.Background(), f.scope, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != AgentRunStatusRunning || current.Revision != f.run.Revision || current.LeaseOwner != "agent-worker" || current.WakeCondition != nil {
		t.Fatalf("nested call changed the Run: %#v", current)
	}
}

func TestNestedReadActionRunsWithoutChangingTheRun(t *testing.T) {
	for _, testCase := range nestedActionStores(t) {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.open(t))
			result, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil {
				t.Fatal(err)
			}
			call := result.Call
			if !result.Created || call.Status != ActionCallStatusReady || call.Origin != ActionOriginVM || call.OriginRef != "command-1" ||
				call.TurnID != f.turn.ID || call.BindingID != "repo-binding" || call.Action != "read_file" || result.Settled() {
				t.Fatalf("nested proposal = %#v", result)
			}
			f.assertRunUnchanged(t)
			replay, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil || replay.Created || replay.Call.ID != call.ID {
				t.Fatalf("idempotent replay = %#v, %v", replay, err)
			}

			executed, err := f.worker(t).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["content"] != "hello" || executed.Run != nil {
				t.Fatalf("nested execution = %#v", executed)
			}
			if executed.Event.Payload["origin"] != "vm" || executed.Event.Payload["originRef"] != "command-1" || executed.Event.TurnID != f.turn.ID {
				t.Fatalf("nested activity is not groupable: %#v", executed.Event)
			}
			f.assertRunUnchanged(t)
			got, err := f.coordinator.GetNested(ctx, f.scope, f.run.ID, call.ID)
			if err != nil || !got.Settled() || got.Call.Status != ActionCallStatusSucceeded {
				t.Fatalf("nested get = %#v, %v", got, err)
			}
			events, err := f.store.ListActivity(ctx, ActivityFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil {
				t.Fatal(err)
			}
			grouped := map[string]bool{}
			for _, event := range events {
				if event.Payload["actionCallId"] == call.ID {
					if event.Payload["origin"] != "vm" || event.Payload["originRef"] != "command-1" {
						t.Fatalf("nested activity %s is not groupable: %#v", event.EventType, event.Payload)
					}
					grouped[event.EventType] = true
				}
			}
			if !grouped["action.proposed"] || !grouped["action.succeeded"] {
				t.Fatalf("nested activity = %#v", grouped)
			}
		})
	}
}

func TestNestedActionRefusesCapabilitiesTheTurnDidNotOffer(t *testing.T) {
	for _, testCase := range nestedActionStores(t) {
		t.Run(testCase.name, func(t *testing.T) {
			f := newNestedActionFixture(t, testCase.open(t))
			for _, capability := range []string{"repo.delete_all", "other.read_file", "read_file"} {
				if _, err := f.coordinator.ProposeNested(context.Background(), f.request(capability, "bridge-"+capability)); !errors.Is(err, ErrNestedActionNotAuthorized) {
					t.Fatalf("%s: err = %v, want not authorized", capability, err)
				}
			}
			calls, err := f.store.ListActionCalls(context.Background(), ActionFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil || len(calls) != 0 {
				t.Fatalf("refused calls were recorded: %#v, %v", calls, err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedActionRequiresTheRunsInFlightTurn(t *testing.T) {
	for _, testCase := range nestedActionStores(t) {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.open(t))
			other := f.request("repo.read_file", "bridge-other")
			other.TurnID = "turn-unknown"
			if _, err := f.coordinator.ProposeNested(ctx, other); !errors.Is(err, ErrNestedTurnNotActive) {
				t.Fatalf("unknown turn err = %v", err)
			}
			wrongRun := f.request("repo.read_file", "bridge-run")
			wrongRun.RunID = "run-unknown"
			if _, err := f.coordinator.ProposeNested(ctx, wrongRun); !errors.Is(err, ErrNestedTurnNotActive) {
				t.Fatalf("wrong run err = %v", err)
			}

			// A call made while the Turn ran is canceled, not dispatched, once
			// the Turn has finished.
			pending, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil {
				t.Fatal(err)
			}
			finished := cloneAgentTurn(f.turn)
			finished.Status = AgentTurnStatusCompleted
			completedAt := f.now.Add(time.Second)
			finished.CompletedAt = &completedAt
			finished.Revision++
			if err := f.store.UpdateAgentTurn(ctx, finished, f.turn.Revision, "agent-worker"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-2")); !errors.Is(err, ErrNestedTurnNotActive) {
				t.Fatalf("finished turn err = %v", err)
			}
			worker := NewActionWorker(f.store, f.catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				t.Fatal("a nested call of a finished Turn was dispatched")
				return nil, nil
			}))
			worker.now = func() time.Time { return f.now.Add(2 * time.Second) }
			executed, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.ID != pending.Call.ID || executed.Call.Status != ActionCallStatusCanceled || executed.Run != nil {
				t.Fatalf("stale nested call = %#v", executed)
			}
			f.assertRunUnchanged(t)

			f.closeTurn()
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-3")); !errors.Is(err, ErrNestedTurnNotActive) {
				t.Fatalf("closed turn err = %v", err)
			}
		})
	}
}

func TestNestedWriteActionWaitsForApprovalWithoutChangingTheRun(t *testing.T) {
	for _, testCase := range nestedActionStores(t) {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.open(t))
			result, err := f.coordinator.ProposeNested(ctx, f.request("repo.push", "bridge-push"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Call.Status != ActionCallStatusWaitingApproval || result.Approval == nil || result.Approval.Status != ApprovalStatusPending ||
				result.Approval.ActionCallID != result.Call.ID || !result.Settled() {
				t.Fatalf("nested write = %#v", result)
			}
			f.assertRunUnchanged(t)

			approvals := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{})
			approvals.now = func() time.Time { return f.now.Add(2 * time.Second) }
			resolved, err := approvals.Resolve(ctx, ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: result.Approval.ID, ExpectedRevision: result.Approval.Revision, DecisionID: "decision-1",
				Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "user"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.Resolved || resolved.Call.Status != ActionCallStatusReady || resolved.Approval.Status != ApprovalStatusApproved ||
				resolved.Run == nil || resolved.Run.Revision != f.run.Revision || resolved.Event.Payload["origin"] != "vm" {
				t.Fatalf("nested approval = %#v", resolved)
			}
			f.assertRunUnchanged(t)

			executed, err := f.worker(t).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.ID != result.Call.ID || executed.Call.Status != ActionCallStatusSucceeded || executed.Run != nil {
				t.Fatalf("approved nested execution = %#v", executed)
			}
			f.assertRunUnchanged(t)

			rejected, err := f.coordinator.ProposeNested(ctx, f.request("repo.push", "bridge-push-2"))
			if err != nil {
				t.Fatal(err)
			}
			denied, err := approvals.Resolve(ctx, ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: rejected.Approval.ID, ExpectedRevision: rejected.Approval.Revision, DecisionID: "decision-2",
				Decision: ApprovalDecisionReject, Principal: ApprovalPrincipal{Type: "user", ID: "user"}, Reason: "not now",
			})
			if err != nil || denied.Call.Status != ActionCallStatusDenied || denied.Approval.Status != ApprovalStatusRejected {
				t.Fatalf("rejected nested approval = %#v, %v", denied, err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedActionBoundsPerTurn(t *testing.T) {
	for _, testCase := range nestedActionStores(t) {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.open(t))
			f.coordinator.nestedMaxCalls, f.coordinator.nestedMaxConcurrent = 5, 2
			for index := 0; index < 2; index++ {
				if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", fmt.Sprint("bridge-", index))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-over")); !errors.Is(err, ErrNestedActionLimit) {
				t.Fatalf("concurrency bound err = %v", err)
			}
			worker := f.worker(t)
			for index := 0; index < 2; index++ {
				if _, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			for index := 2; index < 5; index++ {
				if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", fmt.Sprint("bridge-", index))); err != nil {
					t.Fatalf("call %d: %v", index, err)
				}
				if _, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-total")); !errors.Is(err, ErrNestedActionLimit) {
				t.Fatalf("total bound err = %v", err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

// The worker opens the Turn's offered actions to nested calls only while the
// Turn runs.
func TestPreparedTurnRunnerOpensNestedActionsDuringTheTurn(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	f := newNestedActionFixture(t, store)
	f.closeTurn()
	var during error
	f.binding.Runner = TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		_, during = f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, OutputSummary: "done"}, nil
	})
	runner := preparedRuntimeTurnRunner{binding: f.binding, actions: f.coordinator}
	if _, err := runner.RunTurn(ctx, TurnExecutionContext{Run: f.run, Turn: f.turn}); err != nil {
		t.Fatal(err)
	}
	if during != nil {
		t.Fatalf("nested call during the Turn: %v", during)
	}
	if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-2")); !errors.Is(err, ErrNestedTurnNotActive) {
		t.Fatalf("nested call after the Turn: %v", err)
	}
}
