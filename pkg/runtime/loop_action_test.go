package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/google/uuid"
)

const (
	governedWorker  = "loop-worker"
	governedWorker2 = "loop-worker-2"
	governedStep    = "step-1"
	governedDeploy  = "release_deploy"
	governedStatus  = "release_status"
)

var governedApprover = ApprovalPrincipal{Type: "user", ID: "user"}

// governedFixture is a running Run of release-agent claimed by governedWorker
// for an hour, whose step governedStep makes governed tool calls through one
// GovernedToolExecutor. Every component reads the fixture's clock.
type governedFixture struct {
	t         *testing.T
	ctx       context.Context
	store     KernelStore
	scope     Scope
	start     time.Time
	clock     time.Time
	run       *AgentRun
	step      *AgentTurn
	binding   *agentrun.TurnRunnerBinding
	executor  *GovernedToolExecutor
	actions   *runtimeaction.ActionCoordinator
	approvals *runtimeaction.ApprovalCoordinator
	worker    *runtimeaction.ActionWorker
	// policy maps a Skill action to its decision; allow by default.
	policy      map[string]runtimeaction.ActionPolicyDecision
	dispatch    func(agentrun.ActionDispatchInput) (map[string]interface{}, error)
	dispatches  int
	validations int
	validate    func(agentrun.ActionProposalValidationInput) error
	observed    int
}

// ObserveActionProposal counts the proposals the executor reports.
func (f *governedFixture) ObserveActionProposal(context.Context, *AgentRun, *AgentTurn, *ActionProposalResult) error {
	f.observed++
	return nil
}

func newGovernedFixture(t *testing.T, store KernelStore) *governedFixture {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "governed-" + uuid.NewString()[:8]}
	start := time.Now().UTC().Truncate(time.Millisecond)
	f := &governedFixture{
		t: t, ctx: context.Background(), store: store, scope: scope, start: start, clock: start.Add(time.Second),
		step: &AgentTurn{ID: governedStep, Scope: scope}, policy: map[string]runtimeaction.ActionPolicyDecision{},
		dispatch: func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"providerResult": "delivered"}, nil
		},
	}
	portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return start }))
	created, err := portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent",
		Goal: "ship the release", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.run = f.claim(governedWorker)
	if f.run.ID != created.ID {
		t.Fatalf("claimed %s, want %s", f.run.ID, created.ID)
	}
	f.step.RunID = f.run.ID
	catalog := actiontest.LoopWorkerCatalog(t, scope)
	now := func() time.Time { return f.clock }
	coordinator := runtimeaction.NewActionCoordinator(store, store, catalog, runtimeaction.ActionPolicyEvaluatorFunc(func(_ context.Context, input runtimeaction.ActionPolicyInput) (runtimeaction.ActionPolicyDecision, error) {
		if decision, ok := f.policy[input.Bound.Action.Name]; ok {
			return decision, nil
		}
		return runtimeaction.ActionPolicyDecision{Disposition: runtimeaction.ActionDispositionAllow}, nil
	}), []runtimeaction.ActionProposalValidator{actiontest.ProposalValidatorFunc(func(_ context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
		f.validations++
		if f.validate != nil {
			return nil, f.validate(input)
		}
		return nil, nil
	})}, models.WithClock(now))
	f.actions = coordinator
	f.approvals = runtimeaction.NewApprovalCoordinator(store, store, runtimeaction.EligibleApprovalAuthorizer{}, models.WithClock(now))
	f.worker = runtimeaction.NewActionWorker(store, catalog,
		runtimeaction.CredentialResolverFunc(func(context.Context, runtimeaction.CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "loop-secret"}, nil
		}), runtimeaction.ActionDispatcherFunc(func(_ context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			f.dispatches++
			return f.dispatch(input)
		}), models.WithClock(now))
	f.executor, err = NewGovernedToolExecutor(GovernedToolExecutorConfig{
		Actions: coordinator, Approvals: f.approvals, Worker: f.worker, Store: store, Now: now,
		Observer: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.binding = &agentrun.TurnRunnerBinding{DeploymentID: "release-agent", ModelActions: []capability.ModelAction{
		governedModelAction(governedDeploy, "deploy", capability.SideEffectExternal, capability.RiskLevelProduction),
		governedModelAction(governedStatus, "status", capability.SideEffectRead, capability.RiskLevelRead),
	}}
	return f
}

func governedModelAction(name, action string, effect capability.SideEffect, risk capability.RiskLevel) capability.ModelAction {
	return capability.ModelAction{
		Name: name, BindingID: "release-binding", BindingRevision: 1, SkillID: "release", Version: "1.0.0", Action: action,
		SideEffect: effect, Risk: risk,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"environment": map[string]interface{}{"type": "string"}}, "required": []interface{}{"environment"},
		},
	}
}

