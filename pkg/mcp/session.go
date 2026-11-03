package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ErrSessionExpired means the server no longer knows the session (HTTP 404
// to a request carrying Mcp-Session-Id). Open a new Session and retry reads;
// a write's outcome is unknown.
var ErrSessionExpired = errors.New("MCP session expired; open a new session")

// Session is one initialized connection to the server, owning one call's or
// one connection's secrets. It is safe for concurrent use: requests run in
// parallel and each waits for its own response. Hosts may keep a Session
// across calls (it reuses Mcp-Session-Id) but must key it by binding,
// revision and credential, never share it across credentials, and must
// Close it.
type Session struct {
	client  *Client
	headers http.Header
	redact  redactor
	version string
	id      string
	seq     atomic.Int64
	sse     *sseStream
	mu      sync.Mutex
	// listed is every tool name of the last Verify with its live hash, and
	// verified the pinned tools that matched it and can be called.
	listed   map[string]string
	verified map[string]verifiedTool
	closed   sync.Once
}

type verifiedTool struct {
	hash        string
	inputSchema map[string]interface{}
	input       *schema
	output      *schema
	tasks       bool
}

type envelope struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      *int64                 `json:"id"`
	Method  string                 `json:"method"`
	Result  map[string]interface{} `json:"result"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// Open connects with secrets and, before 2026-07-28, negotiates the protocol
// version with initialize and notifications/initialized.
func (c *Client) Open(ctx context.Context, secrets Secrets) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout())
	defer cancel()
	headers, redact, err := c.authHeaders(secrets)
	if err != nil {
		return nil, err
	}
	s := &Session{client: c, headers: headers, redact: redact}
	if c.config.Transport == TransportSSE {
		if err := s.startSSE(ctx); err != nil {
			return nil, err
		}
	}
	version := c.config.version()
	if version == ProtocolVersion2026 {
		s.version = version
		return s, nil
	}
	result, err := s.request(ctx, "initialize", map[string]interface{}{
		"protocolVersion": version, "capabilities": map[string]interface{}{},
		"clientInfo": map[string]interface{}{"name": c.name, "version": c.version},
	}, nil)
	if err != nil {
		s.Close()
		return nil, err
	}
	negotiated, _ := result["protocolVersion"].(string)
	if !SupportedProtocolVersion(negotiated) || negotiated == ProtocolVersion2026 {
		s.Close()
		return nil, errors.New("unsupported negotiated MCP version")
	}
	s.version = negotiated
	if caps, _ := result["capabilities"].(map[string]interface{}); caps["tools"] == nil {
		s.Close()
		return nil, errors.New("MCP server does not advertise tools")
	}
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close ends the session: it closes a legacy SSE stream or DELETEs a
// Streamable HTTP session. It is idempotent.
func (s *Session) Close() {
	s.closed.Do(func() {
		if s.sse != nil {
			s.sse.close()
			return
		}
		if s.id == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		resp, err := s.do(ctx, http.MethodDelete, s.client.config.Endpoint, nil, http.Header{"Mcp-Session-Id": {s.id}, "Mcp-Protocol-Version": {s.version}})
		if err == nil {
			resp.Body.Close()
		}
	})
}

// Tools lists the server's tools (at most MaxToolPages pages and
// MaxListedTools tools) and returns every tool with secret values redacted.
func (s *Session) Tools(ctx context.Context) ([]ToolSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, s.client.Timeout())
	defer cancel()
	tools, _, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tools {
		tools[i].Tool = s.redact.value(tools[i].Tool, false).(map[string]interface{})
	}
	return tools, nil
}

// Verify re-lists the server's tools and checks the binding's pinned tools,
// remote name to pinned hash as the binding tools' fixed arguments hold them.
// It returns the names of pinned tools that are missing, changed or
// uncallable. Only tools verified by the latest Verify can be called on this
// session; hosts reusing a session verify it when it opens.
func (s *Session) Verify(ctx context.Context, pins map[string]string) ([]string, error) {
	if len(pins) > skill.MaxBindingTools {
		return nil, fmt.Errorf("at most %d pinned MCP tools per binding", skill.MaxBindingTools)
	}
	ctx, cancel := context.WithTimeout(ctx, s.client.Timeout())
	defer cancel()
	tools, unsupported, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]string, len(tools)+len(unsupported))
	for _, tool := range tools {
		name, _ := tool.Tool["name"].(string)
		listed[name] = tool.Hash
	}
	for name, hash := range unsupported {
		listed[name] = hash
	}
	verified := map[string]verifiedTool{}
	for _, tool := range tools {
		name, _ := tool.Tool["name"].(string)
		if hash, pinned := pins[name]; pinned && hash == tool.Hash {
			inputSchema, _ := tool.Tool["inputSchema"].(map[string]interface{})
			if v, ok := verify(tool.Tool, inputSchema); ok && !v.tasks {
				v.hash = tool.Hash
				verified[name] = v
			}
		}
	}
	s.mu.Lock()
	s.listed, s.verified = listed, verified
	s.mu.Unlock()
	drift := []string{}
	for name := range pins {
		if _, ok := verified[name]; !ok {
			drift = append(drift, name)
		}
	}
	sort.Strings(drift)
	return drift, nil
}

// Call invokes one verified tool for action Read or Call. The kernel has
// already checked that input selects a tool its binding declares for action
// (read tools through Read, write tools through Call) and that the arguments
// match the pinned input schema; the session checks the pinned hash against
// the live definition, validates the arguments against it again and
// structuredContent against its output schema. A tool error (isError) or
// protocol error is a failure; neither is retried.
func (s *Session) Call(ctx context.Context, action string, input Input) (*CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.client.Timeout())
	defer cancel()
	if action != Read && action != Call {
		return nil, errors.New("unknown MCP action")
	}
	remote, err := s.verifiedTool(input)
	if err != nil {
		return nil, err
	}
	args := input.Arguments
	if args == nil {
		args = map[string]interface{}{}
	}
	if raw, err := json.Marshal(args); err != nil || len(raw) > MaxBytes {
		return nil, errors.New("invalid or oversized MCP arguments")
	}
	if remote.input.validate(args) != nil {
		return nil, errors.New("MCP arguments do not match the tool's input schema")
	}
	extra := http.Header{}
	if s.version == ProtocolVersion2026 {
		if err := mirrorHeaders(remote.inputSchema, args, extra); err != nil {
			return nil, err
		}
	}
	result, err := s.request(ctx, "tools/call", map[string]interface{}{"name": input.Tool, "arguments": args}, extra)
	if err != nil {
		return nil, err
	}
	if flag, exists := result["isError"]; exists {
		failed, ok := flag.(bool)
		if !ok {
			return nil, errors.New("invalid MCP isError")
		}
		if failed {
			return nil, errors.New("MCP tool execution failed; outcome may be partial; no automatic retry")
		}
	}
	content, ok := result["content"].([]interface{})
	if !ok {
		return nil, errors.New("MCP result is missing its content array; the call may already have run")
	}
	structured := result["structuredContent"]
	if remote.output != nil && remote.output.validate(structured) != nil {
		return nil, errors.New("MCP structuredContent does not match the output schema; the call may already have run")
	}
	out := &CallResult{Content: s.redact.value(content, true).([]interface{})}
	if structured != nil {
		out.StructuredContent = s.redact.value(structured, true)
	}
	return out, nil
}

// verifiedTool returns input's tool when the latest Verify matched its pinned
// hash, and otherwise says why it cannot be called.
func (s *Session) verifiedTool(input Input) (verifiedTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if remote, ok := s.verified[input.Tool]; ok && remote.hash == input.Hash {
		return remote, nil
	}
	if s.listed == nil {
		return verifiedTool{}, errors.New("MCP session has not verified its tools; call Verify first")
	}
	hash, listed := s.listed[input.Tool]
	switch {
	case !listed:
		return verifiedTool{}, fmt.Errorf("MCP tool %q is no longer offered by the server; reconnect to refresh", input.Tool)
	case hash != input.Hash:
		return verifiedTool{}, fmt.Errorf("MCP tool %q: tool definition changed; reconnect to refresh", input.Tool)
	default:
		return verifiedTool{}, fmt.Errorf("MCP tool %q was not verified on this session or is not callable", input.Tool)
	}
}

// list returns every listed tool usable at the session's version, unredacted,
// and the name and hash of each listed tool that is not.
func (s *Session) list(ctx context.Context) ([]ToolSnapshot, map[string]string, error) {
	result := []ToolSnapshot{}
	unsupported := map[string]string{}
	cursor := ""
	seen, names := map[string]bool{}, map[string]bool{}
	for page := 0; page < MaxToolPages; page++ {
		params := map[string]interface{}{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		response, err := s.request(ctx, "tools/list", params, nil)
		if err != nil {
			return nil, nil, err
		}
		tools, ok := response["tools"].([]interface{})
		if !ok {
			return nil, nil, errors.New("invalid tools/list response")
		}
		for _, item := range tools {
			tool, ok := item.(map[string]interface{})
			if !ok {
				return nil, nil, errors.New("invalid MCP tool")
			}
			name, _ := tool["name"].(string)
			if name == "" || names[name] {
				return nil, nil, errors.New("missing or duplicate MCP tool name")
			}
			if names[name] = true; len(names) > MaxListedTools {
				return nil, nil, fmt.Errorf("MCP server lists more than %d tools", MaxListedTools)
			}
			inputSchema, _ := tool["inputSchema"].(map[string]interface{})
			hash := Digest(tool)
			if s.version == ProtocolVersion2026 && validateHeaderSchema(inputSchema) != nil {
				unsupported[name] = hash
				continue
			}
			result = append(result, ToolSnapshot{Tool: tool, Hash: hash})
		}
		next, exists := response["nextCursor"]
		if !exists || next == nil {
			return result, unsupported, nil
		}
		cursor, ok = next.(string)
		if !ok || cursor == "" || seen[cursor] {
			return nil, nil, errors.New("invalid or repeating MCP pagination cursor")
		}
		seen[cursor] = true
	}
	return nil, nil, fmt.Errorf("MCP tools/list exceeds %d pages", MaxToolPages)
}

// verify compiles a tool's remote schemas; a tool whose schemas fall
// outside the supported subset cannot be called.
func verify(tool, inputSchema map[string]interface{}) (verifiedTool, bool) {
	v := verifiedTool{inputSchema: inputSchema}
	var err error
	if v.input, err = compileSchema(inputSchema); err != nil {
		return v, false
	}
	if raw, exists := tool["outputSchema"]; exists {
		output, ok := raw.(map[string]interface{})
		if !ok {
			return v, false
		}
		if v.output, err = compileSchema(output); err != nil {
			return v, false
		}
	}
	if execution, ok := tool["execution"].(map[string]interface{}); ok {
		v.tasks = execution["taskSupport"] == "required"
	}
	return v, true
}

func (s *Session) message(method string, params map[string]interface{}, id int64) ([]byte, error) {
	if params == nil {
		params = map[string]interface{}{}
	}
	if s.version == ProtocolVersion2026 {
		withMeta := make(map[string]interface{}, len(params)+1)
		for k, v := range params {
			withMeta[k] = v
		}
		withMeta["_meta"] = map[string]interface{}{
			"io.modelcontextprotocol/protocolVersion":    s.version,
			"io.modelcontextprotocol/clientInfo":         map[string]interface{}{"name": s.client.name, "version": s.client.version},
			"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
		}
		params = withMeta
	}
	msg := map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params}
	if id != 0 {
		msg["id"] = id
	}
	data, err := json.Marshal(msg)
	if err != nil || len(data) > MaxBytes {
		return nil, errors.New("invalid or oversized MCP request")
	}
	return data, nil
}

func (s *Session) notify(ctx context.Context, method string) error {
	data, err := s.message(method, nil, 0)
	if err != nil {
		return err
	}
	if s.sse != nil {
		_, err = s.sse.send(ctx, s, 0, data)
		return err
	}
	resp, err := s.do(ctx, http.MethodPost, s.client.config.Endpoint, data, s.protocolHeaders(method, nil, nil))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *Session) request(ctx context.Context, method string, params map[string]interface{}, extra http.Header) (map[string]interface{}, error) {
	id := s.seq.Add(1)
	data, err := s.message(method, params, id)
	if err != nil {
		return nil, err
	}
	if s.sse != nil {
		return s.sse.send(ctx, s, id, data)
	}
	resp, err := s.do(ctx, http.MethodPost, s.client.config.Endpoint, data, s.protocolHeaders(method, params, extra))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" {
		s.id = resp.Header.Get("Mcp-Session-Id")
		if len(s.id) > 256 || strings.IndexFunc(s.id, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
			return nil, errors.New("invalid MCP session ID")
		}
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch contentType {
	case "text/event-stream":
		return readStreamResponse(resp.Body, id)
	case "application/json":
		b, err := readBounded(resp.Body)
		if err != nil {
			return nil, err
		}
		result, done, err := response(b, id)
		if err != nil {
			return nil, err
		}
		if !done {
			return nil, errors.New("MCP response ID missing")
		}
		return result, nil
	}
	return nil, errors.New("unsupported MCP response content type")
}

func (s *Session) protocolHeaders(method string, params map[string]interface{}, extra http.Header) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	if s.id != "" {
		h.Set("Mcp-Session-Id", s.id)
	}
	if s.version != "" {
		h.Set("Mcp-Protocol-Version", s.version)
	}
	if s.version == ProtocolVersion2026 {
		h.Set("Mcp-Method", method)
		if name, ok := params["name"].(string); ok {
			h.Set("Mcp-Name", headerValue(name))
		}
		for k, v := range extra {
			h[k] = v
		}
	}
	return h
}

// readStreamResponse returns the matching response from a Streamable HTTP
// SSE body without waiting for the server to close it.
func readStreamResponse(body io.Reader, id int64) (map[string]interface{}, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, MaxBytes+1))
	scanner.Buffer(make([]byte, 4096), MaxBytes)
	var event []string
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		if total += len(line) + 1; total > MaxBytes {
			return nil, errors.New("MCP stream exceeds 2 MiB")
		}
		if line == "" {
			if len(event) > 0 {
				result, done, err := response([]byte(strings.Join(event, "\n")), id)
				if err != nil || done {
					return result, err
				}
			}
			event = nil
		} else if strings.HasPrefix(line, "data:") {
			event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return nil, errors.New("MCP stream ended without a response; outcome may be unknown")
}

// response reads one JSON-RPC message: done with the result when it answers
// id, not done for a notification, and an error for anything else. Remote
// error messages are never surfaced, only their code.
func response(b []byte, id int64) (map[string]interface{}, bool, error) {
	var msg envelope
	if json.Unmarshal(b, &msg) != nil || msg.JSONRPC != "2.0" {
		return nil, false, errors.New("invalid MCP JSON-RPC envelope")
	}
	return msg.answer(id)
}

func (msg envelope) answer(id int64) (map[string]interface{}, bool, error) {
	if msg.Method != "" {
		if msg.ID != nil {
			return nil, false, errors.New("server-initiated MCP requests are not supported")
		}
		return nil, false, nil
	}
	if msg.ID == nil || *msg.ID != id {
		return nil, false, errors.New("MCP response ID mismatch")
	}
	if msg.Error != nil {
		return nil, false, fmt.Errorf("MCP protocol error %d", msg.Error.Code)
	}
	if msg.Result == nil {
		return nil, false, errors.New("MCP response missing result")
	}
	return msg.Result, true, nil
}

// do sends one request with the session's auth headers. Errors carry no
// response body, and transport errors are redacted.
func (s *Session) do(ctx context.Context, method, target string, body []byte, headers http.Header) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.New("invalid MCP request")
	}
	req.Header = headers
	for k, v := range s.headers {
		req.Header[k] = v
	}
	resp, err := s.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP request failed; outcome may be unknown; do not blindly replay writes: %s", s.redact.string(err.Error()))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && s.id != "" {
			return nil, ErrSessionExpired
		}
		return nil, fmt.Errorf("MCP server returned HTTP %d; no automatic retry", resp.StatusCode)
	}
	return resp, nil
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, errors.New("MCP response read failed; outcome may be unknown")
	}
	if len(b) > MaxBytes {
		return nil, errors.New("MCP response exceeds 2 MiB; outcome may be unknown")
	}
	return b, nil
}
