package runtime

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestRepeatedRefusedSetupIsNotRetriedInTheSameRun(t *testing.T) {
	ctx, store, _, dispatcher, scope := credentialRequestDispatchFixture(t)
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "skill-live-browser", "skillVersion": "latest", "reason": "connect"}
	refused := &ActionCall{ID: "refused", Scope: scope, RunID: run.ID, DeploymentID: "agent", SkillID: SkillManagementSkillID, Action: agentrun.SkillActionRequestSetup,
		Status: ActionCallStatusSucceeded, Arguments: args, Output: interactionRequestRefusalResult(refuseInteraction("not available"))}
	storetest.SeedActionCall(t, store, refused)
	_, err := dispatcher.requestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &ActionCall{ID: "retry"}, Arguments: args}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	var refusal *interactionRequestRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("repeated refused setup: %v", err)
	}
}
