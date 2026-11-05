// Package mcp is a client for remote Model Context Protocol servers
// (Streamable HTTP and legacy SSE) and the openseal.mcp Skill that exposes one
// connected server's tools as binding tools. OpenSeal owns the protocol, limits, schema
// checks and redaction; the embedding host owns the network, egress policy and
// credentials. Nothing here keeps state outside a Client or Session, so one
// process may serve every tenant.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// Secrets are one call's credential values keyed by field: SecretAccess,
// SecretUsername and SecretPassword for Auth, plus SecretHeaders fields. The
// host resolves them per call; they are never part of BindingConfig.
type Secrets map[string]string

// Options configures a Client.
type Options struct {
	// HTTPClient carries every request and is required. It is the host's
	// egress guard: its transport must refuse private, loopback and
	// link-local destinations (checked at dial time, not by hostname), must
	// not use environment proxies, and should bound TLS and header waits.
	// The Client copies it without its cookie jar, timeout or redirect
	// policy: requests never follow redirects and are bounded by context.
	HTTPClient *http.Client
	// ClientName and ClientVersion identify this client to servers;
	// "openseal" and SkillVersion when empty.
	ClientName    string
	ClientVersion string
}

// Client talks to the server of one binding. It is immutable and safe for
// concurrent use; secrets arrive per Session.
type Client struct {
	config  BindingConfig
	http    *http.Client
	name    string
	version string
}

// ToolSnapshot is one tool as a server lists it, with secret values
// redacted. Hash is Digest of the unredacted definition.
type ToolSnapshot struct {
	Tool map[string]interface{} `json:"tool"`
	Hash string                 `json:"hash"`
}

// BindingTool turns a discovered tool into the binding tool that pins it:
// named ToolSlug of the remote name, with the server's input schema and fixed
// arguments {tool: remote name, hash: Hash}. effect is the user's choice; an
// empty effect defaults to read when the server marks the tool readOnlyHint,
// otherwise write. The output schema is not pinned: Hash covers it, and a
// call validates structuredContent against the verified live definition. A
// tool this client cannot call is refused.
func (t ToolSnapshot) BindingTool(effect skill.BindingToolEffect) (skill.BindingTool, error) {
	name, _ := t.Tool["name"].(string)
	if name == "" || len(name) > MaxToolNameBytes {
		return skill.BindingTool{}, fmt.Errorf("MCP tool name must be 1–%d bytes", MaxToolNameBytes)
	}
	if !toolHash.MatchString(t.Hash) {
		return skill.BindingTool{}, fmt.Errorf("MCP tool %q hash must be 64 lowercase hex digits", name)
	}
	annotations, _ := t.Tool["annotations"].(map[string]interface{})
	switch effect {
	case "":
		effect = skill.BindingToolEffectWrite
		if readOnly, _ := annotations["readOnlyHint"].(bool); readOnly {
			effect = skill.BindingToolEffectRead
		}
	case skill.BindingToolEffectRead, skill.BindingToolEffectWrite:
	default:
		return skill.BindingTool{}, fmt.Errorf("MCP tool %q effect must be read or write", name)
	}
	input, _ := t.Tool["inputSchema"].(map[string]interface{})
	if input["type"] != "object" {
		return skill.BindingTool{}, fmt.Errorf("MCP tool %q input schema must describe an object", name)
	}
	verified, ok := verify(t.Tool, input)
	if !ok {
		return skill.BindingTool{}, fmt.Errorf("MCP tool %q uses schemas outside the supported subset", name)
	}
	if verified.tasks {
		return skill.BindingTool{}, fmt.Errorf("MCP tool %q requires tasks, which are not supported", name)
	}
	var schema map[string]interface{}
	encoded, _ := json.Marshal(input)
	_ = json.Unmarshal(encoded, &schema)
	return skill.BindingTool{
		Name: ToolSlug(name), Description: toolDescription(t.Tool, name), InputSchema: schema, Effect: effect,
		Arguments: map[string]interface{}{"tool": name, "hash": t.Hash},
	}, nil
}

