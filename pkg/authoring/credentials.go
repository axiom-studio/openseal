package authoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ValidateCredentialPlacement verifies that every supplied placement uses a
// canonical credential kind and one currently authorized opaque reference.
// When a generated candidate is supplied, references are also restricted to
// the exact kinds required by that Agent. Resolved credential values never
// cross this boundary.
func ValidateCredentialPlacement(candidate *WorkforceCandidate, required map[string][]string, placement ChangeSetPlacement, choices []capability.CredentialBindingChoice) error {
	authorized := make(map[string]map[string]bool)
	declaredBindings := make(map[string]bool)
	for _, choice := range choices {
		kind := strings.TrimSpace(choice.Reference.Kind)
		id := strings.TrimSpace(choice.Reference.ID)
		if kind == "" || id == "" {
			return fmt.Errorf("credential binding choices require an opaque kind and reference")
		}
		bindingKeys := append([]string(nil), choice.BindingKeys...)
		if len(bindingKeys) == 0 {
			bindingKeys = []string{kind}
		}
		seenKeys := make(map[string]bool, len(bindingKeys))
		for _, bindingKey := range bindingKeys {
			bindingKey = strings.TrimSpace(bindingKey)
			if bindingKey == "" || seenKeys[bindingKey] {
				return fmt.Errorf("credential binding choices require unique non-empty binding keys")
			}
			seenKeys[bindingKey] = true
			declaredBindings[bindingKey] = true
			if authorized[bindingKey] == nil {
				authorized[bindingKey] = make(map[string]bool)
			}
			authorized[bindingKey][kind+"\x00"+id] = true
		}
	}

	requiredByAgent := make(map[string]map[string]bool)
	if candidate != nil {
		for _, definition := range candidate.Agents {
			if definition == nil {
				continue
			}
			kinds := make(map[string]bool)
			for _, kind := range required[definition.ID] {
				if kind = strings.TrimSpace(kind); kind != "" {
					kinds[kind] = true
				}
			}
			requiredByAgent[definition.ID] = kinds
		}
	}

	for agentID, references := range placement.CredentialReferences {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			return fmt.Errorf("credential placement requires an Agent definition ID")
		}
		allowedKinds, knownAgent := requiredByAgent[agentID]
		if candidate != nil && !knownAgent {
			return fmt.Errorf("credential placement Agent %s is not part of this workforce candidate", agentID)
		}
		for key, reference := range references {
			key = strings.TrimSpace(key)
			kind := strings.TrimSpace(reference.Kind)
			id := strings.TrimSpace(reference.ID)
			if key == "" || kind == "" || id == "" {
				return fmt.Errorf("Agent %s credential placement requires a kind and opaque reference", agentID)
			}
			if key != kind && !declaredBindings[key] {
				return fmt.Errorf("Agent %s credential placement key %s must match credential kind %s", agentID, key, kind)
			}
			if candidate != nil && !allowedKinds[key] {
				expected := sortedCredentialKinds(allowedKinds)
				if len(expected) == 0 {
					return fmt.Errorf("Agent %s does not require credential kind %s", agentID, key)
				}
				return fmt.Errorf("Agent %s credential key %s is not a required kind; expected %s", agentID, key, strings.Join(expected, ", "))
			}
			if !authorized[key][kind+"\x00"+id] {
				return fmt.Errorf("Agent %s credential reference for binding %s is unavailable or no longer authorized", agentID, key)
			}
		}
	}
	return nil
}

func sortedCredentialKinds(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