func (f *governedFixture) claim(worker string) *AgentRun {
	f.t.Helper()
	run, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{Scope: f.scope, WorkerID: worker, Now: f.clock, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil {
		f.t.Fatalf("claim for %s = %#v, %v", worker, run, err)
	}
	return run
}

func (f *governedFixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func (f *governedFixture) requireApprovalFor(action string) {
	f.policy[action] = runtimeaction.ActionPolicyDecision{
		Disposition: runtimeaction.ActionDispositionRequireApproval, Reason: "deploys need review",
		EligibleApprovers: []ApprovalPrincipal{governedApprover}, ApprovalTTL: time.Hour,
	}
}

// deployArguments is the reviewed envelope of a deploy call.
func deployArguments(environment string) json.RawMessage {
	return json.RawMessage(`{"arguments":{"environment":"` + environment + `"},"reviewContext":{"summary":"Deploy the release to ` + environment + `","content":"release 1.4.2"}}`)
}

func statusArguments() json.RawMessage { return json.RawMessage(`{"environment":"production"}`) }

func (f *governedFixture) call(toolCallID, name string, arguments json.RawMessage) GovernedToolCall {
	return GovernedToolCall{
		Run: f.currentRun(), Step: f.step, WorkerID: f.currentRun().LeaseOwner, Binding: f.binding,
		ToolCallID: toolCallID, Name: name, Arguments: arguments, Narration: "Checking the release.",
	}
}

func (f *governedFixture) execute(call GovernedToolCall) GovernedToolOutcome {
	f.t.Helper()
	outcome, err := f.executor.Execute(f.ctx, call)
	if err != nil {
		f.t.Fatalf("execute %s: %v", call.ToolCallID, err)
	}
	return outcome
}

func (f *governedFixture) resume(call GovernedToolCall, entry *SessionEntry) GovernedToolOutcome {
	f.t.Helper()
	outcome, err := f.executor.Resume(f.ctx, call, entry, nil)
	if err != nil {
		f.t.Fatalf("resume %s: %v", call.ToolCallID, err)
	}
	return outcome
}

func (f *governedFixture) currentRun() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("get Run = %#v, %v", run, err)
	}
	return run
}

