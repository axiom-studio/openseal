package server

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
)

var _ action.AgentBuilderApprovalAuthorizer = DesktopApprovalAuthorizer{}

// DesktopApprovalAuthorizer binds decisions to the owner of one authenticated
// local workspace. The local owner holds the default operator role, not arbitrary
// named users or tenant roles. Checkpoint eligibility remains mandatory.
type DesktopApprovalAuthorizer struct{ Scope models.Scope }

func (a DesktopApprovalAuthorizer) ValidateApprovalRequest(scope models.Scope, principal models.ApprovalPrincipal) error {
	if a.Scope.Kind != "local" || strings.TrimSpace(a.Scope.ID) == "" || scope != a.Scope || principal.Type != "user" || principal.ID != "local-operator" {
		return errors.New("desktop approvals require the configured local workspace operator")
	}
	return nil
}

// AuthorizeAgentBuilderApproval lets the authenticated local operator approve
// agent-builder cards (create_agent, update_agent, create_team; D11). The
// coordinator has already required a "user" principal and AuthorizeApproval.
func (a DesktopApprovalAuthorizer) AuthorizeAgentBuilderApproval(_ context.Context, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
	if approval == nil {
		return errors.New("approval is required")
	}
	return a.ValidateApprovalRequest(approval.Scope, principal)
}

func (a DesktopApprovalAuthorizer) AuthorizeApproval(_ context.Context, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
	if approval == nil {
		return errors.New("approval is required")
	}
	if err := a.ValidateApprovalRequest(approval.Scope, principal); err != nil {
		return err
	}
	for _, eligible := range approval.EligibleApprovers {
		if eligible == principal || eligible == (models.ApprovalPrincipal{Type: "role", ID: "operator"}) {
			return nil
		}
	}
	return errors.New("the local operator is not an eligible reviewer for this action")
}
