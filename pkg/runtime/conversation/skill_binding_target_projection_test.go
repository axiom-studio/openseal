package conversation

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
)

func TestSkillBindingTargetSetupRequestIsOwnedByTheTarget(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "finn")
	result, err := f.Dispatch(t, run, "setup", agentrun.SkillActionRequestSetup, map[string]interface{}{
		"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Read communities", skillbinding.SkillActionTargetAgentArgument: "researcher",
	})
	if err != nil || result["setupRequest"] == nil {
		t.Fatalf("targeted setup = %#v, %v", result, err)
	}
	request, err := f.Store.GetSkillSetupRequest(f.Ctx, f.Scope, "skill-setup:setup")
	if err != nil || request == nil {
		t.Fatalf("saved request = %#v, %v", request, err)
	}
	builder := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}
	if request.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}) || request.DeploymentID != "researcher" || request.AgentID != "finn" ||
		request.ConversationID != "chat-finn" || request.RunID != run.ID || request.ChatOwner() != builder || !request.Targeted() || request.Validate() != nil {
		t.Fatalf("targeted setup request = %#v", request)
	}
	if last := f.Discovered[len(f.Discovered)-1]; last.DeploymentID != "researcher" {
		t.Fatalf("setup resolved the Skill for %q", last.DeploymentID)
	}
	resumable, err := models.InteractionRunResumable(f.Ctx, f.Store, run, request.Owner, request.AgentID)
	if err != nil || !resumable {
		t.Fatalf("builder Run is not resumable by the target's request: %v, %v", resumable, err)
	}
	// The card is in the builder's chat and its list.
	projected, err := (&ConversationChangeService{skillSetups: f.Store.ListSkillSetupRequests}).listConversationSkillSetupRequests(f.Ctx,
		&models.Conversation{Scope: f.Scope, ID: "chat-finn", Owner: builder}, nil, nil)
	if err != nil || len(projected) != 1 || projected[0].ID != request.ID {
		t.Fatalf("builder chat projection = %#v, %v", projected, err)
	}
	listed, err := f.Dispatch(t, run, "list", skillbinding.SkillActionListSetupRequests, map[string]interface{}{})
	if err != nil || len(listed["requests"].([]interface{})) != 1 {
		t.Fatalf("builder list_setup_requests = %#v, %v", listed, err)
	}
}