func (f *governedFixture) actionCall(id string) *ActionCall {
	f.t.Helper()
	call, err := f.store.GetActionCall(f.ctx, f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return call
}

func (f *governedFixture) approval(id string) *ApprovalCheckpoint {
	f.t.Helper()
	approval, err := f.store.GetApproval(f.ctx, f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return approval
}

// updateRun writes mutate's change to the Run outside its lease.
func (f *governedFixture) updateRun(mutate func(*AgentRun)) {
	f.t.Helper()
	current := f.currentRun()
	updated := cloneAgentRun(current)
	mutate(updated)
	updated.Revision++
	updated.UpdatedAt = f.clock
	event := &ActivityEvent{
		ID: "run-update-" + uuid.NewString()[:8], Scope: f.scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
		AgentID: current.AssignedAgentID, RunID: current.ID, Actor: ActivityActor{Type: "worker", ID: governedWorker},
		Summary: "Run updated", Visibility: ActivityVisibilityScope, CreatedAt: f.clock,
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, event, nil); err != nil {
		f.t.Fatalf("update Run: %v", err)
	}
}

// parkOn parks the Run on park as the loop's step-end park does.
func (f *governedFixture) parkOn(park *GovernedPark) {
	f.t.Helper()
	f.updateRun(func(run *AgentRun) {
		run.Status, run.WakeCondition = park.Status, park.Wake
		run.LeaseOwner, run.LeaseExpiresAt = "", nil
	})
}

func (f *governedFixture) decide(approvalID string, decision ApprovalDecision, reason string) *ApprovalResolutionResult {
	f.t.Helper()
	approval := f.approval(approvalID)
	result, err := f.approvals.Resolve(f.ctx, runtimeaction.ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: governedApprover, Reason: reason,
	})
	if err != nil {
		f.t.Fatalf("decide %s %s: %v", approvalID, decision, err)
	}
	return result
}

func governedEntry(toolCallID, actionCallID string, state ToolEntryState) *SessionEntry {
	return &SessionEntry{Type: SessionEntryTool, StepID: governedStep, ToolCallID: toolCallID, ActionCallID: actionCallID, State: state, Tool: &ToolEntry{}}
}

func governedValue(t *testing.T, result *ToolResult) map[string]interface{} {
	t.Helper()
	if result == nil || len(result.Content) == 0 || result.Content[0].Type != ContentPartJSON {
		t.Fatalf("result = %#v, want a json part", result)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func requireGovernedState(t *testing.T, outcome GovernedToolOutcome, state ToolEntryState, status ToolResultStatus) map[string]interface{} {
	t.Helper()
	if outcome.State != state || outcome.Result == nil || outcome.Result.Status != status {
		t.Fatalf("outcome = %#v (result %#v), want %s/%s", outcome, outcome.Result, state, status)
	}
	return governedValue(t, outcome.Result)
}

func TestGovernedExecuteAllowedReadRunsInlineWithoutPool(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		before := f.currentRun()
		outcome := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		value := requireGovernedState(t, outcome, ToolEntryCompleted, ToolResultSucceeded)
		call := f.actionCall(outcome.ActionCallID)
		if call.Status != ActionCallStatusSucceeded || call.Origin != ActionOriginLoop || call.Execution != ActionExecutionLoop ||
			call.TurnID != governedStep || call.IdempotencyKey != "loop:"+governedStep+":tool-1" || call.LeaseOwner != "" {
			t.Fatalf("call = %#v", call)
		}
		if value["evidenceRef"] != "action-call:"+call.ID || outcome.Output["providerResult"] != "delivered" || f.dispatches != 1 {
			t.Fatalf("value = %#v, output = %#v, dispatches = %d", value, outcome.Output, f.dispatches)
		}
		after := f.currentRun()
		if after.Status != AgentRunStatusRunning || after.LeaseOwner != governedWorker || outcome.ToolCallID != "tool-1" {
			t.Fatalf("Run after = %s/%s, outcome = %#v", after.Status, after.LeaseOwner, outcome)
		}
		if (outcome.Run == nil) != (after.Revision == before.Revision) {
			t.Fatalf("outcome.Run = %#v with revisions %d -> %d", outcome.Run, before.Revision, after.Revision)
		}
	})
}

func TestGovernedExecuteTeamMemberUsesActionDeploymentID(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		// A Team member Run: its own identity, Team actions resolved against
		// the action deployment.
		f.binding.DeploymentID = "member-agent"
		f.binding.ActionDeploymentID = "release-agent"
		outcome := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		if call := f.actionCall(outcome.ActionCallID); call.DeploymentID != "release-agent" {
			t.Fatalf("deployment = %q, want the action deployment", call.DeploymentID)
		}
		// The selected action's own owner wins over the binding's.
		f.binding.ActionDeploymentID = "another-team"
		f.binding.ModelActions[1].DeploymentID = "release-agent"
		outcome = f.execute(f.call("tool-2", governedStatus, statusArguments()))
		if call := f.actionCall(outcome.ActionCallID); call.DeploymentID != "release-agent" || outcome.State != ToolEntryCompleted {
			t.Fatalf("call = %#v, outcome = %#v", call, outcome)
		}
	})
}

func TestGovernedExecuteUnknownToolRefused(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		if f.executor.IsGoverned(f.binding, "release_rollback") || !f.executor.IsGoverned(f.binding, governedDeploy) {
			t.Fatal("IsGoverned does not follow the binding's actions")
		}
		outcome := f.execute(f.call("tool-1", "release_rollback", statusArguments()))
		value := requireGovernedState(t, outcome, ToolEntryError, ToolResultRefused)
		if value["code"] != runtimeaction.ProposalRejectionUnknownTool || outcome.ActionCallID != "" {
			t.Fatalf("value = %#v, outcome = %#v", value, outcome)
		}
		// Bad arguments are a refusal too, and no row is written.
		outcome = f.execute(f.call("tool-2", governedDeploy, json.RawMessage(`{"environment":"production"}`)))
		if value := requireGovernedState(t, outcome, ToolEntryError, ToolResultRefused); value["code"] != runtimeaction.ProposalRejectionToolArgumentsInvalid {
			t.Fatalf("value = %#v", value)
		}
		calls, err := store.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: f.run.ID})
		if err != nil || len(calls) != 0 || f.dispatches != 0 {
			t.Fatalf("calls = %d, %v, dispatches = %d", len(calls), err, f.dispatches)
		}
	})
}

