package server

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const desktopInstallPolicy = "local-desktop-install"
const desktopOwnerRole = "workspace-owner"

// WorkforcePolicyDecision is a host-authored policy result. When present, it
// replaces all policy claims from an interactive evaluation request.
type WorkforcePolicyDecision struct {
	Allowed              bool
	Findings             []authoring.ChangeSetPolicyFinding
	ApprovalRequirements []authoring.ChangeSetApprovalRequirement
}

// DesktopLifecycleAuthorizer grants one authenticated local workspace owner
// lifecycle control. It is only installed by the explicit desktop daemon mode,
// which requires a loopback listener and a bearer token. It does not represent
// tenant/enterprise roles or authorize calls to external providers.
type DesktopLifecycleAuthorizer struct {
	Scope capability.ScopeReference
}

func (a DesktopLifecycleAuthorizer) AuthorizeWorkforceLifecycle(_ context.Context, operation string, changeSet *authoring.ChangeSet) (WorkforceLifecycleAuthorization, error) {
	if a.Scope.Kind != "local" || strings.TrimSpace(a.Scope.ID) == "" || changeSet == nil || changeSet.Scope != a.Scope {
		return WorkforceLifecycleAuthorization{}, errors.New("desktop authority requires its configured local workspace")
	}
	result := WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "user", ID: "local-operator"}}
	switch operation {
	case kernelapi.OperationRetry, kernelapi.OperationRefine, kernelapi.OperationPatch, kernelapi.OperationActivate:
		return result, nil
	case kernelapi.OperationApply:
		if !desktopOwnerReviewed(changeSet, result.Actor) {
			return WorkforceLifecycleAuthorization{}, errors.New("desktop installation requires its host policy and workspace-owner approval")
		}
		return result, nil
	case kernelapi.OperationEvaluate:
		result.Actor = authoring.ChangeSetActor{Type: "policy_evaluator", ID: "local-desktop-policy"}
		result.PolicyDecision = &WorkforcePolicyDecision{
			Allowed:              true,
			Findings:             []authoring.ChangeSetPolicyFinding{{PolicyID: desktopInstallPolicy, Code: "owner_review_required", Message: "The workspace owner must approve this exact proposal before installation. Kernel readiness checks still apply."}},
			ApprovalRequirements: []authoring.ChangeSetApprovalRequirement{{PolicyID: desktopInstallPolicy, Role: desktopOwnerRole, Count: 1}},
		}
		return result, nil
	case kernelapi.OperationApprove:
		for _, evaluation := range changeSet.Evaluations {
			if evaluation.CandidateDigest != changeSet.CandidateDigest || !evaluation.Allowed || evaluation.Actor.ID != "local-desktop-policy" || evaluation.Actor.Type != "policy_evaluator" {
				continue
			}
			for _, requirement := range evaluation.ApprovalRequirements {
				if requirement.PolicyID == desktopInstallPolicy && requirement.Role == desktopOwnerRole {
					result.EligibleApprovalRequirements = append(result.EligibleApprovalRequirements, kernelapi.ApprovalRequirementReference{EvaluationID: evaluation.ID, PolicyID: desktopInstallPolicy, Role: desktopOwnerRole})
				}
			}
		}
		return result, nil
	default:
		return WorkforceLifecycleAuthorization{}, errors.New("unsupported desktop lifecycle operation")
	}
}

// A ready proposal imported from another host is not evidence of this owner's
// review. Require the latest evaluation and its exact immutable decision.
func desktopOwnerReviewed(changeSet *authoring.ChangeSet, owner authoring.ChangeSetActor) bool {
	if len(changeSet.Evaluations) == 0 {
		return false
	}
	evaluation := changeSet.Evaluations[len(changeSet.Evaluations)-1]
	if evaluation.CandidateDigest != changeSet.CandidateDigest || !evaluation.Allowed || evaluation.Actor.Type != "policy_evaluator" || evaluation.Actor.ID != "local-desktop-policy" {
		return false
	}
	required := false
	for _, requirement := range evaluation.ApprovalRequirements {
		if requirement.PolicyID == desktopInstallPolicy && requirement.Role == desktopOwnerRole && requirement.Count == 1 {
			required = true
		}
	}
	if !required {
		return false
	}
	for _, decision := range changeSet.ApprovalDecisions {
		if decision.EvaluationID == evaluation.ID && decision.PolicyID == desktopInstallPolicy && decision.Role == desktopOwnerRole && decision.Actor == owner && decision.Approved {
			return true
		}
	}
	return false
}

// DesktopApprovalAuthorizer binds decisions to the owner of one authenticated
// local workspace. The local owner holds the default operator role, not arbitrary
// named users or tenant roles. Checkpoint eligibility remains mandatory.
type DesktopApprovalAuthorizer struct{ Scope runtime.Scope }

func (a DesktopApprovalAuthorizer) ValidateApprovalRequest(scope runtime.Scope, principal runtime.ApprovalPrincipal) error {
	if a.Scope.Kind != "local" || strings.TrimSpace(a.Scope.ID) == "" || scope != a.Scope || principal.Type != "user" || principal.ID != "local-operator" {
		return errors.New("desktop approvals require the configured local workspace operator")
	}
	return nil
}

func (a DesktopApprovalAuthorizer) AuthorizeApproval(_ context.Context, principal runtime.ApprovalPrincipal, approval *runtime.ApprovalCheckpoint) error {
	if approval == nil {
		return errors.New("approval is required")
	}
	if err := a.ValidateApprovalRequest(approval.Scope, principal); err != nil {
		return err
	}
	for _, eligible := range approval.EligibleApprovers {
		if eligible == principal || eligible == (runtime.ApprovalPrincipal{Type: "role", ID: "operator"}) {
			return nil
		}
	}
	return errors.New("the local operator is not an eligible reviewer for this action")
}
