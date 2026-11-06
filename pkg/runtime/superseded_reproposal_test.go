package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// revisionFixture drives one Run through governed deploy proposals that need
// approval, as in Run 2b4f1557: the person answered a pending publish with a
// chat message and every later Turn re-proposed the same arguments.
type revisionFixture struct {
	t         *testing.T
	store     *MemoryStore
	scope     Scope
	now       time.Time
	tick      time.Duration
	run       *AgentRun
	pool      *AgentRunWorkerPool
	approvals *ApprovalCoordinator
	binding   *TurnRunnerBinding
}

func newRevisionFixture(t *testing.T) *revisionFixture {
	t.Helper()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Now().UTC()
	f := &revisionFixture{t: t, store: store, scope: scope, now: now}
	f.run = claimedActionRun(t, store, scope, now, "worker")
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "release needs review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "alice"}}, ApprovalTTL: time.Hour,
		}, nil
	}))
	actions.now = func() time.Time { return f.now.Add(f.tick) }
	f.approvals = NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	f.approvals.now = func() time.Time { return f.now.Add(f.tick) }
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent")
	if err != nil {
		t.Fatal(err)
	}
	f.binding = &TurnRunnerBinding{DeploymentID: "release-agent", ActionDeploymentID: "release-agent", ModelActions: modelActions}
	f.pool, err = NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, fmt.Errorf("unused")
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "release-agent", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.pool.SetActionCoordinator(actions)
	return f
}

// propose materializes the same deploy with the same model key in turnID.
func (f *revisionFixture) propose(turnID string) *AgentRun {
	f.t.Helper()
	f.tick += time.Second
	run, err := f.pool.materializeTurnAction(f.t.Context(), "worker", f.run, &AgentTurn{
		ID: turnID, RequestedActions: []TurnAction{{
			Type: "skill_action", Capability: "release.deploy", Summary: "Deploy staging",
			IdempotencyKey: "deploy-staging", InputRef: "/actionInputs/call",
		}},
		ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{
			"call": map[string]interface{}{"environment": "staging"},
		}},
	}, f.binding)
	if err != nil {
		f.t.Fatalf("materialize %s: %v", turnID, err)
	}
	f.run = run
	return run
}

