package server

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
)

func TestDesktopActionAuthorityPreservesCheckpointEligibility(t *testing.T) {
	scope := models.Scope{Kind: "local", ID: "default"}
	host := DesktopApprovalAuthorizer{Scope: scope}
	principal := models.ApprovalPrincipal{Type: "user", ID: "local-operator"}
	for _, eligible := range []models.ApprovalPrincipal{principal, {Type: "role", ID: "operator"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &models.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []models.ApprovalPrincipal{eligible}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []models.ApprovalPrincipal{{Type: "user", ID: "alice"}, {Type: "role", ID: "admin"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &models.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []models.ApprovalPrincipal{other}}); err == nil {
			t.Fatal("accepted unrelated eligible reviewer")
		}
		if err := host.ValidateApprovalRequest(scope, other); err == nil {
			t.Fatal("accepted impersonated reviewer")
		}
	}
	if err := host.ValidateApprovalRequest(models.Scope{Kind: "local", ID: "other"}, principal); err == nil {
		t.Fatal("accepted foreign scope")
	}
}

func TestDesktopApprovalAuthorizerAgentBuilder(t *testing.T) {
	scope := models.Scope{Kind: "local", ID: "default"}
	var host action.AgentBuilderApprovalAuthorizer = DesktopApprovalAuthorizer{Scope: scope}
	operator := models.ApprovalPrincipal{Type: "user", ID: "local-operator"}
	card := &models.ApprovalCheckpoint{Scope: scope, ProposedAction: map[string]interface{}{"resourceType": "agent_profile", "operation": "create_agent"}}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, card); err != nil {
		t.Fatalf("local operator approve = %v", err)
	}
	for _, other := range []models.ApprovalPrincipal{{Type: "user", ID: "alice"}, {Type: "role", ID: "operator"}} {
		if err := host.AuthorizeAgentBuilderApproval(t.Context(), other, card); err == nil {
			t.Fatalf("%#v approved an agent-builder card", other)
		}
	}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, &models.ApprovalCheckpoint{Scope: models.Scope{Kind: "local", ID: "other"}}); err == nil {
		t.Fatal("accepted foreign scope")
	}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, nil); err == nil {
		t.Fatal("accepted a missing approval")
	}
}
