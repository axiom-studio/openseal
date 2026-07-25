package capability

import (
	"errors"
	"sort"
	"strings"
)

// NormalizeConversationAdapter validates and canonicalizes one Skill-owned
// provider adapter declaration. Executable entrypoints remain opaque to the
// kernel and are dispatched only by a generic governed host.
func NormalizeConversationAdapter(value ConversationAdapter) (ConversationAdapter, error) {
	value.Name = strings.TrimSpace(value.Name)
	value.Description = strings.TrimSpace(value.Description)
	value.Provider = strings.TrimSpace(value.Provider)
	value.Transport.Kind = strings.TrimSpace(value.Transport.Kind)
	value.Transport.IngressEndpoint = strings.TrimSpace(value.Transport.IngressEndpoint)
	value.Transport.DeliveryEndpoint = strings.TrimSpace(value.Transport.DeliveryEndpoint)
	value.ProtocolVersion = strings.TrimSpace(value.ProtocolVersion)
	if value.ProtocolVersion != ConversationAdapterProtocolV1 {
		return ConversationAdapter{}, errors.New("conversation adapter protocol version is unsupported")
	}
	if value.Name == "" || len(value.Name) > 160 || value.Description == "" || len(value.Description) > 1024 ||
		!validOAuth2Provider(value.Provider) {
		return ConversationAdapter{}, errors.New("conversation adapter requires a bounded name, description, and stable provider")
	}
	if !validConversationAdapterIdentifier(value.Transport.Kind, 128) ||
		!validConversationAdapterIdentifier(value.Transport.IngressEndpoint, 256) ||
		!validConversationAdapterIdentifier(value.Transport.DeliveryEndpoint, 256) {
		return ConversationAdapter{}, errors.New("conversation adapter transport and entrypoints are invalid")
	}
	var err error
	value.EndpointModes, err = normalizeConversationEndpointModes(value.EndpointModes)
	if err != nil {
		return ConversationAdapter{}, err
	}
	value.InboundEventTypes, err = normalizeConversationEventTypes(value.InboundEventTypes)
	if err != nil {
		return ConversationAdapter{}, err
	}
	value.Features, err = normalizeConversationFeatures(value.Features)
	if err != nil {
		return ConversationAdapter{}, err
	}
	value.Delivery, err = normalizeConversationDeliveryCapabilities(value.Delivery)
	if err != nil {
		return ConversationAdapter{}, err
	}
	credentials := make([]CredentialRequirement, len(value.Credentials))
	seenCredentials := make(map[string]struct{}, len(value.Credentials))
	for index, credential := range value.Credentials {
		credential.Name, credential.Kind = strings.TrimSpace(credential.Name), strings.TrimSpace(credential.Kind)
		if !validConversationAdapterIdentifier(credential.Name, 128) || !validConversationAdapterIdentifier(credential.Kind, 128) {
			return ConversationAdapter{}, errors.New("conversation adapter credential name or kind is invalid")
		}
		if _, exists := seenCredentials[credential.Name]; exists {
			return ConversationAdapter{}, errors.New("conversation adapter credential names must be unique")
		}
		seenCredentials[credential.Name] = struct{}{}
		credential.OAuth2, err = NormalizeOAuth2Requirement(credential.OAuth2)
		if err != nil {
			return ConversationAdapter{}, err
		}
		credentials[index] = credential
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].Name < credentials[j].Name })
	value.Credentials = credentials
	value.Transport.IngressCredentials, err = normalizeConversationCredentialSelection(
		value.Transport.IngressCredentials, seenCredentials, "ingress",
	)
	if err != nil {
		return ConversationAdapter{}, err
	}
	value.Transport.DeliveryCredentials, err = normalizeConversationCredentialSelection(
		value.Transport.DeliveryCredentials, seenCredentials, "delivery",
	)
	if err != nil {
		return ConversationAdapter{}, err
	}
	usedCredentials := make(map[string]struct{}, len(credentials))
	for _, name := range value.Transport.IngressCredentials {
		usedCredentials[name] = struct{}{}
	}
	for _, name := range value.Transport.DeliveryCredentials {
		usedCredentials[name] = struct{}{}
	}
	if len(usedCredentials) != len(credentials) {
		return ConversationAdapter{}, errors.New("conversation adapter credentials must declare ingress or delivery use")
	}
	return value, nil
}

