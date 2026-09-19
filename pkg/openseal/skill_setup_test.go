package openseal

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"testing"
	"time"
)

func TestSkillSetupResolutionRequiresExactSavedBindingAndPersists(t *testing.T) {
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
	binding := &SkillBinding{ID: "github", Scope: skillScope, DeploymentID: "agent", SkillID: "github", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead, Revision: 1}
	if err := e.BindSkill(ctx, binding); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := &SkillSetupRequest{ID: "request", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: "reauthorize", SkillID: "github", SkillVersion: "1.0.0", SkillName: "GitHub", BindingID: "github", BindingRevision: 1, Reason: "Read your repository", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, 1, "github", "user", false); err == nil {
		t.Fatal("unchanged connection resolved request")
	}
	if _, err := e.ResolveSkillSetupRequest(ctx, Scope{Kind: "tenant", ID: "other"}, "agent", request.ID, 1, "github", "user", false); err == nil {
		t.Fatal("cross-tenant resolution")
	}
	if _, err := e.ResolveSkillSetupRequest(ctx, scope, "other-agent", request.ID, 1, "github", "user", false); err == nil {
		t.Fatal("cross-agent resolution")
	}
	if _, err := e.UpsertSkillBinding(ctx, UpsertSkillBindingRequest{Binding: binding, ExpectedRevision: 1, Actor: SkillBindingActor{Type: "user", ID: "user"}, Reason: "Reconnect requested account"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := e.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, 1, "github", "user", false)
	if err != nil || resolved.Status != "resolved" || resolved.ResolvedBindingRevision != 2 {
		t.Fatalf("resolution: %#v %v", resolved, err)
	}
	replay, err := e.ResolveSkillSetupRequest(ctx, scope, "agent", request.ID, 1, "github", "user", false)
	if err != nil || replay.Revision != 2 {
		t.Fatalf("resolution not idempotent: %#v %v", replay, err)
	}
	if _, _, err := e.CreateConversation(ctx, CreateConversationRequest{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, Title: "Setup", IdempotencyKey: "setup-chat"}); err != nil {
		t.Fatal(err)
	}
	pending := *request
	pending.ID = "request-settings"
	pending.BindingRevision = 2
	pending.CreatedAt = time.Now().UTC()
	pending.UpdatedAt = pending.CreatedAt
	if err := store.SaveSkillSetupRequest(ctx, &pending, 0); err != nil {
		t.Fatal(err)
	}
	current, _ := e.GetSkillBinding(ctx, skillScope, "agent", "github")
	if _, err := e.UpsertSkillBinding(ctx, UpsertSkillBindingRequest{Binding: current, ExpectedRevision: 2, Actor: SkillBindingActor{Type: "user", ID: "user"}, Reason: "Updated in Settings"}); err != nil {
		t.Fatal(err)
	}
	requests, err := e.ListSkillSetupRequests(ctx, scope, "agent", "chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range requests {
		if r.Status != "resolved" {
			t.Fatalf("Settings update stranded request: %#v", r)
		}
	}

}
