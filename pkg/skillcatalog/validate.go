package skillcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

var (
	catalogDiagnosticCodePattern      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	catalogDiagnosticReferencePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,255}$`)
	authorityConstraintVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)
	credentialBindingKeyPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// ValidateCapabilityCatalog rejects malformed host projections before they are
// persisted or placed in model context.
func ValidateCapabilityCatalog(catalog CapabilityCatalog) error {
	if err := ValidateRuntimeCompositionCapability(catalog.RuntimeComposition); err != nil {
		return fmt.Errorf("runtime composition capability: %w", err)
	}
	if hosted := catalog.HostedExecution; hosted != nil {
		if hosted.ProtocolVersion != HostedExecutionProtocolV1 || hosted.BaseInputTokens < 1 || hosted.MinimumOutputTokens < 1 {
			return errors.New("hosted execution capability is invalid")
		}
	}
	if constraint := catalog.AuthorityConstraint; constraint != nil {
		if constraint.ID != strings.TrimSpace(constraint.ID) || constraint.Version != strings.TrimSpace(constraint.Version) ||
			!catalogDiagnosticReferencePattern.MatchString(constraint.ID) || !authorityConstraintVersionPattern.MatchString(constraint.Version) {
			return errors.New("authority constraint requires a valid id and version")
		}
		if constraint.MaximumRisk != "" && riskRank(constraint.MaximumRisk) < 0 {
			return errors.New("authority constraint maximum risk is invalid")
		}
		if constraint.RequireApprovalAt != "" && riskRank(constraint.RequireApprovalAt) < 0 {
			return errors.New("authority constraint approval risk is invalid")
		}
		if constraint.MaximumRisk != "" && constraint.RequireApprovalAt != "" && riskRank(constraint.RequireApprovalAt) > riskRank(constraint.MaximumRisk) {
			return errors.New("authority constraint approval risk cannot exceed maximum risk")
		}
	}
	for id, skill := range catalog.Skills {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(skill.ID) == "" || id != skill.ID {
			return errors.New("Skill catalog keys must match non-empty Skill ids")
		}
		if skill.MaximumRisk != "" && riskRank(skill.MaximumRisk) < 0 {
			return fmt.Errorf("Skill %s maximum risk is invalid", id)
		}
		if skill.HostedModelInputTokens < 0 {
			return fmt.Errorf("Skill %s hosted model input ceiling is invalid", id)
		}
		actions := stringSet(skill.Actions)
		for action, risk := range skill.ActionRisks {
			if action != strings.TrimSpace(action) || action == "" || !actions[action] {
				return fmt.Errorf("Skill %s action risk references an undeclared action", id)
			}
			if riskRank(risk) < 0 {
				return fmt.Errorf("Skill %s action %s risk is invalid", id, action)
			}
			if skill.MaximumRisk != "" && riskRank(risk) > riskRank(skill.MaximumRisk) {
				return fmt.Errorf("Skill %s action %s risk exceeds the Skill maximum", id, action)
			}
		}
		if len(strings.TrimSpace(skill.SourceIdentity)) > 1024 {
			return fmt.Errorf("Skill %s source identity is too long", id)
		}
		if skill.RuntimeIdentity != nil {
			identity := skill.RuntimeIdentity.Normalized()
			if !identity.Valid() || identity != *skill.RuntimeIdentity {
				return fmt.Errorf("Skill %s runtime identity is invalid or non-canonical", id)
			}
			if source := strings.TrimSpace(skill.SourceIdentity); source != "" && identity.SourceIdentity != source {
				return fmt.Errorf("Skill %s runtime identity conflicts with its source identity", id)
			}
		}
		switch skill.Readiness {
		case "", SkillReadinessReady, SkillReadinessNeedsBinding, SkillReadinessNeedsInstallation, SkillReadinessUnavailable:
		default:
			return fmt.Errorf("Skill %s has invalid readiness", id)
		}
		for _, evidence := range skill.Compatibility {
			if strings.TrimSpace(evidence.Requirement) == "" || strings.TrimSpace(evidence.Evidence) == "" {
				return fmt.Errorf("Skill %s compatibility requires a requirement and evidence", id)
			}
		}
		type credentialContract struct {
			kind    string
			oauth2  *capability.OAuth2Requirement
			actions map[string]bool
			all     bool
		}
		credentialNames := make(map[string]credentialContract, len(skill.Credentials))
		for index, credential := range skill.Credentials {
			if credential.Name != strings.TrimSpace(credential.Name) || credential.Kind != strings.TrimSpace(credential.Kind) ||
				credential.Name == "" || credential.Kind == "" || len(credential.Name) > 128 || len(credential.Kind) > 128 {
				return fmt.Errorf("Skill %s credential %d is invalid", id, index)
			}
			normalized, err := capability.NormalizeOAuth2Requirement(credential.OAuth2)
			if err != nil || !reflect.DeepEqual(normalized, credential.OAuth2) {
				return fmt.Errorf("Skill %s credential %d OAuth 2 requirement is invalid or non-canonical", id, index)
			}
			selectedActions := make(map[string]bool, len(credential.Actions))
			for _, action := range credential.Actions {
				if action != strings.TrimSpace(action) || !actions[action] || selectedActions[action] {
					return fmt.Errorf("Skill %s credential %d references an undeclared action", id, index)
				}
				selectedActions[action] = true
			}
			if existing, ok := credentialNames[credential.Name]; ok {
				if existing.kind != credential.Kind || !sameOAuth2Authorization(existing.oauth2, credential.OAuth2) ||
					existing.all || len(selectedActions) == 0 {
					return fmt.Errorf("Skill %s credential %s has conflicting action-scoped declarations", id, credential.Name)
				}
				for action := range selectedActions {
					if existing.actions[action] {
						return fmt.Errorf("Skill %s credential %s repeats action %s", id, credential.Name, action)
					}
					existing.actions[action] = true
				}
				credentialNames[credential.Name] = existing
			} else {
				credentialNames[credential.Name] = credentialContract{
					kind: credential.Kind, oauth2: credential.OAuth2,
					actions: selectedActions, all: len(selectedActions) == 0,
				}
			}
		}
		adapterIDs := make(map[string]bool, len(skill.ConversationAdapters))
		for index, adapter := range skill.ConversationAdapters {
			if adapter.ID == "" || adapter.ID != strings.TrimSpace(adapter.ID) || len(adapter.ID) > 128 || adapterIDs[adapter.ID] {
				return fmt.Errorf("Skill %s conversation adapter %d has an invalid or duplicate id", id, index)
			}
			normalized, err := capability.NormalizeConversationAdapter(capability.ConversationAdapter{
				ProtocolVersion: adapter.ProtocolVersion,
				Name:            adapter.ID, Description: adapter.ID, Provider: adapter.Provider,
				EndpointModes: adapter.EndpointModes, InboundEventTypes: adapter.InboundEventTypes, Features: adapter.Features,
				Delivery: adapter.Delivery,
				Transport: capability.ConversationAdapterTransport{
					Kind: "authoring", IngressEndpoint: "authoring", DeliveryEndpoint: "authoring",
				},
			})
			if err != nil || normalized.Provider != adapter.Provider ||
				!reflect.DeepEqual(normalized.EndpointModes, adapter.EndpointModes) ||
				!reflect.DeepEqual(normalized.InboundEventTypes, adapter.InboundEventTypes) ||
				!reflect.DeepEqual(normalized.Features, adapter.Features) {
				return fmt.Errorf("Skill %s conversation adapter %d is invalid or non-canonical", id, index)
			}
			endpointModes := make(map[capability.ConversationEndpointMode]bool, len(adapter.EndpointModes))
			for _, mode := range adapter.EndpointModes {
				endpointModes[mode] = true
			}
			seenDiscoverableModes := make(map[capability.ConversationEndpointMode]bool, len(adapter.DiscoverableDestinationModes))
			for modeIndex, mode := range adapter.DiscoverableDestinationModes {
				if !endpointModes[mode] || seenDiscoverableModes[mode] {
					return fmt.Errorf("Skill %s conversation adapter %d has invalid discoverable destination modes", id, index)
				}
				if modeIndex > 0 && adapter.DiscoverableDestinationModes[modeIndex-1] > mode {
					return fmt.Errorf("Skill %s conversation adapter %d discoverable destination modes are not canonical", id, index)
				}
				seenDiscoverableModes[mode] = true
			}
			seenAdapterCredentials := make(map[string]bool, len(adapter.Credentials))
			for credentialIndex, credential := range adapter.Credentials {
				if credential.Name == "" || credential.Name != strings.TrimSpace(credential.Name) ||
					credential.Kind == "" || credential.Kind != strings.TrimSpace(credential.Kind) ||
					len(credential.Name) > 128 || len(credential.Kind) > 128 || seenAdapterCredentials[credential.Name] {
					return fmt.Errorf("Skill %s conversation adapter %d credential %d is invalid", id, index, credentialIndex)
				}
				normalizedOAuth2, oauthErr := capability.NormalizeOAuth2Requirement(credential.OAuth2)
				if oauthErr != nil || !reflect.DeepEqual(normalizedOAuth2, credential.OAuth2) {
					return fmt.Errorf("Skill %s conversation adapter %d credential %d OAuth 2 requirement is invalid or non-canonical", id, index, credentialIndex)
				}
				seenAdapterCredentials[credential.Name] = true
			}
			adapterIDs[adapter.ID] = true
		}
		callbackAdapterIDs := make(map[string]bool, len(skill.CallbackAdapters))
		for index, adapter := range skill.CallbackAdapters {
			if adapter.ID == "" || adapter.ID != strings.TrimSpace(adapter.ID) || len(adapter.ID) > 128 || callbackAdapterIDs[adapter.ID] {
				return fmt.Errorf("Skill %s callback adapter %d has an invalid or duplicate id", id, index)
			}
			credentials := make([]capability.CredentialRequirement, len(adapter.Credentials))
			for credentialIndex, credential := range adapter.Credentials {
				credentials[credentialIndex] = capability.CredentialRequirement{
					Name: credential.Name, Kind: credential.Kind, OAuth2: credential.OAuth2,
				}
			}
			normalized, err := capability.NormalizeCallbackAdapter(capability.CallbackAdapter{
				ProtocolVersion: adapter.ProtocolVersion, Name: adapter.ID, Description: adapter.ID,
				Provider: adapter.Provider, EventTypes: adapter.EventTypes, Credentials: credentials,
				Transport: capability.CallbackAdapterTransport{
					Kind: "authoring", IngressEndpoint: "authoring",
					IngressCredentials: callbackCredentialNames(credentials),
				},
			})
			if err != nil || normalized.Provider != adapter.Provider ||
				!reflect.DeepEqual(normalized.EventTypes, adapter.EventTypes) || len(normalized.Credentials) != len(adapter.Credentials) {
				return fmt.Errorf("Skill %s callback adapter %d is invalid or non-canonical", id, index)
			}
			callbackAdapterIDs[adapter.ID] = true
		}
	}
	for bindingKey, grants := range catalog.AvailableCredentialGrants {
		if bindingKey != strings.TrimSpace(bindingKey) || bindingKey == "" || len(bindingKey) > 128 || len(grants) > 32 {
			return fmt.Errorf("available OAuth 2 grants for credential %q are invalid", bindingKey)
		}
		seen := make(map[string]bool, len(grants))
		for index := range grants {
			normalized, err := capability.NormalizeOAuth2GrantSummary(&grants[index])
			if err != nil || !reflect.DeepEqual(normalized, &grants[index]) {
				return fmt.Errorf("available OAuth 2 grant %d for credential %s is invalid or non-canonical", index, bindingKey)
			}
			encoded, _ := json.Marshal(normalized)
			if seen[string(encoded)] {
				return fmt.Errorf("available OAuth 2 grants for credential %s contain a duplicate", bindingKey)
			}
			seen[string(encoded)] = true
		}
	}
	credentialKeys := make(map[string]bool, len(catalog.AgentCredentialRequirements))
	for index, requirement := range catalog.AgentCredentialRequirements {
		key := strings.TrimSpace(requirement.BindingKey)
		name := strings.TrimSpace(requirement.DisplayName)
		prompt := strings.TrimSpace(requirement.Prompt)
		if key != requirement.BindingKey || name != requirement.DisplayName || prompt != requirement.Prompt ||
			!credentialBindingKeyPattern.MatchString(key) || name == "" || prompt == "" ||
			len(name) > 200 || len(prompt) > 1024 || strings.ContainsAny(name+prompt, "\r\n\t") ||
			credentialKeys[key] {
			return fmt.Errorf("Agent credential requirement %d is invalid", index)
		}
		credentialKeys[key] = true
	}
	if len(catalog.CapabilityNeeds) > MaximumCapabilityNeeds {
		return fmt.Errorf("capability catalog has more than %d capability needs", MaximumCapabilityNeeds)
	}
	needIDs := make(map[string]bool, len(catalog.CapabilityNeeds))
	for index, need := range catalog.CapabilityNeeds {
		need.ID, need.Prompt, need.WhyNeeded = strings.TrimSpace(need.ID), strings.TrimSpace(need.Prompt), strings.TrimSpace(need.WhyNeeded)
		if !catalogDiagnosticReferencePattern.MatchString(need.ID) || need.Prompt == "" || need.WhyNeeded == "" ||
			len(need.Prompt) > 1024 || len(need.WhyNeeded) > 1024 || strings.ContainsAny(need.Prompt+need.WhyNeeded, "\r\n\t") ||
			need.Priority < 1 || need.Priority > 1000 {
			return fmt.Errorf("capability catalog need %d is invalid", index)
		}
		if scope := need.SourceScope; scope != nil {
			scope.Prompt, scope.WhyNeeded = strings.TrimSpace(scope.Prompt), strings.TrimSpace(scope.WhyNeeded)
			if scope.Prompt == "" || scope.WhyNeeded == "" || len(scope.Prompt) > 1024 || len(scope.WhyNeeded) > 1024 ||
				strings.ContainsAny(scope.Prompt+scope.WhyNeeded, "\r\n\t") || scope.Minimum < 1 || scope.Maximum < scope.Minimum ||
				scope.Maximum > 100 || scope.Priority < 1 || scope.Priority > 1000 {
				return fmt.Errorf("capability catalog need %d source scope is invalid", index)
			}
			seenInputKeys := make(map[string]bool, len(scope.MaterializationInputKeys))
			seenTargets := make(map[string]bool, len(scope.Targets))
			for _, target := range scope.Targets {
				if target != strings.TrimSpace(target) || target == "" || len(target) > 2048 || strings.ContainsAny(target, "\r\n\t") || seenTargets[target] {
					return fmt.Errorf("capability catalog need %d source scope has invalid targets", index)
				}
				seenTargets[target] = true
			}
			if len(scope.Targets) > 0 && (len(scope.Targets) < scope.Minimum || len(scope.Targets) > scope.Maximum) {
				return fmt.Errorf("capability catalog need %d source scope targets are outside its bounds", index)
			}
			for _, key := range scope.MaterializationInputKeys {
				if key != strings.TrimSpace(key) || !catalogDiagnosticCodePattern.MatchString(key) || seenInputKeys[key] {
					return fmt.Errorf("capability catalog need %d source scope has invalid materialization input keys", index)
				}
				seenInputKeys[key] = true
			}
		}
		if proposal := need.SourcePolicyProposal; proposal != nil {
			proposal.Reason = strings.TrimSpace(proposal.Reason)
			if proposal.Reason == "" || len(proposal.Reason) > 1024 || strings.ContainsAny(proposal.Reason, "\r\n\t") {
				return fmt.Errorf("capability catalog need %d source policy proposal reason is invalid", index)
			}
			if err := proposal.Policy.Validate(); err != nil {
				return fmt.Errorf("capability catalog need %d source policy proposal is invalid: %w", index, err)
			}
			if !proposal.Policy.Enabled {
				return fmt.Errorf("capability catalog need %d source policy proposal must describe an enabled immutable version", index)
			}
			for _, policySource := range proposal.Policy.Sources {
				if len(policySource.PathPrefixes) == 0 || len(policySource.Methods) == 0 {
					return fmt.Errorf("capability catalog need %d source policy proposal requires explicit paths and methods", index)
				}
			}
			allowedSkills := stringSet(need.SkillIDs)
			seenProposalSkills := make(map[string]bool, len(proposal.SkillIDs))
			if len(proposal.SkillIDs) == 0 {
				return fmt.Errorf("capability catalog need %d source policy proposal requires at least one applicable Skill", index)
			}
			for _, skillID := range proposal.SkillIDs {
				if skillID != strings.TrimSpace(skillID) || !allowedSkills[skillID] || seenProposalSkills[skillID] {
					return fmt.Errorf("capability catalog need %d source policy proposal has an invalid applicable Skill", index)
				}
				seenProposalSkills[skillID] = true
			}
		}
		if needIDs[need.ID] {
			return fmt.Errorf("capability catalog need %d duplicates id %s", index, need.ID)
		}
		needIDs[need.ID] = true
		if len(need.SkillIDs) == 0 || len(need.SkillIDs) > MaximumCapabilityNeedSkillChoices {
			return fmt.Errorf("capability catalog need %s must contain between 1 and %d Skill choices", need.ID, MaximumCapabilityNeedSkillChoices)
		}
		seenSkills := make(map[string]bool, len(need.SkillIDs))
		for _, skillID := range need.SkillIDs {
			if skillID != strings.TrimSpace(skillID) || skillID == "" || seenSkills[skillID] {
				return fmt.Errorf("capability catalog need %s contains an invalid or duplicate Skill id", need.ID)
			}
			seenSkills[skillID] = true
			skill, exists := catalog.Skills[skillID]
			if !exists || !skillChoiceViable(skill) {
				return fmt.Errorf("capability catalog need %s references unavailable or incompatible Skill %s", need.ID, skillID)
			}
		}
	}
	if len(catalog.Diagnostics) > MaximumCatalogDiagnostics {
		return fmt.Errorf("capability catalog has more than %d diagnostics", MaximumCatalogDiagnostics)
	}
	seenDiagnostics := make(map[string]bool, len(catalog.Diagnostics))
	for index, diagnostic := range catalog.Diagnostics {
		code, message, reference := strings.TrimSpace(diagnostic.Code), strings.TrimSpace(diagnostic.Message), strings.TrimSpace(diagnostic.Reference)
		if code != diagnostic.Code || message != diagnostic.Message || reference != diagnostic.Reference ||
			!catalogDiagnosticCodePattern.MatchString(code) || message == "" || len(message) > 1024 || strings.ContainsAny(message, "\r\n\t") ||
			(reference != "" && !catalogDiagnosticReferencePattern.MatchString(reference)) {
			return fmt.Errorf("capability catalog diagnostic %d is invalid", index)
		}
		identity := code + "\x00" + reference
		if seenDiagnostics[identity] {
			return fmt.Errorf("capability catalog diagnostic %d duplicates code and reference", index)
		}
		seenDiagnostics[identity] = true
	}
	return nil
}

func callbackCredentialNames(values []capability.CredentialRequirement) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].Name
	}
	sort.Strings(result)
	return result
}

func sameOAuth2Authorization(left, right *capability.OAuth2Requirement) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Provider == right.Provider && left.Subject == right.Subject && left.Resource == right.Resource
}

func skillChoiceViable(skill SkillCapability) bool {
	if skill.Readiness == SkillReadinessUnavailable {
		return false
	}
	for _, evidence := range skill.Compatibility {
		if !evidence.Compatible && !lifecycleGap(skill.Readiness, evidence.Requirement) {
			return false
		}
	}
	return true
}

// Lifecycle gaps are not compatibility failures: they are the explicit work a
// refinement is proposing. Keep this allowlist narrow so action, platform,
// source, compilation, and other semantic incompatibilities still fail closed.
func lifecycleGap(readiness SkillReadiness, requirement string) bool {
	requirement = strings.TrimSpace(requirement)
	if requirement == "installation" {
		return readiness == SkillReadinessNeedsInstallation
	}
	if requirement == "binding_configuration" || strings.HasPrefix(requirement, "credential:") {
		return readiness == SkillReadinessNeedsBinding || readiness == SkillReadinessNeedsInstallation
	}
	return false
}

// CanonicalConversationTriggerInputSchema is the stable credential-free
// Runbook boundary emitted by the external conversation dispatcher.
func CanonicalConversationTriggerInputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"conversationId":   map[string]interface{}{"type": "string"},
			"triggerMessageId": map[string]interface{}{"type": "string"},
			"endpointId":       map[string]interface{}{"type": "string"},
			"event": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"id":         map[string]interface{}{"type": "string"},
					"type":       map[string]interface{}{"type": "string"},
					"source":     map[string]interface{}{"type": "string"},
					"subject":    map[string]interface{}{"type": "string"},
					"occurredAt": map[string]interface{}{"type": "string"},
					"attributes": map[string]interface{}{"type": "object"},
					"payload":    map[string]interface{}{"type": "object"},
				},
				"required": []interface{}{"id", "type", "source", "subject", "occurredAt", "attributes", "payload"},
			},
		},
		"required": []interface{}{"conversationId", "triggerMessageId", "endpointId", "event"},
	}
}

func ValidateRuntimeCompositionCapability(value *RuntimeCompositionCapability) error {
	if value == nil {
		return nil
	}
	if value.ProtocolVersion != RuntimeCompositionProtocolV1 {
		return errors.New("runtime composition protocol version is unsupported")
	}
	if !value.Conversation.CanonicalReply ||
		!uniqueConversationEventTypes(value.Conversation.EventTypes) ||
		!uniqueConversationHandlerKinds(value.Conversation.HandlerKinds) ||
		!uniqueConversationReplyModes(value.Conversation.ReplyModes) ||
		!uniqueRunbookTriggerKinds(value.Runbook.TriggerKinds) ||
		!uniqueRunbookStepKinds(value.Runbook.StepKinds) ||
		!reflect.DeepEqual(value.Runbook.ConversationTriggerInputSchema, CanonicalConversationTriggerInputSchema()) {
		return errors.New("runtime composition capability is invalid or non-canonical")
	}
	return nil
}

func uniqueConversationEventTypes(values []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return sort.StringsAreSorted(values)
}

func uniqueConversationHandlerKinds(values []ConversationHandlerKind) bool {
	return uniqueEnumStrings(conversationHandlerStrings(values), []string{
		string(ConversationHandlerAgent), string(ConversationHandlerRunbook), string(ConversationHandlerTeam),
	})
}

func uniqueConversationReplyModes(values []ConversationReplyMode) bool {
	return uniqueEnumStrings(conversationReplyStrings(values), []string{
		string(ConversationReplyChannel), string(ConversationReplyProviderDefault), string(ConversationReplyThread),
	})
}

func uniqueRunbookTriggerKinds(values []runbook.TriggerKind) bool {
	if len(values) == 0 {
		return false
	}
	stringsValue := make([]string, len(values))
	for index, value := range values {
		stringsValue[index] = string(value)
	}
	return uniqueEnumStrings(stringsValue, []string{string(runbook.TriggerEvent), string(runbook.TriggerSchedule)})
}

func uniqueRunbookStepKinds(values []runbook.StepKind) bool {
	if len(values) == 0 {
		return false
	}
	allowed := map[runbook.StepKind]bool{
		runbook.StepAction: true, runbook.StepDelegate: true, runbook.StepDecision: true,
		runbook.StepTransform: true, runbook.StepWait: true, runbook.StepFork: true,
		runbook.StepJoin: true, runbook.StepForEach: true, runbook.StepLoopReturn: true, runbook.StepEnd: true,
	}
	seen := map[runbook.StepKind]bool{}
	previous := ""
	for _, value := range values {
		if !allowed[value] || seen[value] || previous > string(value) {
			return false
		}
		seen[value] = true
		previous = string(value)
	}
	return true
}

func uniqueEnumStrings(values, allowedValues []string) bool {
	if len(values) == 0 {
		return false
	}
	allowed := map[string]bool{}
	for _, value := range allowedValues {
		allowed[value] = true
	}
	seen := map[string]bool{}
	previous := ""
	for _, value := range values {
		if !allowed[value] || seen[value] || previous > value {
			return false
		}
		seen[value] = true
		previous = value
	}
	return true
}

func conversationHandlerStrings(values []ConversationHandlerKind) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func conversationReplyStrings(values []ConversationReplyMode) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = true
		}
	}
	return result
}

func riskRank(value capability.RiskLevel) int {
	switch value {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}