func TestGovernedExecuteApprovalAwaitsAndParksAtStepEnd(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		before := f.currentRun()
		outcome := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		if outcome.State != ToolEntryAwaitingApproval || outcome.Wait == nil || outcome.Wait.ApprovalID == "" || outcome.Result != nil || f.dispatches != 0 {
			t.Fatalf("outcome = %#v, dispatches = %d", outcome, f.dispatches)
		}
		approval := f.approval(outcome.Wait.ApprovalID)
		if approval.Status != ApprovalStatusPending || approval.Summary != "Deploy the release to production" {
			t.Fatalf("approval = %#v", approval)
		}
		after := f.currentRun()
		if after.Status != AgentRunStatusRunning || after.Revision != before.Revision || outcome.Run != nil {
			t.Fatalf("proposal changed the Run: %s rev %d -> %d", after.Status, before.Revision, after.Revision)
		}
		plan, err := f.executor.PlanPark(f.ctx, after, []GovernedToolOutcome{outcome})
		if err != nil || plan.Park == nil || plan.Park.Status != AgentRunStatusWaitingForApproval ||
			plan.Park.Wake.Type != "approval" || plan.Park.Wake.Reference != approval.ID {
			t.Fatalf("plan = %#v, %v", plan, err)
		}
		if plan.Park.Summary != "Waiting for approval: Deploy the release to production" {
			t.Fatalf("park = %#v", plan.Park)
		}
		// A crash before the park replays the same approval: no second card.
		replay := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		if replay.ActionCallID != outcome.ActionCallID || replay.Wait.ApprovalID != approval.ID || f.observed != 1 {
			t.Fatalf("replay = %#v, observed = %d", replay, f.observed)
		}
	})
}

func TestGovernedResumeApprovedExecutesOnResumingReplica(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		outcome := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		f.parkOn(outcome.Park)
		f.advance(time.Second)
		f.decide(outcome.Wait.ApprovalID, ApprovalDecisionApprove, "")
		f.advance(time.Second)
		f.run = f.claim(governedWorker2)
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", outcome.ActionCallID, ToolEntryAwaitingApproval))
		requireGovernedState(t, resumed, ToolEntryCompleted, ToolResultSucceeded)
		call := f.actionCall(outcome.ActionCallID)
		if call.Status != ActionCallStatusSucceeded || f.dispatches != 1 {
			t.Fatalf("call = %s, dispatches = %d", call.Status, f.dispatches)
		}
		if f.currentRun().LeaseOwner != governedWorker2 {
			t.Fatal("the resuming replica lost the Run")
		}
	})
}

