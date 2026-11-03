package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Transport string

const (
	TransportStreamableHTTP Transport = "streamable-http"
	TransportSSE            Transport = "sse"
)

type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthBearer AuthKind = "bearer"
	AuthAPIKey AuthKind = "api-key"
	AuthBasic  AuthKind = "basic"
	// AuthOAuth sends a pre-issued access token as a bearer token. The host
	// owns the grant, refresh and rotation; this package never calls a token
	// endpoint.
	AuthOAuth AuthKind = "oauth"
)

// Secret fields the authentication kinds read from a call's Secrets.
const (
	SecretAccess   = "access"   // bearer, api-key and oauth
	SecretUsername = "username" // basic
	SecretPassword = "password" // basic
)

const (
	MaxBytes     = 2 << 20
	MaxToolPages = 20
	// MaxListedTools bounds one tools/list across all pages. A binding pins
	// at most skill.MaxBindingTools of them.
	MaxListedTools   = 1000
	MaxSecretHeaders = 64
	// MaxSecretHeaderBytes bounds one secret header value.
	MaxSecretHeaderBytes   = 8 << 10
	MaxToolNameBytes       = 128
	MaxToolSlugBytes       = 48
	MaxTimeoutSeconds      = 300
	DefaultTimeout         = 60 * time.Second
	DefaultProtocolVersion = "2025-11-25"
	// ProtocolVersion2026 is stateless: no initialize or session, and every
	// request carries its version and client identity in params._meta.
	ProtocolVersion2026 = "2026-07-28"
)

// SupportedProtocolVersion reports whether version can be configured.
func SupportedProtocolVersion(version string) bool {
	switch version {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", ProtocolVersion2026:
		return true
	}
	return false
}

var (
	headerName = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	fieldName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	toolHash   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// BindingConfig is one connected server: host-owned, non-secret configuration
// fixed when an openseal.mcp binding is created. Secret values are never part
// of it; Auth and SecretHeaders only name fields of a call's Secrets. The
// pinned tools are the binding's own Tools (see ToolSnapshot.BindingTool).
type BindingConfig struct {
	Endpoint        string         `json:"endpoint"`
	Transport       Transport      `json:"transport"`
	ProtocolVersion string         `json:"protocolVersion,omitempty"`
	TimeoutSeconds  int            `json:"timeoutSeconds,omitempty"`
	Auth            Auth           `json:"auth"`
	SecretHeaders   []SecretHeader `json:"secretHeaders,omitempty"`
}

// Auth is how a request authenticates. Header and Prefix apply to api-key
// only; bearer and oauth always send "Authorization: Bearer <access>".
type Auth struct {
	Kind   AuthKind `json:"kind"`
	Header string   `json:"header,omitempty"`
	Prefix string   `json:"prefix,omitempty"`
}

// SecretHeader sends Secrets[Field] as header Name on every request.
type SecretHeader struct {
	Name  string `json:"name"`
	Field string `json:"field"`
}

// HeaderSecrets decodes the headers credential (CredentialHeaders): a JSON
// object holding exactly one string per SecretHeaders field. It refuses a
// non-object, a non-string, empty or oversized value, a missing or unknown
// field and a value with CR, LF or NUL. Errors name fields, never values.
func (c *BindingConfig) HeaderSecrets(raw string) (Secrets, error) {
	if len(c.SecretHeaders) == 0 {
		return nil, errors.New("MCP binding has no secret headers")
	}
	var values map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&values); err != nil || values == nil || decoder.Decode(new(interface{})) != io.EOF {
		return nil, errors.New("MCP secret headers must be a JSON object of strings")
	}
	secrets := make(Secrets, len(values))
	for _, header := range c.SecretHeaders {
		if _, done := secrets[header.Field]; done {
			continue
		}
		raw, ok := values[header.Field]
		if !ok {
			return nil, fmt.Errorf("MCP secret header field %s is missing", header.Field)
		}
		value, ok := raw.(string)
		if !ok || value == "" || len(value) > MaxSecretHeaderBytes || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("MCP secret header field %s must be a non-empty single-line string", header.Field)
		}
		secrets[header.Field] = value
	}
	if len(secrets) != len(values) {
		return nil, errors.New("MCP secret headers hold a field that is not configured")
	}
	return secrets, nil
}

// EncodeHeaderSecrets is the headers credential for values keyed by
// SecretHeaders field, checked as HeaderSecrets checks it.
func (c *BindingConfig) EncodeHeaderSecrets(values map[string]string) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", errors.New("MCP secret headers could not be encoded")
	}
	if _, err := c.HeaderSecrets(string(encoded)); err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ToolSlug maps a remote tool name to a suffix safe in model tool names such
