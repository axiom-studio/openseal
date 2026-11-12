package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

// The create_task approval names the activation the approved call creates, so
// a decided approval can open the routine (a webhook routine's address).
func TestCreateTaskApprovalNamesItsActivation(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, catalog, bound, args := routinetest.ScheduledTask(t, store)
	now := time.Now().Add(time.Second)
	if claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || claimed == nil {
		t.Fatalf("claim=%#v,%v", claimed, err)
	}
	validator, _ := routine.NewRunbookActionValidator(store)
	coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, Reason: "Review recurring work", EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour}, nil
	}), []action.ActionProposalValidator{validator}, models.WithClock(func() time.Time { return now }))
	result, err := coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: models.RunbookActionCreateTask, Arguments: args, IdempotencyKey: " routine-key ", Summary: "Create recurring research", Actor: models.ActivityActor{Type: "agent", ID: run.AssignedAgentID}}, "create-task"))
	if err != nil || result.Approval == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	named, _ := result.Approval.ProposedAction["activationId"].(string)
	if named == "" || named != routine.RunbookActivationIDForKey(run.Scope, "routine-key") {
		t.Fatalf("approval activationId = %q", named)
	}
	created := dispatchTaskForTest(t, store, run, bound, args, result.Call.IdempotencyKey)
	activation, _ := created["activation"].(*models.RunbookActivation)
	if activation == nil || activation.ID != named {
		t.Fatalf("approval named %q, call created %#v", named, created["activation"])
	}
}