func TestGovernedResumeEachDecisionOutcome(t *testing.T) {
	cases := []struct {
		name   string
		decide func(f *governedFixture, approvalID string)
		state  ToolEntryState
		status ToolResultStatus
		check  func(t *testing.T, value map[string]interface{})
	}{
		{name: "reject", decide: func(f *governedFixture, id string) { f.decide(id, ApprovalDecisionReject, "not today") },
			state: ToolEntryError, status: ToolResultDenied,
			check: func(t *testing.T, value map[string]interface{}) {
				if value["status"] != "rejected" || value["reason"] != "not today" {
					t.Fatalf("value = %#v", value)
				}
			}},
		{name: "changes_requested", decide: func(f *governedFixture, id string) {
			f.decide(id, ApprovalDecisionRequestChanges, "use staging first")
		}, state: ToolEntryError, status: ToolResultChangesRequested,
			check: func(t *testing.T, value map[string]interface{}) {
				if value["guidance"] != "use staging first" {
					t.Fatalf("value = %#v", value)
				}
			}},
		{name: "superseded", decide: func(f *governedFixture, id string) {
			approval := f.approval(id)
			if _, err := f.approvals.ResolveSupersededByMessage(f.ctx, f.scope, id, approval.Revision, "message-2"); err != nil {
				f.t.Fatal(err)
			}
		}, state: ToolEntryError, status: ToolResultSuperseded,
			check: func(t *testing.T, value map[string]interface{}) {
				if value["reason"] != runtimeaction.ApprovalSupersededReason {
					t.Fatalf("value = %#v", value)
				}
			}},
		{name: "expired", decide: func(f *governedFixture, id string) {
			approval := f.approval(id)
			f.clock = approval.ExpiresAt.Add(time.Second)
			if _, err := f.approvals.ResolveTimeout(f.ctx, f.scope, id, approval.Revision, id); err != nil {
				f.t.Fatal(err)
			}
		}, state: ToolEntryError, status: ToolResultExpired,
			check: func(t *testing.T, value map[string]interface{}) {
				if value["reason"] != governedExpiredReason {
					t.Fatalf("value = %#v", value)
				}
			}},
		{name: "still_pending", decide: func(*governedFixture, string) {}, state: ToolEntryAwaitingApproval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
				f := newGovernedFixture(t, store)
				f.requireApprovalFor("deploy")
				outcome := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
				f.parkOn(outcome.Park)
				f.advance(time.Second)
				tc.decide(f, outcome.Wait.ApprovalID)
				if tc.state == ToolEntryAwaitingApproval {
					// Another wake (here: a manual requeue) resumes the Run
					// while this approval is still pending.
					f.updateRun(func(run *AgentRun) {
						run.Status, run.WakeCondition = AgentRunStatusQueued, nil
						run.AvailableAt, run.QueueEnteredAt = f.clock, f.clock
					})
				}
				f.advance(time.Second)
				f.run = f.claim(governedWorker2)
				resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", outcome.ActionCallID, ToolEntryAwaitingApproval))
				if f.dispatches != 0 {
					t.Fatalf("dispatches = %d", f.dispatches)
				}
				if tc.state == ToolEntryAwaitingApproval {
					if resumed.State != ToolEntryAwaitingApproval || resumed.Wait.ApprovalID != outcome.Wait.ApprovalID || resumed.Park == nil {
						t.Fatalf("resumed = %#v", resumed)
					}
					approvals, err := store.ListApprovals(f.ctx, ApprovalFilter{Scope: f.scope, RunID: f.run.ID})
					if err != nil || len(approvals) != 1 {
						t.Fatalf("approvals = %d, %v; want no second card", len(approvals), err)
					}
					return
				}
				tc.check(t, requireGovernedState(t, resumed, tc.state, tc.status))
			})
		})
	}
}

func TestGovernedResumeSupersedesSiblingApprovals(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		first := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		second := f.execute(f.call("tool-2", governedDeploy, deployArguments("staging")))
		plan, err := f.executor.PlanPark(f.ctx, f.currentRun(), []GovernedToolOutcome{first, second})
		if err != nil || plan.Park.Wake.Reference != first.Wait.ApprovalID {
			t.Fatalf("plan = %#v, %v; want the first approval", plan, err)
		}
		f.parkOn(plan.Park)
		f.advance(time.Second)
		// A new message arrives while the Run waits on the first approval.
		f.updateRun(func(run *AgentRun) {
			run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{
				ID: "message-2", Instruction: "actually wait", CreatedAt: f.clock,
				ConversationMessage: &ConversationMessageDelivery{MessageID: "message-2"},
			})
		})
		approval := f.approval(first.Wait.ApprovalID)
		if _, err := f.approvals.ResolveSupersededByMessage(f.ctx, f.scope, approval.ID, approval.Revision, "message-2"); err != nil {
			t.Fatal(err)
		}
		f.advance(time.Second)
		f.run = f.claim(governedWorker2)
		resumedFirst := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", first.ActionCallID, ToolEntryAwaitingApproval))
		requireGovernedState(t, resumedFirst, ToolEntryError, ToolResultSuperseded)
		resumedSecond := f.resume(f.call("tool-2", governedDeploy, deployArguments("staging")), governedEntry("tool-2", second.ActionCallID, ToolEntryAwaitingApproval))
		requireGovernedState(t, resumedSecond, ToolEntryError, ToolResultSuperseded)
		sibling := f.approval(second.Wait.ApprovalID)
		if sibling.Status != ApprovalStatusSuperseded || sibling.SupersededByMessageID != "message-2" {
			t.Fatalf("sibling = %#v", sibling)
		}
		if run := f.currentRun(); run.Status != AgentRunStatusRunning || run.LeaseOwner != governedWorker2 || f.dispatches != 0 {
			t.Fatalf("Run = %s/%s, dispatches = %d", run.Status, run.LeaseOwner, f.dispatches)
		}
	})
}

