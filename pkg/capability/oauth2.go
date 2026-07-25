package capability

import (
	"errors"
	"sort"
	"strings"
)

// OAuth2Subject identifies the principal class that grants access. It is part
// of the portable capability contract because an installation grant and an
// end-user delegated grant are not interchangeable.
type OAuth2Subject string

const (
	OAuth2SubjectInstallation OAuth2Subject = "installation"
	OAuth2SubjectUser         OAuth2Subject = "user"
)

// OAuth2Requirement is the credential-free authorization contract required by
// a Skill action or prompt module. Provider is a stable adapter identity, not
// an authorization URL. Resource uses OAuth 2 resource-indicator semantics
// when a provider distinguishes grants for different APIs.
type OAuth2Requirement struct {
	Provider string        `json:"provider"`
	Subject  OAuth2Subject `json:"subject"`
	Scopes   []string      `json:"scopes"`
	Resource string        `json:"resource,omitempty"`
}

// OAuth2GrantSummary is a non-secret host attestation for an authorized
// connection. It deliberately has no token, client credential, callback,
// external subject identifier, or refresh metadata. The opaque credential
// reference remains the only identity used for binding and resolution.
type OAuth2GrantSummary struct {
	Provider string        `json:"provider"`
	Subject  OAuth2Subject `json:"subject"`
	Scopes   []string      `json:"scopes"`
	Resource string        `json:"resource,omitempty"`
}

// NormalizeOAuth2Requirement validates and canonicalizes a portable OAuth 2
// requirement. Stable scope ordering keeps catalog digests and review diffs
// deterministic across providers.
func NormalizeOAuth2Requirement(value *OAuth2Requirement) (*OAuth2Requirement, error) {
	if value == nil {
		return nil, nil
	}
	provider, subject, scopes, resource, err := normalizeOAuth2Fields(value.Provider, value.Subject, value.Scopes, value.Resource)
	if err != nil {
		return nil, err
	}
	return &OAuth2Requirement{Provider: provider, Subject: subject, Scopes: scopes, Resource: resource}, nil
}

// NormalizeOAuth2GrantSummary validates and canonicalizes a secret-free host
// grant attestation.
func NormalizeOAuth2GrantSummary(value *OAuth2GrantSummary) (*OAuth2GrantSummary, error) {
	if value == nil {
		return nil, nil
	}
	provider, subject, scopes, resource, err := normalizeOAuth2Fields(value.Provider, value.Subject, value.Scopes, value.Resource)
	if err != nil {
		return nil, err
	}
	return &OAuth2GrantSummary{Provider: provider, Subject: subject, Scopes: scopes, Resource: resource}, nil
}

// OAuth2GrantSatisfies reports whether an exact host-attested grant covers a
// Skill requirement. Scope coverage is a superset check; provider, subject,
// and resource must match exactly so authoring never silently widens identity
// or API audience.
func OAuth2GrantSatisfies(requirement *OAuth2Requirement, grant *OAuth2GrantSummary) bool {
	normalizedRequirement, err := NormalizeOAuth2Requirement(requirement)
	if err != nil || normalizedRequirement == nil {
		return requirement == nil
	}
	normalizedGrant, err := NormalizeOAuth2GrantSummary(grant)
	if err != nil || normalizedGrant == nil ||
		normalizedGrant.Provider != normalizedRequirement.Provider ||
		normalizedGrant.Subject != normalizedRequirement.Subject ||
		normalizedGrant.Resource != normalizedRequirement.Resource {
		return false
	}
	granted := make(map[string]struct{}, len(normalizedGrant.Scopes))
	for _, scope := range normalizedGrant.Scopes {
		granted[scope] = struct{}{}
	}
	for _, scope := range normalizedRequirement.Scopes {
		if _, ok := granted[scope]; !ok {
			return false
		}
	}
	return true
}

func normalizeOAuth2Fields(provider string, subject OAuth2Subject, scopes []string, resource string) (string, OAuth2Subject, []string, string, error) {
	provider = strings.TrimSpace(provider)
	resource = strings.TrimSpace(resource)
	if !validOAuth2Provider(provider) {
		return "", "", nil, "", errors.New("OAuth 2 provider must be a lowercase stable identifier")
	}
	switch subject {
	case OAuth2SubjectInstallation, OAuth2SubjectUser:
	default:
		return "", "", nil, "", errors.New("OAuth 2 subject must be installation or user")
	}
	if len(scopes) == 0 || len(scopes) > 128 {
		return "", "", nil, "", errors.New("OAuth 2 authorization requires between 1 and 128 scopes")
	}
	normalizedScopes := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if !validOAuth2Text(scope, 512) {
			return "", "", nil, "", errors.New("OAuth 2 scopes must be non-empty bounded values without control characters")
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		normalizedScopes = append(normalizedScopes, scope)
	}
	if len(normalizedScopes) == 0 {
		return "", "", nil, "", errors.New("OAuth 2 authorization requires at least one scope")
	}
	sort.Strings(normalizedScopes)
	if resource != "" && !validOAuth2Text(resource, 1024) {
		return "", "", nil, "", errors.New("OAuth 2 resource is invalid")
	}
	return provider, subject, normalizedScopes, resource, nil
}

func validOAuth2Provider(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

func validOAuth2Text(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