// reclaim claims the queued Run again for the next Turn.
func (f *revisionFixture) reclaim() {
	f.t.Helper()
	f.tick += time.Second
	claimed, err := f.store.ClaimNextAgentRun(f.t.Context(), AgentRunClaim{
		Scope: f.scope, WorkerID: "worker", Now: f.now.Add(f.tick), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != f.run.ID {
		f.t.Fatalf("reclaimed Run = %#v, %v", claimed, err)
	}
	f.run = claimed
}

func (f *revisionFixture) calls() []*ActionCall {
	f.t.Helper()
	calls, err := f.store.ListActionCalls(f.t.Context(), ActionFilter{Scope: f.scope, RunID: f.run.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	return calls
}

func (f *revisionFixture) call(key string) *ActionCall {
	f.t.Helper()
	for _, call := range f.calls() {
		if call.IdempotencyKey == key {
			return call
		}
	}
	f.t.Fatalf("no action call with key %q in %#v", key, f.calls())
	return nil
}

func (f *revisionFixture) pending(call *ActionCall) *ApprovalCheckpoint {
	f.t.Helper()
	approval, err := f.store.GetApproval(f.t.Context(), f.scope, call.ApprovalID)
	if err != nil || approval == nil || approval.Status != ApprovalStatusPending || call.Status != ActionCallStatusWaitingApproval {
		f.t.Fatalf("call %s is not waiting on a pending approval: %#v, %#v, %v", call.ID, call, approval, err)
	}
	if f.run.Status != AgentRunStatusWaitingForApproval || f.run.WakeCondition == nil || f.run.WakeCondition.Reference != approval.ID {
		f.t.Fatalf("Run does not wait on approval %s: %#v", approval.ID, f.run)
	}
	return approval
}

func (f *revisionFixture) supersede(approval *ApprovalCheckpoint, messageID string) {
	f.t.Helper()
	f.tick += time.Second
	if _, err := f.approvals.ResolveSupersededByMessage(f.t.Context(), f.scope, approval.ID, approval.Revision, messageID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *revisionFixture) events(eventType string) int {
	f.t.Helper()
	events, err := f.store.ListActivity(f.t.Context(), ActivityFilter{Scope: f.scope, RunID: f.run.ID, EventTypes: []string{eventType}})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(events)
}

// Run 2b4f1557: after a message superseded the pending publish, every later
// Turn re-proposed it and got the closed denial back without a new approval.
// Each re-proposal after a closure is a new request for approval; a reviewer
// rejection still ends the attempt.
func TestReproposalAfterClosedApprovalRequestsApprovalAgain(t *testing.T) {
	f := newRevisionFixture(t)
	f.propose("turn-1")
	first := f.pending(f.call("deploy-staging"))
	f.supersede(first, "message-1")

	f.reclaim()
	f.propose("turn-2")
	second := f.call("deploy-staging:after:" + first.ID)
	if second.TurnID != "turn-2" {
		t.Fatalf("revised call = %#v", second)
	}
	secondApproval := f.pending(second)
	if secondApproval.ID == first.ID || f.events("action.replayed") != 0 || f.events("action.approval_requested") != 2 {
		t.Fatalf("re-proposal did not request approval again: replayed=%d requested=%d", f.events("action.replayed"), f.events("action.approval_requested"))
	}

	// A reviewer's requested changes close the revision the same way.
	f.tick += time.Second
	if _, err := f.approvals.Resolve(t.Context(), ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: secondApproval.ID, ExpectedRevision: secondApproval.Revision, DecisionID: "changes",
		Decision: ApprovalDecisionRequestChanges, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "deploy it anyway",
	}); err != nil {
		t.Fatal(err)
	}
	f.reclaim()
	f.propose("turn-3")
	third := f.call("deploy-staging:after:" + secondApproval.ID)
	thirdApproval := f.pending(third)
	if len(f.calls()) != 3 || third.TurnID != "turn-3" {
		t.Fatalf("calls after two closures = %#v", f.calls())
	}

	// A rejection is a final answer: the same proposal is replayed, never
	// asked again, and the attempt ends with an explanation.
	f.tick += time.Second
	if _, err := f.approvals.Resolve(t.Context(), ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: thirdApproval.ID, ExpectedRevision: thirdApproval.Revision, DecisionID: "reject",
		Decision: ApprovalDecisionReject, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "no",
	}); err != nil {
		t.Fatal(err)
	}
	f.reclaim()
	resumed := f.propose("turn-4")
	last, _ := resumed.Checkpoint["lastAction"].(map[string]interface{})
	if len(f.calls()) != 3 || resumed.Status != AgentRunStatusQueued || fmt.Sprint(last["actionCallId"]) != third.ID ||
		fmt.Sprint(last["approvalStatus"]) != string(ApprovalStatusRejected) || last["idempotentReplay"] != true ||
		!requiresFinalFailureExplanation(resumed.Checkpoint) || f.events("action.replayed") != 1 {
		t.Fatalf("rejected proposal was not replayed as final: calls=%d run=%#v", len(f.calls()), resumed)
	}
}

// A worker retry of the Turn that made the revised proposal reuses that call
// instead of asking again, and keeps the message that closed it.
func TestRetriedRevisionTurnReplaysItsOwnClosedProposal(t *testing.T) {
	f := newRevisionFixture(t)
	f.propose("turn-1")
	first := f.pending(f.call("deploy-staging"))
	f.supersede(first, "message-1")
	f.reclaim()
	f.propose("turn-2")
	second := f.call("deploy-staging:after:" + first.ID)
	f.supersede(f.pending(second), "message-2")

	f.reclaim()
	resumed := f.propose("turn-2")
	last, _ := resumed.Checkpoint["lastAction"].(map[string]interface{})
	if len(f.calls()) != 2 || resumed.Status != AgentRunStatusQueued || fmt.Sprint(last["actionCallId"]) != second.ID ||
		fmt.Sprint(last["approvalStatus"]) != string(ApprovalStatusSuperseded) || last["supersededByMessageId"] != "message-2" ||
		last["idempotentReplay"] != true || requiresFinalFailureExplanation(resumed.Checkpoint) || f.events("action.replayed") != 1 {
		t.Fatalf("retried Turn did not replay its closed proposal: calls=%d run=%#v", len(f.calls()), resumed)
	}
}