func TestGovernedResumeEntryWithoutRowProposesAndRuns(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		// The entry was appended (and may have gone running) but the crash
		// came before Propose: nothing was dispatched, so the write runs.
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", "", ToolEntryRunning))
		requireGovernedState(t, resumed, ToolEntryCompleted, ToolResultSucceeded)
		if f.dispatches != 1 || f.actionCall(resumed.ActionCallID).IdempotencyKey != "loop:"+governedStep+":tool-1" {
			t.Fatalf("dispatches = %d", f.dispatches)
		}
	})
}

// runningCall proposes a ready loop call of action and claims it for worker
// under a one-minute call lease, as an inline execution that then crashed.
func (f *governedFixture) runningCall(id, action, worker string) *ActionCall {
	f.t.Helper()
	proposed := actiontest.ProposeLoopAction(f.t, f.store, actiontest.LoopWorkerCatalog(f.t, f.scope), f.currentRun(), governedWorker, f.clock, actiontest.LoopActionSpec{
		ID: id, SkillID: "release", Version: "1.0.0", Action: action, Arguments: map[string]interface{}{"environment": "production"},
	})
	claimed, err := f.store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: worker, Now: f.clock, LeaseDuration: time.Minute, OwnActionID: proposed.Call.ID})
	if err != nil || claimed == nil || claimed.Status != ActionCallStatusRunning {
		f.t.Fatalf("claim %s = %#v, %v", id, claimed, err)
	}
	return claimed
}

func TestGovernedResumeRunningWriteBecomesInterrupted(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		running := f.runningCall("deploy-1", "deploy", governedWorker)
		f.advance(2 * time.Minute)
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", running.ID, ToolEntryRunning))
		value := requireGovernedState(t, resumed, ToolEntryInterrupted, ToolResultInterrupted)
		if value["error"] != governedInterruptedError || f.dispatches != 0 {
			t.Fatalf("value = %#v, dispatches = %d", value, f.dispatches)
		}
		if call := f.actionCall(running.ID); call.Status != ActionCallStatusFailed || !call.RecoveredRunning {
			t.Fatalf("call = %#v", call)
		}
	})
}

func TestGovernedResumeRunningReadReruns(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		running := f.runningCall("status-1", "status", governedWorker)
		f.advance(2 * time.Minute)
		resumed := f.resume(f.call("tool-1", governedStatus, statusArguments()), governedEntry("tool-1", running.ID, ToolEntryRunning))
		requireGovernedState(t, resumed, ToolEntryCompleted, ToolResultSucceeded)
		if f.dispatches != 1 || resumed.ActionCallID != running.ID {
			t.Fatalf("dispatches = %d, outcome = %#v", f.dispatches, resumed)
		}
	})
}

func TestGovernedResumeLiveForeignLeaseRetriesLater(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		running := f.runningCall("deploy-1", "deploy", "replica-gone")
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", running.ID, ToolEntryRunning))
		if resumed.State != ToolEntryRunning || resumed.Result != nil || resumed.RetryAt == nil || !resumed.RetryAt.Equal(running.LeaseExpiresAt.UTC()) || f.dispatches != 0 {
			t.Fatalf("resumed = %#v, dispatches = %d", resumed, f.dispatches)
		}
	})
}

// A tool entry of an earlier step left running without its ActionCallID (the
// RetryAt path) resumes in a later step under the key it was proposed with:
// the existing call is found, nothing is proposed or dispatched again.
func TestGovernedResumeEarlierStepEntryKeepsItsKey(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		outcome := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		f.parkOn(outcome.Park)
		f.advance(time.Second)
		f.decide(outcome.Wait.ApprovalID, ApprovalDecisionApprove, "")
		running, err := f.store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: "replica-gone", Now: f.clock, LeaseDuration: time.Minute, OwnActionID: outcome.ActionCallID})
		if err != nil || running == nil || running.Status != ActionCallStatusRunning {
			t.Fatalf("claim = %#v, %v", running, err)
		}
		f.advance(time.Second)
		f.run = f.claim(governedWorker2)
		f.step = &AgentTurn{ID: "step-2", Scope: f.scope, RunID: f.run.ID}
		entry := governedEntry("tool-1", "", ToolEntryRunning)
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), entry)
		if resumed.State != ToolEntryRunning || resumed.Result != nil || resumed.ActionCallID != outcome.ActionCallID ||
			resumed.RetryAt == nil || !resumed.RetryAt.Equal(running.LeaseExpiresAt.UTC()) || f.dispatches != 0 {
			t.Fatalf("resumed = %#v, dispatches = %d", resumed, f.dispatches)
		}
		calls, err := f.store.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: f.run.ID, Limit: 10})
		if err != nil || len(calls) != 1 {
			t.Fatalf("calls = %d, %v", len(calls), err)
		}
		entry.ToolCallID = "tool-2"
		if _, err := f.executor.Resume(f.ctx, f.call("tool-1", governedDeploy, deployArguments("production")), entry, nil); err == nil {
			t.Fatal("resume accepted an entry of another tool call")
		}
	})
}

