package action_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type nestedActionFixture struct {
	store       models.KernelStore
	scope       models.Scope
	now         time.Time
	run         *models.AgentRun
	turn        *models.AgentTurn
	catalog     *skill.Catalog
	coordinator *action.Coordinator
	clock       func() time.Time // the coordinator's clock
	binding     *agentrun.TurnRunnerBinding
	closeTurn   func()
}

// newNestedActionFixture opens a running Run whose step is running and
// offers two actions: a read and an external write that needs approval.
func newNestedActionFixture(t *testing.T, store models.KernelStore) *nestedActionFixture {
	t.Helper()
	object := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}}
	definition := &skill.Definition{
		ID: "repo", Version: "1.0.0", Name: "Repo", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://repo.invalid"},
		Actions: map[string]skill.Action{
			"read_file":  {Name: "read_file", Description: "Read a file", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: object, Idempotency: skill.IdempotencySupported},
			"push":       {Name: "push", Description: "Push a branch", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal, InputSchema: object, Idempotency: skill.IdempotencySupported},
			"delete_all": {Name: "delete_all", Description: "Delete everything", SideEffect: skill.SideEffectDestructive, Risk: skill.RiskLevelDestructive, InputSchema: object, Idempotency: skill.IdempotencySupported},
		},
	}
	binding := &skill.Binding{
		ID: "repo-binding", DeploymentID: "agent", SkillID: "repo", SkillVersion: "1.0.0", AllowedActions: []string{"read_file", "push", "delete_all"}, MaximumRisk: skill.RiskLevelDestructive, Revision: 1,
	}
	// The step offers only the read and the push.
	return newNestedActionFixtureWith(t, store, definition, binding, func(action capability.ModelAction) bool { return action.Action != "delete_all" })
}

