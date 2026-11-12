package skillbinding_test

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestRepeatedRefusedSetupIsNotRetriedInTheSameRun(t *testing.T) {
	ctx, store, _, dispatcher, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
	run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "skill-live-browser", "skillVersion": "latest", "reason": "connect"}
	refused := &models.ActionCall{ID: "refused", Scope: scope, RunID: run.ID, DeploymentID: "agent", SkillID: models.SkillManagementSkillID, Action: agentrun.SkillActionRequestSetup,
		Status: models.ActionCallStatusSucceeded, Arguments: args, Output: skillbinding.InteractionRequestRefusalResult(skillbinding.RefuseInteraction("not available"))}
	storetest.SeedActionCall(t, store, refused)
	_, err := dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "retry"}, Arguments: args}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
	var refusal *skillbinding.InteractionRequestRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("repeated refused setup: %v", err)
	}
}