func TestGovernedResumeReplayDoesNotRevalidate(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		outcome := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		if f.validations != 1 {
			t.Fatalf("validations = %d", f.validations)
		}
		// The observation the call relied on changed since: a fresh proposal
		// would now be rejected.
		f.validate = func(agentrun.ActionProposalValidationInput) error { return errors.New("the observation is stale") }
		replay := f.execute(f.call("tool-1", governedDeploy, deployArguments("production")))
		resumed := f.resume(f.call("tool-1", governedDeploy, deployArguments("production")), governedEntry("tool-1", "", ToolEntryAwaitingApproval))
		for _, got := range []GovernedToolOutcome{replay, resumed} {
			if got.ActionCallID != outcome.ActionCallID || got.State != ToolEntryAwaitingApproval {
				t.Fatalf("replay = %#v, want the existing call", got)
			}
		}
		if f.validations != 1 {
			t.Fatalf("validations = %d, want the replay unvalidated", f.validations)
		}
		fresh := f.execute(f.call("tool-2", governedDeploy, deployArguments("production")))
		if value := requireGovernedState(t, fresh, ToolEntryError, ToolResultRefused); value["code"] != runtimeaction.ProposalRejectionNotAdmitted {
			t.Fatalf("fresh = %#v", value)
		}
	})
}

func TestGovernedCardSettledWhileHeldRequeuesAfterPark(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.dispatch = func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"requiresHuman": true, "challenges": []interface{}{"captcha"}}, nil
		}
		outcome := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		if outcome.State != ToolEntryAwaitingEvent || outcome.Park == nil || outcome.Park.Wake.Type != governedHumanInterventionWakeType || outcome.Run == nil {
			t.Fatalf("outcome = %#v", outcome)
		}
		requestID := outcome.Park.Wake.Reference
		if outcome.Wait.EventWaitID != requestID {
			t.Fatalf("wait = %#v", outcome.Wait)
		}
		// The person solves it while the step still holds the Run.
		f.updateRun(func(run *AgentRun) {
			for i := range run.HumanInterventions {
				if run.HumanInterventions[i].ID == requestID {
					resolved := f.clock
					run.HumanInterventions[i].Status = HumanInterventionStatusResolved
					run.HumanInterventions[i].Resolution = "Solved the captcha."
					run.HumanInterventions[i].ResolvedAt = &resolved
				}
			}
		})
		entry := governedEntry("tool-1", outcome.ActionCallID, ToolEntryAwaitingEvent)
		entry.Tool.Wait = outcome.Wait
		plan, err := f.executor.ReplanPark(f.ctx, f.currentRun(), []*SessionEntry{entry})
		if err != nil || !plan.Settled || plan.Park != nil {
			t.Fatalf("replan = %#v, %v; want settled, no park", plan, err)
		}
		resumed := f.resume(f.call("tool-1", governedStatus, statusArguments()), entry)
		value := requireGovernedState(t, resumed, ToolEntryCompleted, ToolResultSucceeded)
		if value["outcome"] != "Solved the captcha." || f.dispatches != 1 {
			t.Fatalf("value = %#v, dispatches = %d", value, f.dispatches)
		}
	})
}

func TestGovernedObserverCalledOnceOnCreate(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		first := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		again := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		if f.observed != 1 || again.ActionCallID != first.ActionCallID || f.dispatches != 1 {
			t.Fatalf("observed = %d, dispatches = %d", f.observed, f.dispatches)
		}
		requireGovernedState(t, again, ToolEntryCompleted, ToolResultSucceeded)
	})
}

