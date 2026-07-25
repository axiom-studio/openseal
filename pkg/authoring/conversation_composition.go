package authoring

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

const conversationMessageReceivedEvent = "conversation.message.received"

func CanonicalRuntimeCompositionCapability() *RuntimeCompositionCapability {
	return &RuntimeCompositionCapability{
		ProtocolVersion: RuntimeCompositionProtocolV1,
		Conversation: ConversationRuntimeCapability{
			EventTypes: []string{conversationMessageReceivedEvent},
			HandlerKinds: []ConversationHandlerKind{
				ConversationHandlerAgent, ConversationHandlerRunbook, ConversationHandlerTeam,
			},
			ReplyModes: []ConversationReplyMode{
				ConversationReplyChannel, ConversationReplyProviderDefault, ConversationReplyThread,
			},
			CanonicalReply: true,
		},
		Runbook: RunbookRuntimeCapability{
			TriggerKinds: []runbook.TriggerKind{runbook.TriggerEvent},
			StepKinds: []runbook.StepKind{
				runbook.StepAction, runbook.StepDecision, runbook.StepDelegate, runbook.StepEnd,
				runbook.StepForEach, runbook.StepFork, runbook.StepJoin, runbook.StepLoopReturn,
				runbook.StepTransform, runbook.StepWait,
			},
			CanInvokeAgents: true,
		},
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
		!uniqueRunbookStepKinds(value.Runbook.StepKinds) {
		return errors.New("runtime composition capability is invalid or non-canonical")
	}
	return nil
}

func validateConversationEndpointBlueprints(candidate *WorkforceCandidate) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	agents := candidateAgentsByID(candidate)
	seen := map[string]bool{}
	issues := make([]ValidationIssue, 0)
	for index, endpoint := range candidate.ConversationEndpoints {
		path := fmt.Sprintf("conversationEndpoints[%d]", index)
		if endpoint.ID == "" || endpoint.ID != strings.TrimSpace(endpoint.ID) || len(endpoint.ID) > 128 || seen[endpoint.ID] {
			issues = append(issues, issue(path+".id", "invalid_conversation_endpoint", "Conversation endpoint id must be unique and 1-128 characters"))
		}
		seen[endpoint.ID] = true
		if strings.TrimSpace(endpoint.Name) == "" || len(endpoint.Name) > 160 ||
			strings.TrimSpace(endpoint.SkillID) == "" || strings.TrimSpace(endpoint.SkillVersion) == "" ||
			strings.TrimSpace(endpoint.AdapterID) == "" || strings.TrimSpace(endpoint.ArchitectureReason) == "" ||
			len(endpoint.ArchitectureReason) > 1024 || strings.ContainsAny(endpoint.ArchitectureReason, "\r\n") {
			issues = append(issues, issue(path, "invalid_conversation_endpoint", "Conversation endpoint requires a bounded name, exact Skill adapter, and architecture reason"))
		}
		if !endpoint.CanonicalReply {
			issues = append(issues, issue(path+".canonicalReply", "canonical_reply_required", "Reactive conversations must reply through the canonical Conversation outbox"))
		}
		if endpoint.Mode != capability.ConversationEndpointChannel && endpoint.Mode != capability.ConversationEndpointDirect {
			issues = append(issues, issue(path+".mode", "invalid_conversation_endpoint_mode", "Conversation endpoint mode must be channel or direct"))
		}
		if len(endpoint.Address) > 1024 || strings.ContainsAny(endpoint.Address, "\r\n") {
			issues = append(issues, issue(path+".address", "invalid_conversation_endpoint_address", "Conversation endpoint address is invalid"))
		}
		switch endpoint.Policy.MessageSelection {
		case ConversationSelectAllMessages, ConversationSelectMentions, ConversationSelectDirectOrMention:
		default:
			issues = append(issues, issue(path+".policy.messageSelection", "invalid_conversation_policy", "Conversation message selection is invalid"))
		}
		switch endpoint.Policy.ReplyMode {
		case ConversationReplyProviderDefault, ConversationReplyThread, ConversationReplyChannel:
		default:
			issues = append(issues, issue(path+".policy.replyMode", "invalid_conversation_policy", "Conversation reply mode is invalid"))
		}
		if endpoint.Mode == capability.ConversationEndpointDirect && endpoint.Policy.MessageSelection == ConversationSelectMentions {
			issues = append(issues, issue(path+".policy.messageSelection", "invalid_conversation_policy", "Direct endpoints cannot require mentions"))
		}
		switch endpoint.Owner.Type {
		case ConversationEndpointOwnerAgent:
			if agents[endpoint.Owner.ID] == nil {
				issues = append(issues, issue(path+".owner", "conversation_owner_missing", "Conversation endpoint owner must reference a candidate Agent"))
			}
		case ConversationEndpointOwnerTeam:
			if candidate.Team == nil || candidate.Team.ID != endpoint.Owner.ID {
				issues = append(issues, issue(path+".owner", "conversation_owner_missing", "Conversation endpoint owner must reference the candidate Team"))
			}
		default:
			issues = append(issues, issue(path+".owner.type", "invalid_conversation_owner", "Conversation endpoint owner type is invalid"))
		}
		issues = append(issues, validateConversationHandlerBlueprint(path+".handler", endpoint, agents)...)
	}
	return issues
}

