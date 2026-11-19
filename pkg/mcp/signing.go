package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CredentialField is one value a person enters for an auth kind. Hosts build
// their key forms from Auth.Fields or SigningFields, so a new signing scheme
// needs no host code.
type CredentialField struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret"`
	Optional bool   `json:"optional,omitempty"`
}

// Signing is how a signed binding signs each request: a scheme, its
// non-secret params, and the scheme's optional fields this binding fills.
type Signing struct {
	Scheme   string            `json:"scheme"`
	Params   map[string]string `json:"params,omitempty"`
	Optional []string          `json:"optional,omitempty"`
}

// scheme is one request-signing method. Each lives in its own file.
type scheme interface {
	fields() []CredentialField
	checkParams(params map[string]string) error
	// ownsHeader reports whether the scheme sets the lowercase header name,
	// so no secret header may.
	ownsHeader(name string) bool
	// suggest reads params from an endpoint the scheme recognises.
	suggest(endpoint *url.URL) (map[string]string, bool)
	sign(params map[string]string, secrets Secrets) func(*http.Request, []byte) error
}

// SigningSchemes are the supported signing schemes.
var SigningSchemes = []string{SigningAWSSigV4}

func signingScheme(name string) scheme {
	switch name {
	case SigningAWSSigV4:
		return awsSigV4{}
	}
	return nil
}

func schemeNames() []interface{} {
	names := make([]interface{}, len(SigningSchemes))
	for i, name := range SigningSchemes {
		names[i] = name
	}
	return names
}

// Fields are the credential fields of a, in form order.
func (a Auth) Fields() []CredentialField {
	switch a.Kind {
	case AuthBearer, AuthAPIKey:
		return []CredentialField{{Name: SecretAccess, Label: "API key", Secret: true}}
	case AuthOAuth:
		return []CredentialField{{Name: SecretAccess, Label: "Access token", Secret: true}}
	case AuthBasic:
		return []CredentialField{{Name: SecretUsername, Label: "Username"}, {Name: SecretPassword, Label: "Password", Secret: true}}
	case AuthSigned:
		if a.Signing != nil {
			if s := signingScheme(a.Signing.Scheme); s != nil {
				return s.fields()
			}
		}
	}
	return nil
}

// SigningFields are the credential fields of a signing scheme.
func SigningFields(name string) ([]CredentialField, bool) {
	s := signingScheme(name)
	if s == nil {
		return nil, false
	}
	return s.fields(), true
}

// SuggestSigning returns the signing an endpoint needs when a scheme
// recognises it, such as SigV4 for <service>.<region>.api.aws.
func SuggestSigning(endpoint string) (*Signing, bool) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Hostname() == "" {
		return nil, false
	}
	for _, name := range SigningSchemes {
		if params, ok := signingScheme(name).suggest(parsed); ok {
			return &Signing{Scheme: name, Params: params}, true
		}
	}
	return nil, false
}

func (s *Signing) validate() error {
	if s == nil {
		return errors.New("MCP signed auth needs a signing scheme")
	}
	scheme := signingScheme(s.Scheme)
	if scheme == nil {
		return fmt.Errorf("unsupported MCP signing scheme %q", s.Scheme)
	}
	if err := scheme.checkParams(s.Params); err != nil {
		return err
	}
	optional := map[string]bool{}
	for _, field := range scheme.fields() {
		optional[field.Name] = field.Optional
	}
	seen := map[string]bool{}
	for _, name := range s.Optional {
		if !optional[name] || seen[name] {
			return fmt.Errorf("MCP signing field %q is not an optional field of %s", name, s.Scheme)
		}
		seen[name] = true
	}
	return nil
}

// fills reports whether the binding fills the optional field name.
func (s *Signing) fills(name string) bool {
	if s == nil {
		return false
	}
	for _, optional := range s.Optional {
		if optional == name {
			return true
		}
	}
	return false
}

func (s *Signing) ownsHeader(name string) bool {
	scheme := signingScheme(s.Scheme)
	return scheme != nil && scheme.ownsHeader(name)
}

// signer is the per-request signer of a signed binding, nil without
// signing. Validate has checked the scheme.
func (s *Signing) signer(secrets Secrets) func(*http.Request, []byte) error {
	if s == nil {
		return nil
	}
	return signingScheme(s.Scheme).sign(s.Params, secrets)
}
