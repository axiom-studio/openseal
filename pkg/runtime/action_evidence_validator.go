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
			return nil, fmt.Errorf("action requires successful %s evidence from the same Run", strings.TrimSpace(requirement.Action))
		}
	}
	return nil, nil
}
