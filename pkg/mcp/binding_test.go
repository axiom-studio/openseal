package mcp

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func validConfig() *BindingConfig {
	return &BindingConfig{
		Endpoint: "https://mcp.example:8443/mcp", Transport: TransportStreamableHTTP, TimeoutSeconds: 90,
		Auth:          Auth{Kind: AuthBearer},
		SecretHeaders: []SecretHeader{{Name: "X-Workspace", Field: "workspace"}},
	}
}

func TestBindingConfigValidation(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*BindingConfig){
		"http endpoint":        func(c *BindingConfig) { c.Endpoint = "http://mcp.example/mcp" },
		"userinfo":             func(c *BindingConfig) { c.Endpoint = "https://u:p@mcp.example/mcp" },
		"query":                func(c *BindingConfig) { c.Endpoint = "https://mcp.example/mcp?key=x" },
		"fragment":             func(c *BindingConfig) { c.Endpoint = "https://mcp.example/mcp#x" },
		"stdio transport":      func(c *BindingConfig) { c.Transport = "stdio" },
		"unknown version":      func(c *BindingConfig) { c.ProtocolVersion = "2023-01-01" },
		"sse with 2026":        func(c *BindingConfig) { c.Transport, c.ProtocolVersion = TransportSSE, ProtocolVersion2026 },
		"timeout too long":     func(c *BindingConfig) { c.TimeoutSeconds = 301 },
		"negative timeout":     func(c *BindingConfig) { c.TimeoutSeconds = -1 },
		"unknown auth":         func(c *BindingConfig) { c.Auth.Kind = "oauth2" },
		"api-key no header":    func(c *BindingConfig) { c.Auth = Auth{Kind: AuthAPIKey} },
		"api-key reserved":     func(c *BindingConfig) { c.Auth = Auth{Kind: AuthAPIKey, Header: "Mcp-Session-Id"} },
		"bearer with prefix":   func(c *BindingConfig) { c.Auth.Prefix = "Token " },
		"prefix CRLF":          func(c *BindingConfig) { c.Auth = Auth{Kind: AuthAPIKey, Header: "X-Key", Prefix: "a\r\n"} },
		"header is auth":       func(c *BindingConfig) { c.SecretHeaders[0].Name = "authorization" },
		"header reuses access": func(c *BindingConfig) { c.SecretHeaders[0].Field = SecretAccess },
		"duplicate header": func(c *BindingConfig) {
			c.SecretHeaders = append(c.SecretHeaders, SecretHeader{Name: "x-workspace", Field: "other"})
		},
	} {
		c := validConfig()
		mutate(c)
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReservedSecretHeaders(t *testing.T) {
	for _, header := range []string{"Host", "Mcp-Session-Id", "Content-Type", "Bad\r\nHeader", "Mcp-Param-X", "Proxy-Authorization"} {
		c := validConfig()
		c.SecretHeaders[0].Name = header
		if c.Validate() == nil {
			t.Errorf("secret header %q accepted", header)
		}
	}
}

func TestDecodeBindingConfigRejectsUnknownFields(t *testing.T) {
	value := map[string]interface{}{
		"endpoint": "https://mcp.example/mcp", "transport": "streamable-http", "auth": map[string]interface{}{"kind": "none"},
	}
	if _, err := DecodeBindingConfig(value); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []map[string]interface{}{{"token": "secret"}, {"tools": []interface{}{}}, {"auth": map[string]interface{}{"kind": "none", "value": "secret"}}} {
		bad := map[string]interface{}{}
		for k, v := range value {
			bad[k] = v
		}
		for k, v := range extra {
			bad[k] = v
		}
		if _, err := DecodeBindingConfig(bad); err == nil {
			t.Errorf("unknown field accepted: %v", extra)
		}
	}
	if _, err := DecodeInput(map[string]interface{}{"tool": "a", "hash": "b", "arguments": map[string]interface{}{}, "endpoint": "https://x"}); err == nil {
		t.Fatal("action input with unknown field accepted")
	}
}

func TestToolSlug(t *testing.T) {
	for name, want := range map[string]string{"lookup": "lookup", "files.read": "files_read", "a/b c": "a_b_c", "Get-Item_2": "Get-Item_2", "-x-": "_x-"} {
		if got := ToolSlug(name); got != want {
			t.Errorf("ToolSlug(%q) = %q, want %q", name, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	slug := ToolSlug(long)
	if len(slug) != MaxToolSlugBytes || !strings.HasPrefix(slug, strings.Repeat("a", 39)+"_") || slug == ToolSlug(long+"b") {
		t.Fatalf("long slug %q", slug)
	}
}

func TestBindingToolPinsDiscoveredTool(t *testing.T) {
	tool := lookupTool()
	tool["name"] = "files.lookup"
	snapshot := ToolSnapshot{Tool: tool, Hash: Digest(tool)}
	pinned, err := snapshot.BindingTool("")
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Name != "files_lookup" || pinned.Description != "Read a value" || pinned.Effect != skill.BindingToolEffectRead ||
		!reflect.DeepEqual(pinned.InputSchema, tool["inputSchema"]) ||
		!reflect.DeepEqual(pinned.Arguments, map[string]interface{}{"tool": "files.lookup", "hash": snapshot.Hash}) {
		t.Fatalf("binding tool = %#v", pinned)
	}
	if pinned.InputSchema["properties"].(map[string]interface{})["key"] = "changed"; tool["inputSchema"].(map[string]interface{})["properties"].(map[string]interface{})["key"] == "changed" {
		t.Fatal("binding tool aliases the snapshot's schema")
	}
	if write, err := snapshot.BindingTool(skill.BindingToolEffectWrite); err != nil || write.Effect != skill.BindingToolEffectWrite {
		t.Fatalf("user effect not kept: %v %v", write.Effect, err)
	}
	delete(tool, "annotations")
	tool["description"] = "  "
	tool["title"] = strings.Repeat("é", skill.MaxBindingToolDescriptionBytes)
	if hinted, err := (ToolSnapshot{Tool: tool, Hash: Digest(tool)}).BindingTool(""); err != nil ||
		hinted.Effect != skill.BindingToolEffectWrite || len(hinted.Description) > skill.MaxBindingToolDescriptionBytes || !utf8.ValidString(hinted.Description) {
		t.Fatalf("unhinted tool = %v %q %v", hinted.Effect, hinted.Description, err)
	}
	delete(tool, "title")
	if named, _ := (ToolSnapshot{Tool: tool, Hash: Digest(tool)}).BindingTool(""); named.Description != "files.lookup" {
		t.Fatalf("description fallback = %q", named.Description)
	}
	for name, mutate := range map[string]func(map[string]interface{}) string{
		"no name":  func(m map[string]interface{}) string { delete(m, "name"); return Digest(m) },
		"bad hash": func(m map[string]interface{}) string { return "sha256:x" },
		"array input": func(m map[string]interface{}) string {
			m["inputSchema"] = map[string]interface{}{"type": "array"}
			return Digest(m)
		},
		"remote ref": func(m map[string]interface{}) string {
			m["inputSchema"] = schemaOf(`{"type":"object","$ref":"https://evil.example/s.json"}`)
			return Digest(m)
		},
		"bad output": func(m map[string]interface{}) string {
			m["outputSchema"] = schemaOf(`{"nullable":true}`)
			return Digest(m)
		},
		"needs tasks": func(m map[string]interface{}) string {
			m["execution"] = map[string]interface{}{"taskSupport": "required"}
			return Digest(m)
		},
	} {
		m := lookupTool()
		hash := mutate(m)
		if _, err := (ToolSnapshot{Tool: m, Hash: hash}).BindingTool(""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := snapshot.BindingTool("destroy"); err == nil {
		t.Error("unknown effect accepted")
	}
}

func TestSchemaSubset(t *testing.T) {
	ok := schemaOf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"when":{"type":"string","format":"date-time"},"item":{"$ref":"#/$defs/item"}},"$defs":{"item":{"type":"integer","minimum":1}}}`)
	s, err := compileSchema(ok)
	if err != nil {
		t.Fatal(err)
	}
	if s.validate(map[string]interface{}{"item": 1}) != nil || s.validate(map[string]interface{}{"item": 0}) == nil {
		t.Fatal("local $ref not enforced")
	}
	for _, bad := range []string{`{"$schema":"https://example.com/custom"}`, `{"$ref":"other.json"}`, `{"items":[{"type":"string"}]}`, `{"nullable":true}`} {
		if _, err := compileSchema(schemaOf(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	deep := map[string]interface{}{"type": "object"}
	for i := 0; i < maxSchemaDepth+2; i++ {
		deep = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"a": deep}}
	}
	if _, err := compileSchema(deep); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}

func TestHeaderEncodingAndNestedSchema(t *testing.T) {
	schema := schemaOf(`{"type":"object","properties":{"nested":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"},"n":{"type":"integer","x-mcp-header":"N"}}}}}`)
	h := http.Header{}
	if err := mirrorHeaders(schema, map[string]interface{}{"nested": map[string]interface{}{"region": "Hello, 世界", "n": float64(42)}}, h); err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Mcp-Param-Region"); got != "=?base64?SGVsbG8sIOS4lueVjA==?=" {
		t.Fatal(got)
	}
	if h.Get("Mcp-Param-N") != "42" {
		t.Fatal("integer header")
	}
	if mirrorHeaders(schema, map[string]interface{}{"nested": map[string]interface{}{"n": 1.5}}, http.Header{}) == nil {
		t.Fatal("fractional integer header accepted")
	}
	for _, raw := range []string{" padded ", "bad\r\nvalue", "=?base64?a?="} {
		if headerValue(raw) == raw {
			t.Fatalf("unsafe value %q was not encoded", raw)
		}
	}
	if validateHeaderSchema(schemaOf(`{"type":"array","items":{"type":"string","x-mcp-header":"Region"}}`)) == nil {
		t.Fatal("array annotation accepted")
	}
	if validateHeaderSchema(schemaOf(`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`)) == nil {
		t.Fatal("duplicate header annotation accepted")
	}
}

func TestHeaderSecretsHoldExactlyTheConfiguredFields(t *testing.T) {
	c := validConfig()
	c.SecretHeaders = append(c.SecretHeaders, SecretHeader{Name: "X-Team", Field: "team"}, SecretHeader{Name: "X-Team-Copy", Field: "team"})
	secrets, err := c.HeaderSecrets(`{"workspace":"ws-secret-1","team":"team-secret-2"}`)
	if err != nil || !reflect.DeepEqual(secrets, Secrets{"workspace": "ws-secret-1", "team": "team-secret-2"}) {
		t.Fatalf("secrets = %v, %v", secrets, err)
	}
	encoded, err := c.EncodeHeaderSecrets(map[string]string{"workspace": "a", "team": "b"})
	if err != nil || encoded != `{"team":"b","workspace":"a"}` {
		t.Fatalf("encoded = %q, %v", encoded, err)
	}
	for name, raw := range map[string]string{
		"not JSON":      `{"workspace":`,
		"array":         `["ws-secret-1"]`,
		"string":        `"ws-secret-1"`,
		"null":          `null`,
		"trailing":      `{"workspace":"ws-secret-1","team":"t"} {}`,
		"number":        `{"workspace":1,"team":"t"}`,
		"object value":  `{"workspace":{"v":"ws-secret-1"},"team":"t"}`,
		"empty value":   `{"workspace":"","team":"t"}`,
		"missing field": `{"workspace":"ws-secret-1"}`,
		"unknown field": `{"workspace":"ws-secret-1","team":"t","other":"x"}`,
		"CRLF":          `{"workspace":"ws-secret-1\r\nX-Evil: 1","team":"t"}`,
		"LF":            `{"workspace":"ws-secret-1\nx","team":"t"}`,
		"NUL":           `{"workspace":"ws-secret-1\u0000","team":"t"}`,
		"oversized":     `{"workspace":"` + strings.Repeat("a", MaxSecretHeaderBytes+1) + `","team":"t"}`,
	} {
		_, err := c.HeaderSecrets(raw)
		if err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "ws-secret-1") {
			t.Errorf("%s error leaks the value: %v", name, err)
		}
	}
	if _, err := c.EncodeHeaderSecrets(map[string]string{"workspace": "a\r\nb", "team": "b"}); err == nil {
		t.Error("CRLF value encoded")
	}
	if _, err := (&BindingConfig{}).HeaderSecrets(`{}`); err == nil {
		t.Error("headers accepted for a binding without secret headers")
	}
}