// newNestedActionFixtureWith opens the same Run and step offering the actions
// of definition's binding that offer selects.
func newNestedActionFixtureWith(t *testing.T, store models.KernelStore, definition *skill.Definition, skillBinding *skill.Binding, offer func(capability.ModelAction) bool) *nestedActionFixture {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	skillBinding.Scope = skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if err := catalog.Bind(ctx, skillBinding); err != nil {
		t.Fatal(err)
	}
	offered, err := catalog.ListModelActions(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	binding := &agentrun.TurnRunnerBinding{DeploymentID: "agent"}
	for _, action := range offered {
		if offer(action) {
			binding.ModelActions = append(binding.ModelActions, action)
		}
	}
	if len(binding.ModelActions) != 2 {
		t.Fatalf("offered actions = %#v", binding.ModelActions)
	}
	portfolio := portfolio.NewService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "check the repo", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	expires := now.Add(time.Hour)
	turn, err := store.CreateAgentTurn(ctx, &models.AgentTurn{
		ID: "step-" + uuid.NewString(), Scope: scope, RunID: run.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel, LeaseOwner: "agent-worker", LeaseExpiresAt: &expires,
		Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := action.PolicyEvaluatorFunc(func(_ context.Context, input action.PolicyInput) (action.PolicyDecision, error) {
		if input.Bound.Action.SideEffect == skill.SideEffectRead {
			return action.PolicyDecision{Disposition: action.DispositionAllow}, nil
		}
		return action.PolicyDecision{
			Disposition: action.DispositionRequireApproval, Reason: "writes need review",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	fixture := &nestedActionFixture{store: store, scope: scope, now: now, run: run, turn: turn, catalog: catalog, binding: binding}
	fixture.clock = func() time.Time { return now.Add(time.Second) }
	fixture.coordinator = action.NewCoordinator(store, store, catalog, policy, nil, models.WithClock(func() time.Time { return fixture.clock() }))
	fixture.closeTurn = fixture.coordinator.OpenNestedActionTurn(run, turn, binding)
	t.Cleanup(fixture.closeTurn)
	return fixture
}

// finishStep completes the fixture's running step in the store, as the run
// loop does at step end. It leaves the step's nested authority open.
func (f *nestedActionFixture) finishStep(t *testing.T) {
	t.Helper()
	finished := models.CloneAgentTurn(f.turn)
	finished.Status = models.AgentTurnStatusCompleted
	completedAt := f.now.Add(time.Second)
	finished.CompletedAt = &completedAt
	finished.Revision++
	if err := f.store.UpdateAgentTurn(context.Background(), finished, f.turn.Revision, f.turn.LeaseOwner); err != nil {
		t.Fatal(err)
	}
	f.turn = finished
}

// openStep starts the Run's next running step under the Run's lease and opens
// its nested authority, as the run loop does when a step begins.
func (f *nestedActionFixture) openStep(t *testing.T, sequence int64) {
	t.Helper()
	expires := f.now.Add(time.Hour)
	step, err := f.store.CreateAgentTurn(context.Background(), &models.AgentTurn{
		ID: "step-" + uuid.NewString(), Scope: f.scope, RunID: f.run.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel,
		LeaseOwner: "agent-worker", LeaseExpiresAt: &expires, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now, StartedAt: f.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if step.Sequence != sequence {
		t.Fatalf("step sequence = %d, want %d", step.Sequence, sequence)
	}
	f.turn = step
	f.closeTurn = f.coordinator.OpenNestedActionTurn(f.run, step, f.binding)
	t.Cleanup(f.closeTurn)
}

func (f *nestedActionFixture) request(capability, key string) action.NestedActionRequest {
	return action.NestedActionRequest{
		Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, Capability: capability, Origin: models.ActionOriginVM,
		Arguments: map[string]interface{}{"path": "README.md"}, OriginRef: "command-1", IdempotencyKey: key,
	}
}

func (f *nestedActionFixture) worker(t *testing.T) *action.Worker {
	worker := action.NewWorker(f.store, f.catalog, nil, action.DispatcherFunc(func(_ context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		if input.Call.Origin != models.ActionOriginVM || input.Run == nil || input.Run.Status != models.AgentRunStatusRunning {
			t.Fatalf("nested dispatch input = %#v", input)
		}
		return map[string]interface{}{"content": "hello", "path": input.Arguments["path"]}, nil
	}), models.WithClock(func() time.Time { return f.now.Add(2 * time.Second) }))
	return worker
}

func (f *nestedActionFixture) assertRunUnchanged(t *testing.T) {
	t.Helper()
	current, err := f.store.GetAgentRun(context.Background(), f.scope, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != models.AgentRunStatusRunning || current.Revision != f.run.Revision || current.LeaseOwner != "agent-worker" || current.WakeCondition != nil {
		t.Fatalf("nested call changed the Run: %#v", current)
	}
}

func TestNestedReadActionRunsWithoutChangingTheRun(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.Open(t))
			result, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil {
				t.Fatal(err)
			}
			call := result.Call
			if !result.Created || call.Status != models.ActionCallStatusReady || call.Origin != models.ActionOriginVM || call.OriginRef != "command-1" ||
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
			if executed.Call.Status != models.ActionCallStatusSucceeded || executed.Call.Output["content"] != "hello" || executed.Run != nil {
				t.Fatalf("nested execution = %#v", executed)
			}
			if executed.Event.Payload["origin"] != "vm" || executed.Event.Payload["originRef"] != "command-1" || executed.Event.TurnID != f.turn.ID {
				t.Fatalf("nested activity is not groupable: %#v", executed.Event)
			}
			f.assertRunUnchanged(t)
			got, err := f.coordinator.GetNested(ctx, f.scope, f.run.ID, call.ID)
			if err != nil || !got.Settled() || got.Call.Status != models.ActionCallStatusSucceeded {
				t.Fatalf("nested get = %#v, %v", got, err)
			}
			events, err := f.store.ListActivity(ctx, models.ActivityFilter{Scope: f.scope, RunID: f.run.ID})
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

func TestNestedActionRefusesCapabilitiesTheStepDidNotOffer(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			f := newNestedActionFixture(t, testCase.Open(t))
			for _, capability := range []string{"repo.delete_all", "other.read_file", "read_file"} {
				if _, err := f.coordinator.ProposeNested(context.Background(), f.request(capability, "bridge-"+capability)); !errors.Is(err, action.ErrNestedActionNotAuthorized) {
					t.Fatalf("%s: err = %v, want not authorized", capability, err)
				}
			}
			calls, err := f.store.ListActionCalls(context.Background(), models.ActionFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil || len(calls) != 0 {
				t.Fatalf("refused calls were recorded: %#v, %v", calls, err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedActionRequiresTheRunsRunningStep(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.Open(t))
			other := f.request("repo.read_file", "bridge-other")
			other.TurnID = "step-unknown"
			if _, err := f.coordinator.ProposeNested(ctx, other); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("unknown step err = %v", err)
			}
			wrongRun := f.request("repo.read_file", "bridge-run")
			wrongRun.RunID = "run-unknown"
			if _, err := f.coordinator.ProposeNested(ctx, wrongRun); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("wrong run err = %v", err)
			}

			// A call made while the step ran is canceled, not dispatched, once
			// the step has finished, even while its authority is still open.
			pending, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil {
				t.Fatal(err)
			}
			finishedStep := f.turn
			f.finishStep(t)
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-2")); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("finished step err = %v", err)
			}
			worker := action.NewWorker(f.store, f.catalog, nil, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				t.Fatal("a nested call of a finished step was dispatched")
				return nil, nil
			}), models.WithClock(func() time.Time { return f.now.Add(2 * time.Second) }))
			executed, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.ID != pending.Call.ID || executed.Call.Status != models.ActionCallStatusCanceled || executed.Run != nil {
				t.Fatalf("stale nested call = %#v", executed)
			}
			f.assertRunUnchanged(t)

			f.closeTurn()
			if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-3")); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("closed step err = %v", err)
			}

			// The Run's next step is the only one nested calls may use: the
			// finished step stays closed while the new one admits calls.
			f.openStep(t, 2)
			stale := f.request("repo.read_file", "bridge-4")
			stale.TurnID = finishedStep.ID
			if _, err := f.coordinator.ProposeNested(ctx, stale); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("previous step err = %v", err)
			}
			next, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1"))
			if err != nil {
				t.Fatal(err)
			}
			if !next.Created || next.Call.ID == pending.Call.ID || next.Call.TurnID != f.turn.ID {
				t.Fatalf("next step call = %#v", next)
			}
			executed, err = f.worker(t).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.ID != next.Call.ID || executed.Call.Status != models.ActionCallStatusSucceeded {
				t.Fatalf("next step execution = %#v", executed)
			}
			f.assertRunUnchanged(t)
		})
	}
}

// A step admits nested calls only while it is leased, live, by the worker
// holding the Run's lease.
func TestNestedActionRequiresTheStepLeasedByTheRunsWorker(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.Open(t))
			for index, lease := range []struct {
				owner   string
				expires time.Time
			}{
				{owner: "other-worker", expires: f.now.Add(time.Hour)},
				{owner: "agent-worker", expires: f.now},
			} {
				f.finishStep(t)
				f.closeTurn()
				step, err := f.store.CreateAgentTurn(ctx, &models.AgentTurn{
					ID: fmt.Sprint("step-lease-", index), Scope: f.scope, RunID: f.run.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel,
					LeaseOwner: lease.owner, LeaseExpiresAt: &lease.expires, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now, StartedAt: f.now,
				})
				if err != nil {
					t.Fatal(err)
				}
				f.turn = step
				f.closeTurn = f.coordinator.OpenNestedActionTurn(f.run, step, f.binding)
				t.Cleanup(f.closeTurn)
				if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-1")); !errors.Is(err, models.ErrNestedTurnNotActive) {
					t.Fatalf("lease %#v err = %v", lease, err)
				}
			}
			calls, err := f.store.ListActionCalls(ctx, models.ActionFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil || len(calls) != 0 {
				t.Fatalf("calls of unleased steps were recorded: %#v, %v", calls, err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedWriteActionWaitsForApprovalWithoutChangingTheRun(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.Open(t))
			result, err := f.coordinator.ProposeNested(ctx, f.request("repo.push", "bridge-push"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Call.Status != models.ActionCallStatusWaitingApproval || result.Approval == nil || result.Approval.Status != models.ApprovalStatusPending ||
				result.Approval.ActionCallID != result.Call.ID || !result.Settled() {
				t.Fatalf("nested write = %#v", result)
			}
			f.assertRunUnchanged(t)

			approvals := action.NewApprovalCoordinator(f.store, f.store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return f.now.Add(2 * time.Second) }))
			resolved, err := approvals.Resolve(ctx, action.ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: result.Approval.ID, ExpectedRevision: result.Approval.Revision, DecisionID: "decision-1",
				Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "user"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.Resolved || resolved.Call.Status != models.ActionCallStatusReady || resolved.Approval.Status != models.ApprovalStatusApproved ||
				resolved.Run == nil || resolved.Run.Revision != f.run.Revision || resolved.Event.Payload["origin"] != "vm" {
				t.Fatalf("nested approval = %#v", resolved)
			}
			f.assertRunUnchanged(t)

			executed, err := f.worker(t).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.ID != result.Call.ID || executed.Call.Status != models.ActionCallStatusSucceeded || executed.Run != nil {
				t.Fatalf("approved nested execution = %#v", executed)
			}
			f.assertRunUnchanged(t)

			rejected, err := f.coordinator.ProposeNested(ctx, f.request("repo.push", "bridge-push-2"))
			if err != nil {
				t.Fatal(err)
			}
			denied, err := approvals.Resolve(ctx, action.ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: rejected.Approval.ID, ExpectedRevision: rejected.Approval.Revision, DecisionID: "decision-2",
				Decision: models.ApprovalDecisionReject, Principal: models.ApprovalPrincipal{Type: "user", ID: "user"}, Reason: "not now",
			})
			if err != nil || denied.Call.Status != models.ActionCallStatusDenied || denied.Approval.Status != models.ApprovalStatusRejected {
				t.Fatalf("rejected nested approval = %#v, %v", denied, err)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedActionBoundsArePerStep(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedActionFixture(t, testCase.Open(t))
			f.coordinator.SetNestedLimits(5, 2)
			worker := f.worker(t)
			exhaust := func(step int) {
				t.Helper()
				for index := 0; index < 2; index++ {
					if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", fmt.Sprint("bridge-", index))); err != nil {
						t.Fatalf("step %d call %d: %v", step, index, err)
					}
				}
				if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-over")); !errors.Is(err, models.ErrNestedActionLimit) {
					t.Fatalf("step %d concurrency bound err = %v", step, err)
				}
				for index := 0; index < 2; index++ {
					if _, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil {
						t.Fatal(err)
					}
				}
				for index := 2; index < 5; index++ {
					if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", fmt.Sprint("bridge-", index))); err != nil {
						t.Fatalf("step %d call %d: %v", step, index, err)
					}
					if _, err := worker.RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-total")); !errors.Is(err, models.ErrNestedActionLimit) {
					t.Fatalf("step %d total bound err = %v", step, err)
				}
			}
			exhaust(1)
			// The next step of the same Run starts with fresh bounds and its
			// own idempotency keys.
			f.finishStep(t)
			f.closeTurn()
			f.openStep(t, 2)
			exhaust(2)
			calls, err := f.store.ListActionCalls(ctx, models.ActionFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil {
				t.Fatal(err)
			}
			perStep := map[string]int{}
			for _, call := range calls {
				if call.Status != models.ActionCallStatusSucceeded {
					t.Fatalf("call %s status = %s", call.ID, call.Status)
				}
				perStep[call.TurnID]++
			}
			if len(perStep) != 2 || perStep[f.turn.ID] != 5 {
				t.Fatalf("calls per step = %#v", perStep)
			}
			f.assertRunUnchanged(t)
		})
	}
}
