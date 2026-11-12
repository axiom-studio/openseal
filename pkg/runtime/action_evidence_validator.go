package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// RequiredActionEvidenceValidator proves preparatory action requirements from
// the kernel-owned Run history. A model cannot satisfy this contract by saying
// that it filled, uploaded, rendered, or otherwise prepared something.
type RequiredActionEvidenceValidator struct{}

// actionAdvanceRecoveryError marks proposal failures that cannot be repaired by
// merely changing arguments on the rejected capability. A different governed
// action must succeed first and become part of the kernel-owned action history.
// Implementations may name the exact prerequisite action when the Skill
// contract declares one.
type actionAdvanceRecoveryError interface {
	error
	actionAdvanceRecovery() (prerequisiteAction string, required bool)
}

type requiredActionEvidenceError struct {
	action string
}

func (e requiredActionEvidenceError) Error() string {
	return fmt.Sprintf("action requires successful %s evidence from the same Run", e.action)
}

func (e requiredActionEvidenceError) actionAdvanceRecovery() (string, bool) {
	return e.action, true
}

func (RequiredActionEvidenceValidator) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Run == nil || input.Bound == nil || input.Bound.Definition == nil || len(input.Bound.Action.RequiredEvidence) == 0 {
		return nil, nil
	}
	var calls []*ActionCall
	if input.History != nil {
		listed, err := input.History.Calls(ctx)
		if err != nil {
			return nil, fmt.Errorf("read Run action history: %w", err)
		}
		calls = listed
	}
	for _, requirement := range input.Bound.Action.RequiredEvidence {
		action := strings.TrimSpace(requirement.Action)
		matched := false
		for _, call := range calls {
			if call == nil || call.Status != ActionCallStatusSucceeded ||
				call.SkillID != input.Bound.Definition.ID ||
				call.SkillVersion != input.Bound.Definition.Version ||
				call.Action != action {
				continue
			}
			matches := true
			for _, name := range requirement.MatchingArguments {
				if !reflect.DeepEqual(call.Arguments[name], input.Arguments[name]) {
					matches = false
					break
				}
			}
			if matches {
				matched = true
				break
			}
		}
		if !matched {
			return nil, requiredActionEvidenceError{action: action}
		}
	}
	return nil, nil
}
