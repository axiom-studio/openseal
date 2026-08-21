package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	// RunContextSessionKey is reserved for a kernel-created durable session
	// reference. Ordinary request handlers must reject caller-authored context
	// keys with the _openseal prefix.
	RunContextSessionKey            = "_opensealSession"
	sessionContextIDKey             = "id"
	sessionContextVerifiedClaimsKey = "verifiedClaims"
)

// BindingActionArgumentResolver applies deployment binding values after model
// output and before schema, policy, approval, persistence, and execution.
type BindingActionArgumentResolver struct{}

func (BindingActionArgumentResolver) ResolveActionProposalArguments(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if input.Bound == nil || input.Bound.Binding == nil {
		return nil, false, nil
	}
	bindings := input.Bound.Binding.ArgumentBindings[input.Bound.Action.Name]
	if len(bindings) == 0 {
		return nil, false, nil
	}
	resolved := cloneMap(input.Arguments)
	for name, binding := range bindings {
		value, err := resolveBindingActionArgument(input.Run, binding)
		if err != nil {
			return nil, false, fmt.Errorf("resolve bound action argument %s: %w", name, err)
		}
		resolved[name] = value
	}
	return resolved, true, nil
}

func (BindingActionArgumentResolver) ValidateActionProposal(context.Context, ActionProposalValidationInput) (map[string]interface{}, error) {
	return nil, nil
}

func resolveBindingActionArgument(run *AgentRun, binding skill.BindingArgumentValue) (interface{}, error) {
	switch binding.Source {
	case skill.BindingArgumentLiteral:
		return cloneBindingArgumentValue(binding.Literal), nil
	case skill.BindingArgumentSessionID:
		session, err := trustedRunSessionContext(run)
		if err != nil {
			return nil, err
		}
		id, _ := session[sessionContextIDKey].(string)
		if !validOpaqueIdentifier(id, 128) {
			return nil, errors.New("durable session id is missing or invalid")
		}
		return id, nil
	case skill.BindingArgumentVerifiedClaim:
		session, err := trustedRunSessionContext(run)
		if err != nil {
			return nil, err
		}
		claims, _ := session[sessionContextVerifiedClaimsKey].(map[string]interface{})
		claim := strings.TrimSpace(binding.Claim)
		value, ok := claims[claim]
		if !ok {
			return nil, fmt.Errorf("verified session claim %q is not available", claim)
		}
		return cloneBindingArgumentValue(value), nil
	default:
		return nil, fmt.Errorf("unsupported binding source %q", binding.Source)
	}
}

func cloneBindingArgumentValue(value interface{}) interface{} {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result interface{}
	if json.Unmarshal(encoded, &result) != nil {
		return nil
	}
	return result
}

func trustedRunSessionContext(run *AgentRun) (map[string]interface{}, error) {
	if run == nil {
		return nil, errors.New("durable Run is required")
	}
	session, _ := run.Context[RunContextSessionKey].(map[string]interface{})
	if session == nil {
		return nil, errors.New("Run is not attached to a trusted session")
	}
	return session, nil
}

func bindingResolvedArgumentProvenance(bound *skill.BoundAction) map[string]ResolvedActionArgument {
	if bound == nil || bound.Binding == nil {
		return nil
	}
	bindings := bound.Binding.ArgumentBindings[bound.Action.Name]
	if len(bindings) == 0 {
		return nil
	}
	result := make(map[string]ResolvedActionArgument, len(bindings))
	for name, binding := range bindings {
		result[name] = ResolvedActionArgument{Source: binding.Source, Claim: strings.TrimSpace(binding.Claim)}
	}
	return result
}

// EmbedSessionActionAuthorityValidator is the defense-in-depth policy gate for
// calls originating from an embed session. The turn catalog is filtered too,
// but direct or stale proposals must still fail closed here.
type EmbedSessionActionAuthorityValidator struct{}

func (EmbedSessionActionAuthorityValidator) ValidateActionProposal(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Run == nil || input.Bound == nil || input.Bound.Binding == nil {
		return nil, nil
	}
	session, _ := input.Run.Context[RunContextSessionKey].(map[string]interface{})
	if session == nil {
		return nil, nil
	}
	authority, _ := session["authority"].(map[string]interface{})
	if !embedAuthorityAllows(authority, input.Bound.Binding.ID, input.Bound.Action.Name, input.Bound.Action.Risk) {
		return nil, errors.New("embed session does not grant this Skill action")
	}
	return map[string]interface{}{"embedSessionAuthority": "granted"}, nil
}

func constrainEmbedSessionActions(run *AgentRun, actions []skill.ModelAction) []skill.ModelAction {
	if run == nil {
		return actions
	}
	session, _ := run.Context[RunContextSessionKey].(map[string]interface{})
	if session == nil {
		return actions
	}
	authority, _ := session["authority"].(map[string]interface{})
	result := make([]skill.ModelAction, 0, len(actions))
	for _, action := range actions {
		if embedAuthorityAllows(authority, action.BindingID, action.Action, action.Risk) {
			result = append(result, action)
		}
	}
	return result
}

func embedAuthorityAllows(authority map[string]interface{}, bindingID, action string, risk skill.RiskLevel) bool {
	if authority == nil || embedRiskRank(risk) > embedRiskRank(skill.RiskLevel(strings.TrimSpace(fmt.Sprint(authority["maximumActionRisk"])))) {
		return false
	}
	grants, _ := authority["actionGrants"].([]interface{})
	for _, raw := range grants {
		grant, _ := raw.(map[string]interface{})
		if strings.TrimSpace(fmt.Sprint(grant["bindingId"])) == bindingID && strings.TrimSpace(fmt.Sprint(grant["action"])) == action {
			return true
		}
	}
	return false
}

func embedRiskRank(value skill.RiskLevel) int {
	switch value {
	case skill.RiskLevelRead:
		return 1
	case skill.RiskLevelWrite:
		return 2
	case skill.RiskLevelExternal:
		return 3
	case skill.RiskLevelProduction:
		return 4
	case skill.RiskLevelDestructive:
		return 5
	default:
		return 100
	}
}
