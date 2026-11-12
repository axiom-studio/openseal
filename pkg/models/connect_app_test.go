package models

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/mcp"
)

func TestConnectAppRequestValidation(t *testing.T) {
	now := time.Now().UTC()
	base := func() *SkillSetupRequest {
		return &SkillSetupRequest{ID: "connect-app:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message",
			RunID: "run", ActionCallID: "call", Kind: SkillSetupKindConnectApp, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion, SkillName: "Linear",
			App: &ConnectAppTarget{CatalogSlug: "linear", Name: "Linear"}, Reason: "Connect Linear", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SkillSetupRequest){
		"no app":          func(r *SkillSetupRequest) { r.App = nil },
		"slug and addr":   func(r *SkillSetupRequest) { r.App.Address = "https://mcp.linear.app/mcp" },
		"bad address":     func(r *SkillSetupRequest) { r.App = &ConnectAppTarget{Address: "http://x", Name: "x"} },
		"binding":         func(r *SkillSetupRequest) { r.BindingID, r.BindingRevision = "mcp:c1", 1 },
		"app on install":  func(r *SkillSetupRequest) { r.Kind = "install" },
		"bad logo":        func(r *SkillSetupRequest) { r.App.LogoSHA256 = "nothex" },
		"unknown sign-in": func(r *SkillSetupRequest) { r.App.SignIn = "magic" },
	} {
		r := base()
		change(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
