package runtime

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var portableAgentSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// KernelResolvedActionArgumentResolver projects trusted durable context into
// canonical Skill inputs that must never be selected or invented by a model.
// Skills opt in field-by-field through the portable schema extensions.
type KernelResolvedActionArgumentResolver struct{}

func (KernelResolvedActionArgumentResolver) ResolveActionProposalArguments(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if input.Bound == nil || input.Bound.Action.InputSchema == nil {
		return nil, false, nil
	}
	properties, _ := input.Bound.Action.InputSchema["properties"].(map[string]interface{})
	resolved := cloneMap(input.Arguments)
	handled := false
	for name, raw := range properties {
		property, _ := raw.(map[string]interface{})
		kernelResolved, _ := property[skill.SchemaExtensionKernelResolved].(bool)
		if !kernelResolved {
			continue
		}
		source, _ := property[skill.SchemaExtensionKernelSource].(string)
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		var value interface{}
		switch source {
		case skill.KernelSourceAgentSessionID:
			sessionID, err := kernelAgentSessionID(input.Run)
			if err != nil {
				return nil, false, fmt.Errorf("resolve kernel action argument %s: %w", name, err)
			}
			value = sessionID
		default:
			return nil, false, fmt.Errorf("resolve kernel action argument %s: unsupported source %q", name, source)
		}
		resolved[name] = value
		handled = true
	}
	return resolved, handled, nil
}

func (KernelResolvedActionArgumentResolver) ValidateActionProposal(context.Context, ActionProposalValidationInput) (map[string]interface{}, error) {
	return nil, nil
}

func kernelAgentSessionID(run *AgentRun) (string, error) {
	if run == nil {
		return "", errors.New("durable Run is required")
	}
	agentID := strings.TrimSpace(run.AssignedAgentID)
	if agentID == "" && run.Owner.Type == OwnerTypeAgent {
		agentID = strings.TrimSpace(run.Owner.ID)
	}
	if agentID == "" {
		return "", errors.New("assigned durable Agent is required")
	}
	if portableAgentSessionID.MatchString(agentID) {
		return agentID, nil
	}
	digest := strings.TrimPrefix(hashString(agentID), "sha256:")
	return "agent-" + digest[:32], nil
}
