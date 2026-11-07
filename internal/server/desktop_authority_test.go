package server

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestDesktopActionAuthorityPreservesCheckpointEligibility(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	host := DesktopApprovalAuthorizer{Scope: scope}
	principal := runtime.ApprovalPrincipal{Type: "user", ID: "local-operator"}
	for _, eligible := range []runtime.ApprovalPrincipal{principal, {Type: "role", ID: "operator"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &runtime.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []runtime.ApprovalPrincipal{eligible}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []runtime.ApprovalPrincipal{{Type: "user", ID: "alice"}, {Type: "role", ID: "admin"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &runtime.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []runtime.ApprovalPrincipal{other}}); err == nil {
			t.Fatal("accepted unrelated eligible reviewer")
		}
		if err := host.ValidateApprovalRequest(scope, other); err == nil {
			t.Fatal("accepted impersonated reviewer")
		}
	}
	if err := host.ValidateApprovalRequest(runtime.Scope{Kind: "local", ID: "other"}, principal); err == nil {
		t.Fatal("accepted foreign scope")
	}
}

func TestDesktopApprovalAuthorizerAgentBuilder(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	var host runtime.AgentBuilderApprovalAuthorizer = DesktopApprovalAuthorizer{Scope: scope}
	operator := runtime.ApprovalPrincipal{Type: "user", ID: "local-operator"}
	card := &runtime.ApprovalCheckpoint{Scope: scope, ProposedAction: map[string]interface{}{"resourceType": "agent_profile", "operation": "create_agent"}}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, card); err != nil {
		t.Fatalf("local operator approve = %v", err)
	}
	for _, other := range []runtime.ApprovalPrincipal{{Type: "user", ID: "alice"}, {Type: "role", ID: "operator"}} {
		if err := host.AuthorizeAgentBuilderApproval(t.Context(), other, card); err == nil {
			t.Fatalf("%#v approved an agent-builder card", other)
		}
	}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, &runtime.ApprovalCheckpoint{Scope: runtime.Scope{Kind: "local", ID: "other"}}); err == nil {
		t.Fatal("accepted foreign scope")
	}
	if err := host.AuthorizeAgentBuilderApproval(t.Context(), operator, nil); err == nil {
		t.Fatal("accepted a missing approval")
	}
}
