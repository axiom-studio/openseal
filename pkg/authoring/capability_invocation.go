package authoring

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

// validateObjectiveCapabilityInputs checks every durable Objective action
// intent against the exact, host-owned action contract that was reviewed with
// the candidate. Action contracts are deliberately absent from provider
// prompts, so this validation is deterministic and cannot be weakened by
// model output.
func validateObjectiveCapabilityInputs(candidate *WorkforceCandidate, catalog CapabilityCatalog, readiness bool) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	code := "capability_action_input_invalid"
	if readiness {
		code = readinessValidationCodePrefix + code
	}
	issues := make([]ValidationIssue, 0)
	for _, reference := range candidateObjectiveCapabilityInvocations(candidate) {
		invocation := reference.invocation
		skillID, _ := invocation["skillId"].(string)
		version, _ := invocation["skillVersion"].(string)
		action, _ := invocation["action"].(string)
		skillID, version, action = strings.TrimSpace(skillID), strings.TrimSpace(version), strings.TrimSpace(action)
		skillCapability, exists := catalog.Skills[skillID]
		if !exists || strings.TrimSpace(skillCapability.Version) != version {
			continue // Existing catalog/missing-requirement validation owns this diagnostic.
		}
		contract, exists := skillCapability.ActionContracts[action]
		if !exists || contract.InputSchema == nil {
			continue // Older minimal catalogs have no exact host contract to validate.
		}
		inputs, _ := invocation["inputs"].(map[string]interface{})
		if inputs == nil {
			inputs = map[string]interface{}{}
		}
		if err := runbook.ValidateInterfaceInput(contract.InputSchema, inputs); err != nil {
			issues = append(issues, issue(
				reference.path+".runTemplate.capability.inputs",
				code,
				fmt.Sprintf("Skill %s@%s action %s inputs do not match the authorized contract: %v", skillID, version, action, err),
			))
		}
	}
	return issues
}
