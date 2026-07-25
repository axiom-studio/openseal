package authoring

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type skillCredentialBinding struct {
	Key    string
	Kind   string
	OAuth2 *capability.OAuth2Requirement
}

// CredentialBindingRequirement is the complete, secret-free deployment slot
// contract derived from a selected Skill. OAuth2 is nil for ordinary static or
// host-managed secret references.
type CredentialBindingRequirement struct {
	Key    string                        `json:"key"`
	Kind   string                        `json:"kind"`
	OAuth2 *capability.OAuth2Requirement `json:"oauth2,omitempty"`
}

// RequiredCredentialBindings derives exact active deployment requirements
// from a candidate and its authorized catalog. Hosts use the result when
// validating a selected opaque connection; no credential reference or value is
// included.
func RequiredCredentialBindings(candidate WorkforceCandidate, catalog CapabilityCatalog) map[string][]CredentialBindingRequirement {
	result := make(map[string][]CredentialBindingRequirement)
	activation, activationErr := EffectiveWorkforceActivationIntent(candidate.Activation)
	if activationErr != nil || activation != WorkforceActivationActive {
		return result
	}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		byKey := make(map[string]CredentialBindingRequirement)
		for _, selected := range definition.SkillRequirements {
			for _, binding := range requiredSkillCredentialBindings(catalog.Skills[selected.SkillID], selected.RequiredActions) {
				if binding.Key == "" {
					continue
				}
				byKey[binding.Key] = CredentialBindingRequirement{
					Key: binding.Key, Kind: binding.Kind, OAuth2: binding.OAuth2,
				}
			}
		}
		for _, requirement := range catalog.AgentCredentialRequirements {
			if requirement.RequiredForActivation {
				key := strings.TrimSpace(requirement.BindingKey)
				if key != "" {
					if _, exists := byKey[key]; !exists {
						byKey[key] = CredentialBindingRequirement{Key: key}
					}
				}
			}
		}
		for _, requirement := range byKey {
			result[definition.ID] = append(result[definition.ID], requirement)
		}
		sort.Slice(result[definition.ID], func(i, j int) bool {
			return result[definition.ID][i].Key < result[definition.ID][j].Key
		})
	}
	for _, endpoint := range candidate.ConversationEndpoints {
		skillCapability, ok := catalog.Skills[endpoint.SkillID]
		if !ok || skillCapability.Version != endpoint.SkillVersion {
			continue
		}
		var adapter *ConversationAdapterCapability
		for index := range skillCapability.ConversationAdapters {
			if skillCapability.ConversationAdapters[index].ID == endpoint.AdapterID {
				adapter = &skillCapability.ConversationAdapters[index]
				break
			}
		}
		if adapter == nil {
			continue
		}
		target := endpoint.Owner.ID
		byKey := make(map[string]CredentialBindingRequirement, len(result[target])+len(adapter.Credentials))
		for _, requirement := range result[target] {
			byKey[requirement.Key] = requirement
		}
		for _, credential := range adapter.Credentials {
			if credential.Optional {
				continue
			}
			byKey[credential.Name] = CredentialBindingRequirement{
				Key: credential.Name, Kind: credential.Kind, OAuth2: credential.OAuth2,
			}
		}
		result[target] = result[target][:0]
		for _, requirement := range byKey {
			result[target] = append(result[target], requirement)
		}
		sort.Slice(result[target], func(i, j int) bool {
			return result[target][i].Key < result[target][j].Key
		})
	}
	return result
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
	if len(skill.Credentials) > 0 {
		byKey := make(map[string]skillCredentialBinding)
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
			if key == "" || kind == "" {
				continue
			}
			current, exists := byKey[key]
			if !exists {
				byKey[key] = skillCredentialBinding{Key: key, Kind: kind, OAuth2: credential.OAuth2}
				continue
			}
			current.OAuth2 = mergeOAuth2Requirements(current.OAuth2, credential.OAuth2)
			byKey[key] = current
		}
		for _, binding := range byKey {
			result = append(result, binding)
		}
	} else {
		seen := make(map[string]bool, len(skill.CredentialKinds))
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

func mergeOAuth2Requirements(left, right *capability.OAuth2Requirement) *capability.OAuth2Requirement {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	scopes := append(append([]string(nil), left.Scopes...), right.Scopes...)
	normalized, err := capability.NormalizeOAuth2Requirement(&capability.OAuth2Requirement{
		Provider: left.Provider, Subject: left.Subject, Resource: left.Resource, Scopes: scopes,
	})
	if err != nil {
		// Catalog validation rejects incompatible declarations before this
		// helper is used. Returning the left contract remains fail-closed for
		// malformed direct callers because placement catalog validation fails.
		return left
	}
	return normalized
}

// ValidateCredentialPlacement verifies that every supplied placement uses a
// canonical credential kind and one currently authorized opaque reference.
// When a generated candidate is supplied, references are also restricted to
// the exact binding slots required by that Agent. Resolved credential values
// never cross this boundary.
func ValidateCredentialPlacement(candidate *WorkforceCandidate, required map[string][]string, placement ChangeSetPlacement, choices []capability.CredentialBindingChoice) error {
	requirements := make(map[string][]CredentialBindingRequirement, len(required))
	for agentID, keys := range required {
		for _, key := range keys {
			requirements[agentID] = append(requirements[agentID], CredentialBindingRequirement{Key: key})
		}
	}
	return ValidateCredentialPlacementWithRequirements(candidate, requirements, placement, choices)
}

// ValidateCredentialPlacementWithRequirements validates exact credential kinds
// and OAuth 2 grant coverage in addition to opaque reference authorization.
// It is the canonical host boundary for new authoring integrations; the legacy
// kind-only function remains for callers that have not yet projected typed
// requirements.
func ValidateCredentialPlacementWithRequirements(candidate *WorkforceCandidate, required map[string][]CredentialBindingRequirement, placement ChangeSetPlacement, choices []capability.CredentialBindingChoice) error {
	authorized := make(map[string]map[string]*capability.OAuth2GrantSummary)
	declaredBindings := make(map[string]bool)
	deploymentBindings := make(map[string]bool)
	for _, choice := range choices {
		kind := strings.TrimSpace(choice.Reference.Kind)
		id := strings.TrimSpace(choice.Reference.ID)
		if kind == "" || id == "" {
			return fmt.Errorf("credential binding choices require an opaque kind and reference")
		}
		grant, err := capability.NormalizeOAuth2GrantSummary(choice.OAuth2)
		if err != nil || !reflect.DeepEqual(grant, choice.OAuth2) {
			return fmt.Errorf("credential binding choice %s has an invalid or non-canonical OAuth 2 grant summary", choice.DisplayName)
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
				authorized[bindingKey] = make(map[string]*capability.OAuth2GrantSummary)
			}
			identity := kind + "\x00" + id
			authorized[bindingKey][identity] = grant
		}
	}

	requiredByAgent := make(map[string]map[string]CredentialBindingRequirement)
	if candidate != nil {
		addOwner := func(ownerID string) {
			bindings := make(map[string]CredentialBindingRequirement)
			for _, requirement := range required[ownerID] {
				requirement.Key, requirement.Kind = strings.TrimSpace(requirement.Key), strings.TrimSpace(requirement.Kind)
				if requirement.Key != "" {
					bindings[requirement.Key] = requirement
				}
			}
			requiredByAgent[ownerID] = bindings
		}
		for _, definition := range candidate.Agents {
			if definition == nil {
				continue
			}
			addOwner(definition.ID)
		}
		if candidate.Team != nil {
			addOwner(candidate.Team.ID)
		}
	}

	for agentID, references := range placement.CredentialReferences {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			return fmt.Errorf("credential placement requires a workforce owner definition ID")
		}
		allowedBindings, knownAgent := requiredByAgent[agentID]
		if candidate != nil && !knownAgent {
			return fmt.Errorf("credential placement owner %s is not part of this workforce candidate", agentID)
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
			requirement, requiredBinding := allowedBindings[key]
			if candidate != nil && !requiredBinding && !deploymentBindings[key] {
				expected := sortedCredentialRequirements(allowedBindings)
				if len(expected) == 0 {
					return fmt.Errorf("Agent %s does not require credential kind %s", agentID, key)
				}
				return fmt.Errorf("Agent %s credential key %s is not a required kind; expected %s", agentID, key, strings.Join(expected, ", "))
			}
			grant, available := authorized[key][kind+"\x00"+id]
			if !available {
				return fmt.Errorf("Agent %s credential reference for binding %s is unavailable or no longer authorized", agentID, key)
			}
			if requirement.Kind != "" && requirement.Kind != kind {
				return fmt.Errorf("Agent %s credential reference for binding %s must use kind %s", agentID, key, requirement.Kind)
			}
			if requirement.OAuth2 != nil && !capability.OAuth2GrantSatisfies(requirement.OAuth2, grant) {
				return fmt.Errorf("Agent %s OAuth 2 connection for binding %s does not grant the required provider, subject, resource, and scopes", agentID, key)
			}
		}
	}
	return nil
}

func sortedCredentialRequirements(values map[string]CredentialBindingRequirement) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