// as mcp.<binding>.<slug>: every byte outside [A-Za-z0-9_-] becomes "_" (so
// "files.read" is "files_read"), as does a leading "-", and a slug longer than
// MaxToolSlugBytes keeps its first 39 bytes, then "_" and the first 8 hex
// digits of the name's SHA-256. Every slug of a non-empty name is a valid
// binding tool name. Distinct names can share a slug; the kernel refuses a
// binding that declares one name twice.
func ToolSlug(name string) string {
	slug := []byte(name)
	for i, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' && i > 0) {
			slug[i] = '_'
		}
	}
	if len(slug) <= MaxToolSlugBytes {
		return string(slug)
	}
	sum := sha256.Sum256([]byte(name))
	return string(slug[:MaxToolSlugBytes-9]) + "_" + hex.EncodeToString(sum[:4])
}

// DecodeBindingConfig decodes and validates a binding's config, rejecting
// unknown fields.
func DecodeBindingConfig(value map[string]interface{}) (*BindingConfig, error) {
	if value == nil {
		return nil, errors.New("MCP binding config is required")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("MCP binding config is not JSON")
	}
	var config BindingConfig
	if err := strictDecode(encoded, &config); err != nil {
		return nil, fmt.Errorf("invalid MCP binding config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *BindingConfig) Validate() error {
	if c == nil {
		return errors.New("MCP binding config is required")
	}
	if err := validateEndpoint(c.Endpoint); err != nil {
		return err
	}
	if c.Transport != TransportStreamableHTTP && c.Transport != TransportSSE {
		return errors.New("MCP transport must be streamable-http or sse")
	}
	if c.ProtocolVersion != "" && !SupportedProtocolVersion(c.ProtocolVersion) {
		return errors.New("unsupported MCP protocol version")
	}
	if c.Transport == TransportSSE && c.ProtocolVersion == ProtocolVersion2026 {
		return errors.New("legacy SSE requires a protocol version before 2026-07-28")
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("MCP timeout must be 1–%d seconds", MaxTimeoutSeconds)
	}
	authFields := map[string]bool{}
	authHeader := "authorization"
	switch c.Auth.Kind {
	case AuthNone:
		authHeader = ""
	case AuthBearer, AuthOAuth:
		authFields[SecretAccess] = true
	case AuthBasic:
		authFields[SecretUsername], authFields[SecretPassword] = true, true
	case AuthAPIKey:
		if !headerName.MatchString(c.Auth.Header) || reservedHeader(c.Auth.Header) {
			return errors.New("MCP API key header is invalid or protocol-owned")
		}
		authHeader = strings.ToLower(c.Auth.Header)
		authFields[SecretAccess] = true
	default:
		return errors.New("unsupported MCP authentication kind")
	}
	if c.Auth.Kind != AuthAPIKey && (c.Auth.Header != "" || c.Auth.Prefix != "") {
		return errors.New("MCP auth header and prefix apply only to api-key")
	}
	if strings.ContainsAny(c.Auth.Prefix, "\r\n\x00") || len(c.Auth.Prefix) > 64 {
		return errors.New("invalid MCP API key prefix")
	}
	if len(c.SecretHeaders) > MaxSecretHeaders {
		return fmt.Errorf("at most %d MCP secret headers", MaxSecretHeaders)
	}
	seenHeaders := map[string]bool{}
	for _, h := range c.SecretHeaders {
		key := strings.ToLower(h.Name)
		if !headerName.MatchString(h.Name) || reservedHeader(h.Name) || key == authHeader || seenHeaders[key] {
			return fmt.Errorf("MCP secret header %q is invalid, duplicate, protocol-owned or the auth header", h.Name)
		}
		if !fieldName.MatchString(h.Field) || authFields[h.Field] {
			return fmt.Errorf("MCP secret header %q needs its own secret field", h.Name)
		}
		seenHeaders[key] = true
	}
	return nil
}

func (c *BindingConfig) version() string {
	if c.ProtocolVersion != "" {
		return c.ProtocolVersion
	}
	return DefaultProtocolVersion
}

func (c *BindingConfig) timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("MCP endpoint must be HTTPS without credentials, query or fragment")
	}
	return nil
}

// reservedHeader names headers the protocol or HTTP itself owns.
func reservedHeader(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "mcp-") || strings.HasPrefix(name, "proxy-") {
		return true
	}
	switch name {
	case "host", "connection", "keep-alive", "upgrade", "te", "trailer", "transfer-encoding", "expect",
		"content-length", "content-type", "content-encoding", "accept", "accept-encoding", "last-event-id", "cookie":
		return true
	}
	return false
}

// Input is the read and call actions' input as the kernel compiles it from a
// binding tool call: the tool's fixed tool and hash arguments and the model's
// arguments object.
type Input struct {
	Tool      string                 `json:"tool"`
	Hash      string                 `json:"hash"`
	Arguments map[string]interface{} `json:"arguments"`
}

// DecodeInput decodes an action input, rejecting unknown fields.
func DecodeInput(value map[string]interface{}) (*Input, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("MCP action input is not JSON")
	}
	var input Input
	if err := strictDecode(encoded, &input); err != nil || input.Tool == "" || input.Hash == "" {
		return nil, errors.New("MCP action input needs exactly tool, hash and arguments")
	}
	if input.Arguments == nil {
		input.Arguments = map[string]interface{}{}
	}
	return &input, nil
}

func strictDecode(data []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(interface{})) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
