package capability

import (
	"errors"
	"sort"
	"strings"
)

// NormalizeCallbackAdapter validates and canonicalizes a Skill-owned inbound
// callback verifier. Provider payloads remain opaque until this adapter has
// authenticated and normalized them.
func NormalizeCallbackAdapter(value CallbackAdapter) (CallbackAdapter, error) {
	value.ProtocolVersion = strings.TrimSpace(value.ProtocolVersion)
	value.Name = strings.TrimSpace(value.Name)
	value.Description = strings.TrimSpace(value.Description)
	value.Provider = strings.TrimSpace(value.Provider)
	value.Transport.Kind = strings.TrimSpace(value.Transport.Kind)
	value.Transport.IngressEndpoint = strings.TrimSpace(value.Transport.IngressEndpoint)
	if value.Transport.Connection != nil {
		connection := *value.Transport.Connection
		connection.Kind = strings.TrimSpace(connection.Kind)
		connection.Endpoint = strings.TrimSpace(connection.Endpoint)
		connection.SharedByCredential = strings.TrimSpace(connection.SharedByCredential)
		value.Transport.Connection = &connection
	}
	if value.ProtocolVersion != CallbackAdapterProtocolV1 {
		return CallbackAdapter{}, errors.New("callback adapter protocol version is unsupported")
	}
	if value.Name == "" || len(value.Name) > 160 || value.Description == "" || len(value.Description) > 1024 ||
		!validOAuth2Provider(value.Provider) {
		return CallbackAdapter{}, errors.New("callback adapter requires a bounded name, description, and stable provider")
	}
	if !validConversationAdapterIdentifier(value.Transport.Kind, 128) ||
		!validConversationAdapterIdentifier(value.Transport.IngressEndpoint, 256) {
		return CallbackAdapter{}, errors.New("callback adapter transport and entrypoint are invalid")
	}
	if len(value.EventTypes) == 0 || len(value.EventTypes) > 64 {
		return CallbackAdapter{}, errors.New("callback adapter requires bounded event types")
	}
	eventTypes := make([]string, 0, len(value.EventTypes))
	seenEvents := make(map[string]struct{}, len(value.EventTypes))
	for _, eventType := range value.EventTypes {
		eventType = strings.TrimSpace(eventType)
		if !validCallbackEventType(eventType) {
			return CallbackAdapter{}, errors.New("callback adapter event type is invalid")
		}
		if _, exists := seenEvents[eventType]; exists {
			continue
		}
		seenEvents[eventType] = struct{}{}
		eventTypes = append(eventTypes, eventType)
	}
	sort.Strings(eventTypes)
	value.EventTypes = eventTypes

	credentials := make([]CredentialRequirement, len(value.Credentials))
	seenCredentials := make(map[string]struct{}, len(value.Credentials))
	for index, credential := range value.Credentials {
		credential.Name = strings.TrimSpace(credential.Name)
		credential.Kind = strings.TrimSpace(credential.Kind)
		if !validConversationAdapterIdentifier(credential.Name, 128) ||
			!validConversationAdapterIdentifier(credential.Kind, 128) {
			return CallbackAdapter{}, errors.New("callback adapter credential name or kind is invalid")
		}
		if _, exists := seenCredentials[credential.Name]; exists {
			return CallbackAdapter{}, errors.New("callback adapter credential names must be unique")
		}
		seenCredentials[credential.Name] = struct{}{}
		var err error
		credential.OAuth2, err = NormalizeOAuth2Requirement(credential.OAuth2)
		if err != nil {
			return CallbackAdapter{}, err
		}
		credentials[index] = credential
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].Name < credentials[j].Name })
	value.Credentials = credentials
	var err error
	value.Transport.IngressCredentials, err = normalizeConversationCredentialSelection(
		value.Transport.IngressCredentials, seenCredentials, "callback ingress",
	)
	if err != nil {
		return CallbackAdapter{}, err
	}
	usedCredentials := make(map[string]struct{}, len(credentials))
	for _, name := range value.Transport.IngressCredentials {
		usedCredentials[name] = struct{}{}
	}
	if value.Transport.Connection != nil {
		connection := value.Transport.Connection
		if connection.Kind != "websocket" ||
			!validConversationAdapterIdentifier(connection.Endpoint, 256) {
			return CallbackAdapter{}, errors.New("callback adapter connection transport is invalid")
		}
		connection.Credentials, err = normalizeConversationCredentialSelection(
			connection.Credentials, seenCredentials, "callback connection",
		)
		if err != nil {
			return CallbackAdapter{}, err
		}
		if len(connection.Credentials) == 0 {
			return CallbackAdapter{}, errors.New("callback adapter connection requires credentials")
		}
		if connection.SharedByCredential != "" {
			found := false
			for _, name := range connection.Credentials {
				found = found || name == connection.SharedByCredential
			}
			if !found {
				return CallbackAdapter{}, errors.New("callback adapter shared connection credential must be projected into the connection")
			}
		}
		for _, name := range connection.Credentials {
			usedCredentials[name] = struct{}{}
		}
	}
	if len(usedCredentials) != len(credentials) {
		return CallbackAdapter{}, errors.New("callback adapter credentials must declare ingress or connection use")
	}
	return value, nil
}

func validCallbackEventType(value string) bool {
	if value == "" || len(value) > 256 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !validConversationAdapterIdentifier(part, 128) {
			return false
		}
	}
	return true
}
