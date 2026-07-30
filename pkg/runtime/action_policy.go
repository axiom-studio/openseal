package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type DefaultActionPolicy struct {
	Approvers       []ApprovalPrincipal
	ApprovalTTL     time.Duration
	ApprovalTimeout ApprovalTimeoutDecision
}

func NewDefaultActionPolicy() *DefaultActionPolicy {
	return &DefaultActionPolicy{Approvers: []ApprovalPrincipal{{Type: "role", ID: "operator"}}, ApprovalTTL: 24 * time.Hour}
}

func (p *DefaultActionPolicy) EvaluateAction(_ context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
	if input.Bound == nil || input.Bound.Definition == nil || input.Bound.Binding == nil {
		return ActionPolicyDecision{}, errors.New("bound action is required")
	}
	action := input.Bound.Action
	if action.Risk == skill.RiskLevelRead && (action.SideEffect == skill.SideEffectNone || action.SideEffect == skill.SideEffectRead) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "read-only action"}, nil
	}
	if p == nil || len(p.Approvers) == 0 {
		return ActionPolicyDecision{}, errors.New("side-effecting actions require configured approvers")
	}
	ttl := p.ApprovalTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return ActionPolicyDecision{
		Disposition:       ActionDispositionRequireApproval,
		Reason:            fmt.Sprintf("%s risk action with %s side effects", action.Risk, action.SideEffect),
		EligibleApprovers: append([]ApprovalPrincipal(nil), p.Approvers...), ApprovalTTL: ttl,
		ApprovalTimeout: p.ApprovalTimeout,
	}, nil
}

type EligibleApprovalAuthorizer struct{}

func (EligibleApprovalAuthorizer) AuthorizeApproval(_ context.Context, principal ApprovalPrincipal, approval *ApprovalCheckpoint) error {
	if approval == nil {
		return errors.New("approval is required")
	}
	for _, eligible := range approval.EligibleApprovers {
		if eligible == principal {
			return nil
		}
	}
	return errors.New("principal is not eligible to resolve this approval")
}