func validateConversationHandlerBlueprint(
	path string,
	endpoint ConversationEndpointBlueprint,
	agents map[string]*agent.AgentDefinition,
) []ValidationIssue {
	handler := endpoint.Handler
	switch handler.Kind {
	case ConversationHandlerAgent:
		if endpoint.Owner.Type != ConversationEndpointOwnerAgent || handler.AgentDefinitionID != endpoint.Owner.ID ||
			handler.RunbookID != "" || handler.RunbookVersion != "" || handler.Trigger != "" {
			return []ValidationIssue{issue(path, "invalid_conversation_handler", "Direct Agent handler must identify the Agent endpoint owner")}
		}
	case ConversationHandlerTeam:
		if endpoint.Owner.Type != ConversationEndpointOwnerTeam || handler.AgentDefinitionID != "" ||
			handler.RunbookID != "" || handler.RunbookVersion != "" || handler.Trigger != "" {
			return []ValidationIssue{issue(path, "invalid_conversation_handler", "Direct Team handler must identify the Team endpoint owner")}
		}
	case ConversationHandlerRunbook:
		definition := agents[handler.AgentDefinitionID]
		if definition == nil || definition.Runbook == nil ||
			definition.Runbook.ID != handler.RunbookID || definition.Runbook.Version != handler.RunbookVersion {
			return []ValidationIssue{issue(path, "runbook_handler_unavailable", "Runbook handler must pin a candidate Agent's exact embedded Runbook")}
		}
		trigger, ok := definition.Runbook.Triggers[handler.Trigger]
		if !ok || trigger.Kind != runbook.TriggerEvent || trigger.EventType != conversationMessageReceivedEvent {
			return []ValidationIssue{issue(path+".trigger", "conversation_trigger_missing", "Runbook handler must expose a conversation.message.received event trigger")}
		}
		hasDelegation := false
		for _, step := range definition.Runbook.Steps {
			hasDelegation = hasDelegation || step.Kind == runbook.StepDelegate
		}
		if !hasDelegation {
			return []ValidationIssue{issue(path, "conversation_cognitive_handler_missing", "Chatbot Runbook must invoke a bounded Agent through a delegate step")}
		}
	default:
		return []ValidationIssue{issue(path+".kind", "invalid_conversation_handler", "Conversation handler kind is invalid")}
	}
	return nil
}