func TestGovernedBudgetExhaustedTakesBudgetExit(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.updateRun(func(run *AgentRun) {
			run.Budget = &BudgetPolicy{MaxActions: 1}
		})
		first := f.execute(f.call("tool-1", governedStatus, statusArguments()))
		requireGovernedState(t, first, ToolEntryCompleted, ToolResultSucceeded)
		second := f.execute(f.call("tool-2", governedStatus, statusArguments()))
		value := requireGovernedState(t, second, ToolEntryError, ToolResultDenied)
		if !second.BudgetExhausted || value["reason"] != governedBudgetExhaustedReason || second.ActionCallID == "" {
			t.Fatalf("second = %#v", second)
		}
		exit := GovernedBudgetExhaustion(second)
		if exit == nil || exit.ActionCallID != second.ActionCallID || strings.Join(exit.Dimensions, ",") != "actions" {
			t.Fatalf("exit = %#v", exit)
		}
		if GovernedBudgetExhaustion(first) != nil || f.dispatches != 1 {
			t.Fatalf("dispatches = %d", f.dispatches)
		}
		// A crash replay of the denied call still takes the budget exit.
		if replay := f.execute(f.call("tool-2", governedStatus, statusArguments())); !replay.BudgetExhausted {
			t.Fatalf("replay = %#v", replay)
		}
	})
}

func TestLoopIdempotencyKey(t *testing.T) {
	if got := loopIdempotencyKey("step-3", "call_9", nil); got != "loop:step-3:call_9" {
		t.Fatalf("model key = %q", got)
	}
	if got := loopIdempotencyKey("step-3", "call_9", &agentrun.ToolCallDirective{IdempotencyKey: " runbook:op-1 "}); got != "runbook:op-1" {
		t.Fatalf("directive key = %q", got)
	}
}

// approvalReadHookStore runs onRead after it reads an approval, so a test
// can decide it between the executor's inspection and its outcome.
type approvalReadHookStore struct {
	KernelStore
	onRead func(*ApprovalCheckpoint)
}

func (s approvalReadHookStore) GetApproval(ctx context.Context, scope Scope, id string) (*ApprovalCheckpoint, error) {
	approval, err := s.KernelStore.GetApproval(ctx, scope, id)
	if err == nil && s.onRead != nil {
		s.onRead(approval)
	}
	return approval, err
}

func TestGovernedHeldDecisionAfterSettleFailsStepEndPark(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		decided := false
		hooked := approvalReadHookStore{KernelStore: store, onRead: func(approval *ApprovalCheckpoint) {
			if decided || approval == nil || approval.Status != ApprovalStatusPending {
				return
			}
			decided = true
			// The person approves while the step still holds the Run.
			f.decide(approval.ID, ApprovalDecisionApprove, "")
		}}
		executor, err := NewGovernedToolExecutor(GovernedToolExecutorConfig{
			Actions: f.actions, Approvals: f.approvals, Worker: f.worker, Store: hooked, Now: func() time.Time { return f.clock },
		})
		if err != nil {
			t.Fatal(err)
		}
		call := f.call("tool-1", governedDeploy, deployArguments("production"))
		outcome, err := executor.Execute(f.ctx, call)
		if err != nil || !decided || outcome.State != ToolEntryAwaitingApproval {
			t.Fatalf("outcome = %#v, decided = %v, %v", outcome, decided, err)
		}
		known := call.Run
		if outcome.Run != nil {
			known = outcome.Run
		}
		current := f.currentRun()
		if known.Revision >= current.Revision {
			t.Fatalf("outcome Run revision %d hides the held decision (Run at %d)", known.Revision, current.Revision)
		}
		plan, err := executor.PlanPark(f.ctx, known, []GovernedToolOutcome{outcome})
		if err != nil || plan.Park == nil {
			t.Fatalf("plan = %#v, %v", plan, err)
		}
		parked := cloneAgentRun(known)
		parked.Status, parked.WakeCondition = plan.Park.Status, plan.Park.Wake
		parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
		parked.Revision++
		event := &ActivityEvent{
			ID: "park-" + uuid.NewString()[:8], Scope: f.scope, EventType: "run.parked", Severity: ActivitySeverityInfo,
			AgentID: known.AssignedAgentID, RunID: known.ID, Actor: ActivityActor{Type: "worker", ID: governedWorker},
			Summary: "Run parked", Visibility: ActivityVisibilityScope, CreatedAt: f.clock,
		}
		if _, err := store.UpdateAgentRunWithEvent(f.ctx, parked, known.Revision, event, nil); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale park = %v, want ErrRevisionConflict", err)
		}
	})
}
