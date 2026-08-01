package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"
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

func (RequiredActionEvidenceValidator) ValidateActionProposal(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Run == nil || input.Bound == nil || len(input.Bound.Action.RequiredEvidence) == 0 {
		return nil, nil
	}
	entries := actionHistoryEntries(input.Run.Checkpoint)
	for _, requirement := range input.Bound.Action.RequiredEvidence {
		matched := false
		for index := len(entries) - 1; index >= 0; index-- {
			entry := entries[index]
			if strings.TrimSpace(fmt.Sprint(entry["action"])) != strings.TrimSpace(requirement.Action) ||
				strings.TrimSpace(fmt.Sprint(entry["skillId"])) != input.Bound.Definition.ID ||
				strings.TrimSpace(fmt.Sprint(entry["skillVersion"])) != input.Bound.Definition.Version ||
				strings.TrimSpace(fmt.Sprint(entry["status"])) != string(ActionCallStatusSucceeded) {
				continue
			}
			arguments, _ := entry["arguments"].(map[string]interface{})
			matches := true
			for _, name := range requirement.MatchingArguments {
				if !reflect.DeepEqual(arguments[name], input.Arguments[name]) {
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
			return nil, requiredActionEvidenceError{action: strings.TrimSpace(requirement.Action)}
		}
	}
	return nil, nil
}
