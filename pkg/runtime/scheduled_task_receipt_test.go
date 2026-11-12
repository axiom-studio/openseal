package runtime

import (
	"context"
	"testing"
	"time"
)

// The create_task approval names the activation the approved call creates, so
// a decided approval can open the routine (a webhook routine's address).
func TestCreateTaskApprovalNamesItsActivation(t *testing.T) {
	store := newTestKernelStore(t)
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	now := time.Now().Add(time.Second)
	if claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || claimed == nil {
		t.Fatalf("claim=%#v,%v", claimed, err)
	}
	validator, _ := NewRunbookActionValidator(store)
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "Review recurring work", EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour}, nil
	}), []ActionProposalValidator{validator})
	coordinator.now = func() time.Time { return now }
	result, err := coordinator.Propose(t.Context(), loopProposeRequest(ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: RunbookActionCreateTask, Arguments: args, IdempotencyKey: " routine-key ", Summary: "Create recurring research", Actor: ActivityActor{Type: "agent", ID: run.AssignedAgentID}}, "create-task"))
	if err != nil || result.Approval == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	named, _ := result.Approval.ProposedAction["activationId"].(string)
	if named == "" || named != RunbookActivationIDForKey(run.Scope, "routine-key") {
		t.Fatalf("approval activationId = %q", named)
	}
	created := dispatchTaskForTest(t, store, run, bound, args, result.Call.IdempotencyKey)
	activation, _ := created["activation"].(*RunbookActivation)
	if activation == nil || activation.ID != named {
		t.Fatalf("approval named %q, call created %#v", named, created["activation"])
	}
}
