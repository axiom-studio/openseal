package authoring

import (
	"encoding/json"
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
		if reference.action == nil {
			continue
		}
		skillID, version, action := strings.TrimSpace(reference.action.SkillID), strings.TrimSpace(reference.action.SkillVersion), strings.TrimSpace(reference.action.Action)
		skillCapability, exists := catalog.Skills[skillID]
		if !exists || strings.TrimSpace(skillCapability.Version) != version {
			continue // Existing catalog/missing-requirement validation owns this diagnostic.
		}
		contract, exists := skillCapability.ActionContracts[action]
		if !exists || contract.InputSchema == nil {
			continue // Older minimal catalogs have no exact host contract to validate.
		}
		inputs := make(map[string]interface{}, len(reference.action.Arguments))
		dynamic := false
		for name, value := range reference.action.Arguments {
			if value.Ref != "" || len(value.Template) > 0 || len(value.Literal) == 0 {
				dynamic = true
				break
			}
			var decoded interface{}
			if err := json.Unmarshal(value.Literal, &decoded); err != nil {
				dynamic = true
				break
			}
			inputs[name] = decoded
		}
		if dynamic {
			continue // Canonical Runbook validation owns dynamic dataflow.
		}
		if err := runbook.ValidateInterfaceInput(contract.InputSchema, inputs); err != nil {
			issues = append(issues, issue(
				reference.path+".arguments",
				code,
				fmt.Sprintf("Skill %s@%s action %s inputs do not match the authorized contract: %v", skillID, version, action, err),
			))
		}
	}
	return issues
}