// toolDescription is the tool's description, else its title, else its name,
// bounded to a binding tool description.
func toolDescription(tool map[string]interface{}, name string) string {
	annotations, _ := tool["annotations"].(map[string]interface{})
	for _, candidate := range []interface{}{tool["description"], tool["title"], annotations["title"]} {
		if text, _ := candidate.(string); strings.TrimSpace(text) != "" {
			return strings.TrimSpace(truncate(strings.TrimSpace(text), skill.MaxBindingToolDescriptionBytes))
		}
	}
	return name
}

// CallResult is a successful tools/call result with secret values redacted.
type CallResult struct {
	Content           []interface{} `json:"content"`
	StructuredContent interface{}   `json:"structuredContent,omitempty"`
	// OutputSchema is the tool's declared output schema, for taking its file
	// content out of the result (toolfile.ExtractMCP); it is not part of the
	// result.
	OutputSchema map[string]interface{} `json:"-"`
}

// NewClient validates config and snapshots it. See Options.HTTPClient: the
// host's client is the only network path, and passing an unguarded client
// lets bindings reach internal services.
func NewClient(config *BindingConfig, options Options) (*Client, error) {
	if options.HTTPClient == nil {
		return nil, errors.New("MCP client needs the host's egress-guarded HTTP client")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if len(options.ClientName) > 128 || len(options.ClientVersion) > 64 {
		return nil, errors.New("MCP client identity exceeds limits")
	}
	encoded, _ := json.Marshal(config)
	c := &Client{name: "openseal", version: SkillVersion}
	_ = json.Unmarshal(encoded, &c.config)
	if options.ClientName != "" {
		c.name = options.ClientName
	}
	if options.ClientVersion != "" {
		c.version = options.ClientVersion
	}
	httpClient := *options.HTTPClient
	httpClient.Jar, httpClient.Timeout = nil, 0
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = &httpClient
	return c, nil
}

// Timeout bounds each Session operation and each one-shot helper.
func (c *Client) Timeout() time.Duration { return c.config.timeout() }

// Discover opens a session, lists the server's tools and closes it.
func (c *Client) Discover(ctx context.Context, secrets Secrets) ([]ToolSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout())
	defer cancel()
	s, err := c.Open(ctx, secrets)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Tools(ctx)
}

// Call opens a session, verifies input's pinned tool, makes one call for
// action (Read or Call) and closes the session.
func (c *Client) Call(ctx context.Context, secrets Secrets, action string, input Input) (*CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout())
	defer cancel()
	s, err := c.Open(ctx, secrets)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if _, err := s.Verify(ctx, map[string]string{input.Tool: input.Hash}); err != nil {
		return nil, err
	}
	return s.Call(ctx, action, input)
}

// authHeaders builds the per-session auth and secret headers and the values
// to redact.
func (c *Client) authHeaders(secrets Secrets) (http.Header, redactor, error) {
	h := http.Header{}
	values := []string{}
	get := func(field string) (string, error) {
		v := secrets[field]
		if v == "" || strings.ContainsAny(v, "\r\n\x00") {
			return "", errors.New("required MCP secret " + field + " is missing or invalid")
		}
		values = append(values, v)
		return v, nil
	}
	for _, header := range c.config.SecretHeaders {
		v, err := get(header.Field)
		if err != nil {
			return nil, nil, err
		}
		h.Set(header.Name, v)
	}
	switch c.config.Auth.Kind {
	case AuthBearer, AuthOAuth, AuthAPIKey:
		v, err := get(SecretAccess)
		if err != nil {
			return nil, nil, err
		}
		if c.config.Auth.Kind == AuthAPIKey {
			h.Set(c.config.Auth.Header, c.config.Auth.Prefix+v)
		} else {
			h.Set("Authorization", "Bearer "+v)
		}
	case AuthBasic:
		user, err := get(SecretUsername)
		if err != nil {
			return nil, nil, err
		}
		if strings.Contains(user, ":") {
			return nil, nil, errors.New("HTTP Basic username cannot contain a colon")
		}
		pass, err := get(SecretPassword)
		if err != nil {
			return nil, nil, err
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		values = append(values, encoded)
		h.Set("Authorization", "Basic "+encoded)
	}
	return h, newRedactor(values...), nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
