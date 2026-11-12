package models

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// SkillSetupKindConnectApp is the setup request kind of a connect card. It
// is stored with the other in-chat setup requests; its target is always the
// openseal.mcp Skill and its resolution names the connection's host binding.
const SkillSetupKindConnectApp = "connect_app"

const (
	MaximumAppAddress = 2048
)

// App sign-in kinds as the catalog describes an app's first endpoint.
const (
	AppSignInOAuth   = "oauth"
	AppSignInAPIKey  = "api-key"
	AppSignInNone    = "none"
	AppSignInUnknown = "unknown"
)

// ConnectAppTarget is the app a connect_app request asks for: a catalog
// entry (CatalogSlug) or an MCP server address the user gave (Address), with
// what the card shows about it.
type ConnectAppTarget struct {
	CatalogSlug    string `json:"catalogSlug,omitempty"`
	Address        string `json:"address,omitempty"`
	Name           string `json:"name"`
	Official       bool   `json:"official,omitempty"`
	SignIn         string `json:"signIn,omitempty"`
	LogoSHA256     string `json:"logoSha256,omitempty"`
	LogoMonochrome bool   `json:"logoMonochrome,omitempty"`
}

func (t *ConnectAppTarget) validate() error {
	if t == nil {
		return NewSkillSetupValidationError("app", "is required for connect_app")
	}
	if (t.CatalogSlug == "") == (t.Address == "") {
		return NewSkillSetupValidationError("app", "must name exactly one catalog app or address")
	}
	if t.CatalogSlug != "" && !IsValidAppSlug(t.CatalogSlug) {
		return NewSkillSetupValidationError("app", "has an invalid catalog slug")
	}
	if t.Address != "" {
		if normalized, err := NormalizeAppAddress(t.Address); err != nil || normalized != t.Address {
			return NewSkillSetupValidationError("app", "has an invalid address")
		}
	}
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 256 {
		return NewSkillSetupValidationError("app", "needs a display name of at most 256 bytes")
	}
	switch t.SignIn {
	case "", AppSignInOAuth, AppSignInAPIKey, AppSignInNone, AppSignInUnknown:
	default:
		return NewSkillSetupValidationError("app", "has an unknown sign-in kind")
	}
	if t.LogoSHA256 != "" && !IsValidLogoDigest(t.LogoSHA256) {
		return NewSkillSetupValidationError("app", "has an invalid logo digest")
	}
	return nil
}

// appSlugPattern is a catalog slug, which may contain "/" (for example
// neondatabase/mcp-server-neon).
var appSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func IsValidAppSlug(value string) bool {
	return appSlugPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//")
}

func IsValidLogoDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// NormalizeAppAddress returns an MCP server address as connections store it:
// an https URL without credentials, query or fragment.
func NormalizeAppAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || len(value) > MaximumAppAddress || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("the address must be an https URL without credentials or a query, such as https://mcp.example.com/mcp")
	}
	parsed.Scheme, parsed.Host = "https", strings.ToLower(parsed.Host)
	return parsed.String(), nil
}