func normalizeConversationCredentialSelection(
	values []string,
	available map[string]struct{},
	purpose string,
) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > len(available) {
		return nil, errors.New("conversation adapter " + purpose + " credential selection is invalid")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, exists := available[value]; !exists || value == "" {
			return nil, errors.New("conversation adapter " + purpose + " credential is not declared")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeConversationDeliveryCapabilities(value ConversationDeliveryCapabilities) (ConversationDeliveryCapabilities, error) {
	if len(value.Operations) == 0 || len(value.Operations) > 8 {
		return ConversationDeliveryCapabilities{}, errors.New("conversation adapter requires delivery operations")
	}
	seen := make(map[ConversationDeliveryOperation]struct{}, len(value.Operations))
	operations := make([]ConversationDeliveryOperation, 0, len(value.Operations))
	hasMessageSend := false
	for _, operation := range value.Operations {
		switch operation {
		case ConversationDeliveryMessageSend, ConversationDeliveryMessageUpdate, ConversationDeliveryMessageDelete,
			ConversationDeliveryReactionAdd, ConversationDeliveryReactionRemove, ConversationDeliveryTypingIndicator:
		default:
			return ConversationDeliveryCapabilities{}, errors.New("conversation adapter delivery operation is invalid")
		}
		if _, exists := seen[operation]; exists {
			continue
		}
		seen[operation] = struct{}{}
		hasMessageSend = hasMessageSend || operation == ConversationDeliveryMessageSend
		operations = append(operations, operation)
	}
	if !hasMessageSend {
		return ConversationDeliveryCapabilities{}, errors.New("conversation adapter must support message delivery")
	}
	switch value.Ordering {
	case ConversationDeliveryOrderEndpoint, ConversationDeliveryOrderConversation, ConversationDeliveryOrderThread:
	default:
		return ConversationDeliveryCapabilities{}, errors.New("conversation adapter delivery ordering is invalid")
	}
	if value.Idempotency != IdempotencySupported && value.Idempotency != IdempotencyRequired {
		return ConversationDeliveryCapabilities{}, errors.New("conversation adapter delivery must support idempotency")
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i] < operations[j] })
	value.Operations = operations
	return value, nil
}

func normalizeConversationEndpointModes(values []ConversationEndpointMode) ([]ConversationEndpointMode, error) {
	if len(values) == 0 || len(values) > 2 {
		return nil, errors.New("conversation adapter requires at least one endpoint mode")
	}
	seen := make(map[ConversationEndpointMode]struct{}, len(values))
	result := make([]ConversationEndpointMode, 0, len(values))
	for _, value := range values {
		switch value {
		case ConversationEndpointChannel, ConversationEndpointDirect:
		default:
			return nil, errors.New("conversation adapter endpoint mode is invalid")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func normalizeConversationEventTypes(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 16 {
		return nil, errors.New("conversation adapter requires inbound event types")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		switch value {
		case ConversationEventMessageReceived, ConversationEventMessageUpdated, ConversationEventMessageDeleted,
			ConversationEventReactionAdded, ConversationEventReactionRemoved,
			ConversationEventParticipantJoined, ConversationEventParticipantLeft:
		default:
			return nil, errors.New("conversation adapter inbound event type is invalid")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeConversationFeatures(values []ConversationAdapterFeature) ([]ConversationAdapterFeature, error) {
	if len(values) > 16 {
		return nil, errors.New("conversation adapter has too many features")
	}
	seen := make(map[ConversationAdapterFeature]struct{}, len(values))
	result := make([]ConversationAdapterFeature, 0, len(values))
	for _, value := range values {
		switch value {
		case ConversationFeatureThreads, ConversationFeatureMentions, ConversationFeatureAttachments,
			ConversationFeatureReactions, ConversationFeatureEdits, ConversationFeatureDeletes, ConversationFeatureTyping:
		default:
			return nil, errors.New("conversation adapter feature is invalid")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func validConversationAdapterIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:/@+-", character) {
			continue
		}
		return false
	}
	return true
}
