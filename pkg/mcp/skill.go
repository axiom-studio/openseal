package mcp

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID      = "openseal.mcp"
	SkillVersion = "1.1.0"
	// Read calls a binding tool whose effect is read, Call one whose effect
	// is write. The model reaches them only as binding tools
	// mcp.<bindingSlug>.<tool>.
	Read         = "read"
	Call         = "call"
	RefreshTools = "refresh_tools"
	// ModelPrefix is the first segment of every binding tool's model name.
	ModelPrefix = "mcp"
)

// Credential slots of every action. access, username and password are
// named exactly as the Secrets fields the host fills from them; headers holds
// every SecretHeaders value as one JSON object {field: value}, which the host
// turns into Secrets with BindingConfig.HeaderSecrets. All are optional: an
// open server binds none, and a binding fills exactly the slots of its config
// (BindingConfig.CredentialSlots). One kind covers a host-managed OAuth grant
// (resolved to its current access token) and a stored key or password, so
// either can fill access.
const (
	CredentialAccess   = SecretAccess
	CredentialUsername = SecretUsername
	CredentialPassword = SecretPassword
	CredentialHeaders  = "headers"

	// CredentialKind is the kind of every openseal.mcp credential reference,
	// and the OAuth connection kind hosts create MCP grants with.
	CredentialKind = "mcp_credential"
)

// Credentials are the credential requirements of the read, call and
// refresh_tools actions.
func Credentials() []skill.CredentialRequirement {
	return []skill.CredentialRequirement{
		{Name: CredentialAccess, Kind: CredentialKind, Optional: true},
		{Name: CredentialUsername, Kind: CredentialKind, Optional: true},
		{Name: CredentialPassword, Kind: CredentialKind, Optional: true},
		{Name: CredentialHeaders, Kind: CredentialKind, Optional: true},
	}
}

// CredentialSlots names the credential slots a binding with this config must
// fill, and only those: the slots of its Auth kind, then headers when it has
// SecretHeaders.
func (c *BindingConfig) CredentialSlots() []string {
	var slots []string
	switch c.Auth.Kind {
	case AuthOAuth, AuthBearer, AuthAPIKey:
		slots = []string{CredentialAccess}
	case AuthBasic:
		slots = []string{CredentialUsername, CredentialPassword}
	}
	if len(c.SecretHeaders) > 0 {
		slots = append(slots, CredentialHeaders)
	}
	return slots
}

func object(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func text(min, max int) map[string]interface{} {
	return map[string]interface{}{"type": "string", "minLength": min, "maxLength": max}
}

func enum(values ...interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "string", "enum": values}
}

func hashSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": "^[0-9a-f]{64}$"}
}

// SkillDefinition is the immutable contract of one connected remote MCP
// server; each binding's BindingConfig names the server and its Tools pin the
// server's tools (ToolSnapshot.BindingTool). The read and call inputs are
// exactly what the kernel compiles from a binding tool call: the fixed tool
// and hash arguments and the model's arguments object.
// The host runs every action through Client with its own egress guard and
// the binding's secrets.
func SkillDefinition() *skill.Definition {
	input := func() map[string]interface{} {
		return object([]interface{}{"tool", "hash", "arguments"}, map[string]interface{}{
			"tool":      text(1, MaxToolNameBytes),
			"hash":      hashSchema(),
			"arguments": map[string]interface{}{"type": "object"},
		})
	}
	output := func() map[string]interface{} {
		return object([]interface{}{"content"}, map[string]interface{}{
			"content":           map[string]interface{}{"type": "array"},
			"structuredContent": map[string]interface{}{},
		})
	}
	tool := func(name, description string, effect skill.SideEffect, risk skill.RiskLevel, idempotency skill.IdempotencyMode) skill.Action {
		return skill.Action{
			Name: name, Description: description, InputSchema: input(), OutputSchema: output(),
			SideEffect: effect, Risk: risk, Idempotency: idempotency, Credentials: Credentials(),
			Timeout: capability.Duration(MaxTimeoutSeconds * time.Second), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		}
	}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Connected apps",
		Description: "Connect any app or service that publishes an MCP server, such as Notion, Linear or Supabase: sign in or add its key in Settings → Connected apps, then use its tools.",
		Icon:        "plug", Category: "integrations", Tags: []string{"connect", "connection", "connector", "integration", "app", "mcp"},
		Transport:           skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		BindingConfigSchema: bindingConfigSchema(),
		BindingTools: &capability.BindingToolsSpec{
			ModelPrefix: ModelPrefix, ReadAction: Read, WriteAction: Call,
			InputArgument: "arguments", FixedArguments: []string{"tool", "hash"},
		},
		Actions: map[string]skill.Action{
			Read: tool(Read, "Call a tool of the connected MCP server that is pinned as read-only.", skill.SideEffectRead, skill.RiskLevelRead, skill.IdempotencySupported),
			Call: tool(Call, "Call a tool of the connected MCP server that may change data.", skill.SideEffectWrite, skill.RiskLevelWrite, skill.IdempotencyRequired),
			RefreshTools: {
				Name: RefreshTools, Description: "List the connected MCP server's current tools and their hashes.",
				InputSchema: object([]interface{}{}, map[string]interface{}{}),
				// tools are the server's current tools, bindingTools the binding
				// tools suggested for those this client can call, and
				// unsupported the others with the reason.
				OutputSchema: object([]interface{}{"tools"}, map[string]interface{}{
					"tools": map[string]interface{}{"type": "array", "maxItems": MaxListedTools, "items": object([]interface{}{"tool", "hash"}, map[string]interface{}{
						"tool": map[string]interface{}{"type": "object"}, "hash": hashSchema(),
					})},
					"bindingTools": map[string]interface{}{"type": "array", "maxItems": MaxListedTools, "items": map[string]interface{}{"type": "object"}},
					"unsupported": map[string]interface{}{"type": "array", "maxItems": MaxListedTools, "items": object([]interface{}{"name", "reason"}, map[string]interface{}{
						"name": text(1, MaxToolNameBytes), "reason": text(1, 1024),
					})},
				}),
				SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported, Credentials: Credentials(),
				Timeout: capability.Duration(MaxTimeoutSeconds * time.Second), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			},
		},
	}
}

// bindingConfigSchema mirrors BindingConfig; BindingConfig.Validate is the
// full check.
func bindingConfigSchema() map[string]interface{} {
	versions := []interface{}{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", ProtocolVersion2026}
	return object([]interface{}{"endpoint", "transport", "auth"}, map[string]interface{}{
		"endpoint":        map[string]interface{}{"type": "string", "maxLength": 2048, "pattern": "^https://"},
		"transport":       enum(string(TransportStreamableHTTP), string(TransportSSE)),
		"protocolVersion": enum(versions...),
		"timeoutSeconds":  map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaxTimeoutSeconds},
		"auth": object([]interface{}{"kind"}, map[string]interface{}{
			"kind":   enum(string(AuthNone), string(AuthBearer), string(AuthAPIKey), string(AuthBasic), string(AuthOAuth)),
			"header": text(1, 128), "prefix": text(0, 64),
		}),
		"secretHeaders": map[string]interface{}{"type": "array", "maxItems": MaxSecretHeaders, "items": object([]interface{}{"name", "field"}, map[string]interface{}{
			"name": text(1, 128), "field": text(1, 64),
		})},
	})
}
