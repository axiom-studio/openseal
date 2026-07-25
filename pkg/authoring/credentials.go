package authoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type skillCredentialBinding struct {
	Key  string
	Kind string
}

// requiredSkillCredentialBindings returns the exact deployment slots consumed
// by the selected Skill authority. Structured credential declarations use
// their stable names so two credentials of the same transport kind cannot
// satisfy or overwrite one another. CredentialKinds remains a legacy fallback
// for older host catalogs that do not yet project structured credentials.
func requiredSkillCredentialBindings(skill SkillCapability, requiredActions []string) []skillCredentialBinding {
	selectedActions := make(map[string]bool, len(requiredActions))
	for _, action := range requiredActions {
		if action = strings.TrimSpace(action); action != "" {
			selectedActions[action] = true
		}
	}
	result := make([]skillCredentialBinding, 0, len(skill.Credentials)+len(skill.CredentialKinds))
	seen := make(map[string]bool, cap(result))
	if len(skill.Credentials) > 0 {
		for _, credential := range skill.Credentials {
			if credential.Optional {
				continue
			}
			needed := len(credential.Actions) == 0
			for _, action := range credential.Actions {
				if selectedActions[strings.TrimSpace(action)] {
					needed = true
					break
				}
			}
			if !needed {
				continue
			}
			key, kind := strings.TrimSpace(credential.Name), strings.TrimSpace(credential.Kind)
			if key == "" {
				key = kind
			}
			if key == "" || kind == "" || seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, skillCredentialBinding{Key: key, Kind: kind})
		}
	} else {
		for _, value := range skill.CredentialKinds {
			kind := strings.TrimSpace(value)
			if kind == "" || seen[kind] {
				continue
			}
			seen[kind] = true
			result = append(result, skillCredentialBinding{Key: kind, Kind: kind})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// ValidateCredentialPlacement verifies that every supplied placement uses a
// canonical credential kind and one currently authorized opaque reference.
// When a generated candidate is supplied, references are also restricted to
// the exact binding slots required by that Agent. Resolved credential values
// never cross this boundary.
func ValidateCredentialPlacement(candidate *WorkforceCandidate, required map[string][]string, placement ChangeSetPlacement, choices []capability.CredentialBindingChoice) error {
	authorized := make(map[string]map[string]bool)
	declaredBindings := make(map[string]bool)
	deploymentBindings := make(map[string]bool)
	for _, choice := range choices {
		kind := strings.TrimSpace(choice.Reference.Kind)
		id := strings.TrimSpace(choice.Reference.ID)
		if kind == "" || id == "" {
			return fmt.Errorf("credential binding choices require an opaque kind and reference")
		}
		bindingKeys := append([]string(nil), choice.BindingKeys...)
		explicitBindingKeys := len(bindingKeys) > 0
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
			if explicitBindingKeys && bindingKey == agent.ModelProviderCredentialBinding {
				deploymentBindings[bindingKey] = true
			}
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
		allowedBindings, knownAgent := requiredByAgent[agentID]
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
			// Explicit BindingKeys are host-advertised deployment slots rather
			// than Skill credential kinds. They may be configured before they
			// become required (for example, an inactive Agent may already have
			// its model provider selected). Skill credentials remain restricted
			// to the exact requirements declared by the candidate.
			if candidate != nil && !allowedBindings[key] && !deploymentBindings[key] {
				expected := sortedCredentialBindings(allowedBindings)
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

func sortedCredentialBindings(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
