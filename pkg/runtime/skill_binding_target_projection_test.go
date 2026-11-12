package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestSkillBindingTargetSetupRequestIsOwnedByTheTarget(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "finn")
	result, err := f.dispatch(t, run, "setup", agentrun.SkillActionRequestSetup, map[string]interface{}{
		"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Read communities", SkillActionTargetAgentArgument: "researcher",
	})
	if err != nil || result["setupRequest"] == nil {
		t.Fatalf("targeted setup = %#v, %v", result, err)
	}
	request, err := f.store.GetSkillSetupRequest(f.ctx, f.scope, "skill-setup:setup")
	if err != nil || request == nil {
		t.Fatalf("saved request = %#v, %v", request, err)
	}
	builder := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
	if request.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}) || request.DeploymentID != "researcher" || request.AgentID != "finn" ||
		request.ConversationID != "chat-finn" || request.RunID != run.ID || request.ChatOwner() != builder || !request.Targeted() || request.Validate() != nil {
		t.Fatalf("targeted setup request = %#v", request)
	}
	if last := f.discovered[len(f.discovered)-1]; last.DeploymentID != "researcher" {
		t.Fatalf("setup resolved the Skill for %q", last.DeploymentID)
	}
	resumable, err := InteractionRunResumable(f.ctx, f.store, run, request.Owner, request.AgentID)
	if err != nil || !resumable {
		t.Fatalf("builder Run is not resumable by the target's request: %v, %v", resumable, err)
	}
	// The card is in the builder's chat and its list.
	projected, err := (&ConversationChangeService{skillSetups: f.store.ListSkillSetupRequests}).listConversationSkillSetupRequests(f.ctx,
		&Conversation{Scope: f.scope, ID: "chat-finn", Owner: builder}, nil, nil)
	if err != nil || len(projected) != 1 || projected[0].ID != request.ID {
		t.Fatalf("builder chat projection = %#v, %v", projected, err)
	}
	listed, err := f.dispatch(t, run, "list", SkillActionListSetupRequests, map[string]interface{}{})
	if err != nil || len(listed["requests"].([]interface{})) != 1 {
		t.Fatalf("builder list_setup_requests = %#v, %v", listed, err)
	}
}