func validateConversationComposition(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := make([]ValidationIssue, 0)
	for index, endpoint := range candidate.ConversationEndpoints {
		path := fmt.Sprintf("conversationEndpoints[%d]", index)
		skillCapability, ok := request.Catalog.Skills[endpoint.SkillID]
		if !ok || skillCapability.Version != endpoint.SkillVersion {
			issues = append(issues, issue(path, "conversation_adapter_unavailable", "Conversation endpoint must select an exact authorized Skill version"))
			continue
		}
		var adapter *ConversationAdapterCapability
		for adapterIndex := range skillCapability.ConversationAdapters {
			if skillCapability.ConversationAdapters[adapterIndex].ID == endpoint.AdapterID {
				adapter = &skillCapability.ConversationAdapters[adapterIndex]
				break
			}
		}
		if adapter == nil || !containsConversationMode(adapter.EndpointModes, endpoint.Mode) ||
			!containsExactString(adapter.InboundEventTypes, capability.ConversationEventMessageReceived) ||
			!containsDeliveryOperation(adapter.Delivery.Operations, capability.ConversationDeliveryMessageSend) {
			issues = append(issues, issue(path, "conversation_adapter_incompatible", "Selected Skill adapter cannot receive and reply to this conversation endpoint"))
			continue
		}
		if endpoint.Policy.ReplyMode == ConversationReplyThread &&
			!containsConversationFeature(adapter.Features, capability.ConversationFeatureThreads) {
			issues = append(issues, issue(path+".policy.replyMode", "conversation_threads_unsupported", "Selected Skill adapter does not support threaded replies"))
		}
	}
	if !explicitReactiveConversationIntent(request.Prompt) {
		return issues
	}
	if request.Catalog.RuntimeComposition == nil {
		issues = append(issues, issue("conversationEndpoints", "runtime_composition_unavailable", "Reactive chatbot intent requires the authorized runtime composition catalog"))
		return issues
	}
	if len(candidate.ConversationEndpoints) == 0 {
		issues = append(issues, issue("conversationEndpoints", "reactive_conversation_endpoint_missing", "Prompt requires an executable external conversation endpoint, not a prose-only Objective"))
		return issues
	}
	if runtimeSupportsRunbookConversation(request.Catalog.RuntimeComposition) {
		hasRunbook := false
		for _, endpoint := range candidate.ConversationEndpoints {
			hasRunbook = hasRunbook || endpoint.Handler.Kind == ConversationHandlerRunbook
		}
		if !hasRunbook {
			issues = append(issues, issue("conversationEndpoints", "reactive_conversation_runbook_missing", "Reactive chatbot intent should use a conversation event Runbook with bounded Agent invocation"))
		}
	}
	return issues
}

func explicitReactiveConversationIntent(prompt string) bool {
	tokens := compositionTokens(prompt)
	hasConversation := containsAnyToken(tokens, "message", "messages", "channel", "chat", "chatbot")
	hasIngress := containsAnyToken(tokens, "listen", "listens", "listening", "receive", "receives", "watch", "watches")
	hasResponse := containsAnyToken(tokens, "respond", "responds", "reply", "replies", "answer", "answers")
	return hasConversation && hasIngress && hasResponse || containsAnyToken(tokens, "chatbot") && hasResponse
}

func runtimeSupportsRunbookConversation(value *RuntimeCompositionCapability) bool {
	if value == nil || !value.Runbook.CanInvokeAgents ||
		!containsRunbookTriggerKind(value.Runbook.TriggerKinds, runbook.TriggerEvent) ||
		!containsExactString(value.Conversation.EventTypes, conversationMessageReceivedEvent) ||
		!containsConversationHandlerKind(value.Conversation.HandlerKinds, ConversationHandlerRunbook) {
		return false
	}
	return true
}

func compositionTokens(value string) []string {
	result := []string{}
	var current []rune
	flush := func() {
		if len(current) != 0 {
			result = append(result, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			current = append(current, character)
		} else {
			flush()
		}
	}
	flush()
	return result
}

func containsAnyToken(tokens []string, wanted ...string) bool {
	values := map[string]bool{}
	for _, value := range wanted {
		values[value] = true
	}
	for _, token := range tokens {
		if values[token] {
			return true
		}
	}
	return false
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
	return uniqueEnumStrings(stringsValue, []string{string(runbook.TriggerEvent)})
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

func containsConversationMode(values []capability.ConversationEndpointMode, wanted capability.ConversationEndpointMode) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsExactString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsDeliveryOperation(values []capability.ConversationDeliveryOperation, wanted capability.ConversationDeliveryOperation) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsConversationFeature(values []capability.ConversationAdapterFeature, wanted capability.ConversationAdapterFeature) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsRunbookTriggerKind(values []runbook.TriggerKind, wanted runbook.TriggerKind) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsConversationHandlerKind(values []ConversationHandlerKind, wanted ConversationHandlerKind) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
