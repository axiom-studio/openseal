package models

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillSetupRequestConnectAccountValidation(t *testing.T) {
	now := time.Now().UTC()
	slot := ConnectAccountSlot{Name: "google", Requirement: skill.CredentialRequirement{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{}}}
	base := func() *SkillSetupRequest {
		return &SkillSetupRequest{ID: "connect-account:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, DeploymentID: "team", AgentID: "agent",
			ConversationID: "chat", TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: SkillSetupKindConnectAccount,
			SkillID: "acme.gmail", SkillVersion: "1.0.0", SkillName: "Gmail", BindingID: "binding", BindingRevision: 2,
			RequestedFor: "7", Mode: "per_person", Slots: []ConnectAccountSlot{slot},
			Reason: "Connect Google", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}
	shared := base()
	shared.Mode, shared.RequestedFor = "shared", ""
	if err := shared.Validate(); err != nil {
		t.Fatalf("shared request: %v", err)
	}
	for name, change := range map[string]func(*SkillSetupRequest){
		"no binding":            func(r *SkillSetupRequest) { r.BindingID = "" },
		"no slots":              func(r *SkillSetupRequest) { r.Slots = nil },
		"duplicate slot":        func(r *SkillSetupRequest) { r.Slots = append(r.Slots, slot) },
		"slot requirement name": func(r *SkillSetupRequest) { r.Slots[0].Requirement.Name = "other" },
		"slot requirement kind": func(r *SkillSetupRequest) { r.Slots[0].Requirement.Kind = "" },
		"no mode":               func(r *SkillSetupRequest) { r.Mode = "" },
		"unknown mode":          func(r *SkillSetupRequest) { r.Mode = "everyone" },
		"per_person for nobody": func(r *SkillSetupRequest) { r.RequestedFor = "" },
		"requested for invalid": func(r *SkillSetupRequest) { r.RequestedFor = "a b" },
		"app":                   func(r *SkillSetupRequest) { r.App = &ConnectAppTarget{CatalogSlug: "linear", Name: "Linear"} },
		"slots on configure":    func(r *SkillSetupRequest) { r.Kind, r.Mode = "configure", "" },
		"mode on configure":     func(r *SkillSetupRequest) { r.Kind, r.Slots = "configure", nil },
		"required actions":      func(r *SkillSetupRequest) { r.RequiredActions = []string{"send"} },
	} {
		r := base()
		r.Slots = append([]ConnectAccountSlot(nil), r.Slots...)
		change(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSkillSetupRequestConnectAppMode(t *testing.T) {
	now := time.Now().UTC()
	for mode, valid := range map[string]bool{"": true, "per_person": true, "shared": true, "team": false} {
		r := &SkillSetupRequest{ID: "connect-app:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message",
			RunID: "run", ActionCallID: "call", Kind: SkillSetupKindConnectApp, SkillID: "openseal.mcp", SkillVersion: "1.0.0", SkillName: "Linear",
			App: &ConnectAppTarget{CatalogSlug: "linear", Name: "Linear"}, Reason: "Connect Linear", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now, Mode: mode, RequestedFor: "7"}
		if err := r.Validate(); (err == nil) != valid {
			t.Errorf("connect_app mode %q: %v", mode, err)
		}
	}
}
