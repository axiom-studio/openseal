package openseal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// A builder's setup card for another Agent is shown in the builder's chat, so
// the chat addresses it by the builder's umbrella; resolving it checks and
// records the target's binding.
func TestTargetedSkillSetupResolvesFromTheBuildersChat(t *testing.T) {
	ctx := context.Background()
	store := runtime.NewMemoryStore()
	e, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	skillScope := SkillScope{Kind: scope.Kind, ID: scope.ID}
	definition := &SkillDefinition{ID: "github", Version: "1.0.0", Name: "GitHub", Transport: SkillTransportReference{Kind: "local"}, Actions: map[string]SkillAction{"read": {Name: "read", Description: "Read a repository", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}}}
	if err := e.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := &SkillSetupRequest{ID: "request", Scope: scope, Owner: agentOwner("target"), DeploymentID: "target", AgentID: "finn", ConversationID: "chat-finn", TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: "install", SkillID: "github", SkillVersion: "1.0.0", SkillName: "GitHub", RequiredActions: []string{"read"}, Reason: "Read the repository", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := request.Validate(); err != nil {
		t.Fatalf("targeted request invalid: %v", err)
	}
	if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	seedSpace(t, e, scope, "")
	if _, _, err := e.CreateConversation(ctx, CreateConversationRequest{ID: "chat-finn", Scope: scope, Owner: agentOwner("finn"), Title: "New agent", IdempotencyKey: "builder-chat"}); err != nil {
		t.Fatal(err)
	}
	listed, err := e.ListSkillSetupRequests(ctx, scope, agentOwner("finn"), "chat-finn")
	if err != nil || len(listed) != 1 || listed[0].ID != request.ID {
		t.Fatalf("builder chat does not list the card: %#v %v", listed, err)
	}
	for _, owner := range []ObjectiveOwner{agentOwner("finn"), agentOwner("target")} {
		if got, err := e.GetSkillSetupRequest(ctx, scope, owner, request.ID); err != nil || got.ID != request.ID {
			t.Fatalf("%s cannot open the card: %v", owner.ID, err)
		}
	}
	if _, err := e.GetSkillSetupRequest(ctx, scope, agentOwner("other"), request.ID); !errors.Is(err, runtime.ErrInvalidSkillSetup) {
		t.Fatalf("an unrelated umbrella opened the card: %v", err)
	}
	// A binding with the same id on the builder does not satisfy the card.
	finnBinding := &SkillBinding{ID: "github", Scope: skillScope, DeploymentID: "finn", SkillID: "github", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead, Revision: 1}
	if err := e.BindSkill(ctx, finnBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResolveSkillSetupRequest(ctx, scope, agentOwner("finn"), request.ID, 1, "github", InteractionResolver{UserID: "user", CanManage: true}, false); err == nil {
		t.Fatal("the builder's own binding resolved the target's card")
	}
	targetBinding := &SkillBinding{ID: "github", Scope: skillScope, DeploymentID: "target", SkillID: "github", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead, Revision: 1}
	if err := e.BindSkill(ctx, targetBinding); err != nil {
		t.Fatal(err)
	}
	resolved, err := e.ResolveSkillSetupRequest(ctx, scope, agentOwner("finn"), request.ID, 1, "github", InteractionResolver{UserID: "user", CanManage: true}, false)
	if err != nil || resolved.Status != "resolved" || resolved.ResolvedBindingID != "github" || resolved.Owner != agentOwner("target") {
		t.Fatalf("resolution from the builder's chat: %#v %v", resolved, err)
	}
	stored, _ := store.GetSkillSetupRequest(ctx, scope, request.ID)
	if stored.Status != "resolved" || stored.DeploymentID != "target" {
		t.Fatalf("stored request: %#v", stored)
	}
}
