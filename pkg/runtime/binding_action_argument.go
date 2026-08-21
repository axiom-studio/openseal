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
