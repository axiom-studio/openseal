package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func schemaOf(s string) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func lookupTool() map[string]interface{} {
	return map[string]interface{}{
		"name": "lookup", "description": "Read a value", "annotations": map[string]interface{}{"readOnlyHint": true},
		"inputSchema":  schemaOf(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`),
		"outputSchema": schemaOf(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`),
	}
}

// tlsServer is an HTTPS test server; its Client() is the injected host client.
func tlsServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	// Parallel dials still in their handshake at Close are expected.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}

type rpcError int
type rawBody string

// mcpServer answers JSON-RPC over Streamable HTTP. handle returns a result,
// an rpcError, or a rawBody written verbatim.
func mcpServer(t *testing.T, handle func(r *http.Request, method string, params map[string]interface{}) interface{}) *httptest.Server {
	server := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]interface{})
		if msg["id"] == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := handle(r, method, params)
		if method == "initialize" {
			if m, ok := result.(map[string]interface{}); !ok || m["protocolVersion"] == nil {
				result = map[string]interface{}{"protocolVersion": "2025-11-25", "capabilities": map[string]interface{}{"tools": map[string]interface{}{}}}
			}
			w.Header().Set("Mcp-Session-Id", "session-1")
		}
		w.Header().Set("Content-Type", "application/json")
		switch x := result.(type) {
		case rawBody:
			fmt.Fprint(w, x)
		case rpcError:
			json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": msg["id"], "error": map[string]interface{}{"code": int(x), "message": "boom secret-token"}})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": msg["id"], "result": result})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func pin(server *httptest.Server) *BindingConfig {
	return &BindingConfig{Endpoint: server.URL + "/mcp", Transport: TransportStreamableHTTP, Auth: Auth{Kind: AuthBearer}}
}

// lookupInput is the read and call input the kernel compiles for the lookup
// tool pinned at Digest(tool).
func lookupInput(tool map[string]interface{}, key interface{}) Input {
	return Input{Tool: "lookup", Hash: Digest(tool), Arguments: map[string]interface{}{"key": key}}
}

func newClient(t *testing.T, config *BindingConfig, server *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(config, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func toolsServer(t *testing.T, tool map[string]interface{}, calls *atomic.Int32, isError bool) *httptest.Server {
	return mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		if method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "session-1" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25") {
			t.Error("session or version not propagated")
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("bearer token missing")
		}
		switch method {
		case "initialize":
			return nil
		case "tools/list":
			if params["cursor"] == nil {
				return map[string]interface{}{"tools": []interface{}{}, "nextCursor": "page-2"}
			}
			return map[string]interface{}{"tools": []interface{}{tool}}
		case "tools/call":
			calls.Add(1)
			if params["name"] != "lookup" {
				t.Error("wrong tool")
			}
			return map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "text", "text": "echo secret-token"}}, "structuredContent": map[string]interface{}{"value": 7}, "isError": isError}
		}
		t.Errorf("unexpected %s", method)
		return nil
	})
}

var secrets = Secrets{SecretAccess: "secret-token"}

func TestNegotiationPaginationAndCall(t *testing.T) {
	var calls atomic.Int32
	server := toolsServer(t, lookupTool(), &calls, false)
	c := newClient(t, pin(server), server)
	result, err := c.Call(t.Context(), secrets, Read, lookupInput(lookupTool(), "a"))
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	if text := result.Content[0].(map[string]interface{})["text"]; text != "echo [REDACTED]" {
		t.Fatalf("content not redacted: %v", text)
	}
	if result.StructuredContent.(map[string]interface{})["value"] != float64(7) {
		t.Fatal("structured content lost")
	}
}

func TestCallRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	defer func(limit int) { callLimit = limit }(callLimit)
	callLimit = 4 << 20
	var calls atomic.Int32
	server := toolsServer(t, lookupTool(), &calls, false)
	c := newClient(t, pin(server), server)
	hash := Digest(lookupTool())
	for name, tc := range map[string]struct {
		action string
		input  Input
		want   string
	}{
		"bad arguments":   {Read, lookupInput(lookupTool(), 1), "input schema"},
		"extra argument":  {Read, Input{Tool: "lookup", Hash: hash, Arguments: map[string]interface{}{"key": "a", "x": 1}}, "input schema"},
		"unlisted tool":   {Read, Input{Tool: "other", Hash: hash}, "no longer offered"},
		"stale hash":      {Read, Input{Tool: "lookup", Hash: strings.Repeat("0", 64), Arguments: map[string]interface{}{"key": "a"}}, "tool definition changed; reconnect to refresh"},
		"unknown action":  {"delete", lookupInput(lookupTool(), "a"), "unknown MCP action"},
		"oversized input": {Call, lookupInput(lookupTool(), strings.Repeat("x", 4<<20)), "oversized"},
	} {
		if _, err := c.Call(t.Context(), secrets, tc.action, tc.input); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid call reached tools/call")
	}
	if _, err := c.Call(t.Context(), secrets, Call, lookupInput(lookupTool(), "a")); err != nil {
		t.Fatalf("call of write tool: %v", err)
	}
}

func TestDriftAndToolErrors(t *testing.T) {
	for _, drift := range []bool{false, true} {
		var calls atomic.Int32
		remote := lookupTool()
		if drift {
			remote["description"] = "changed"
		}
		server := toolsServer(t, remote, &calls, true)
		_, err := newClient(t, pin(server), server).Call(t.Context(), secrets, Read, lookupInput(lookupTool(), "a"))
		if err == nil {
			t.Fatal("expected failure")
		}
		if drift && !strings.Contains(err.Error(), "tool definition changed; reconnect to refresh") {
			t.Fatalf("drift error = %v", err)
		}
		if drift && calls.Load() != 0 {
			t.Fatal("drifted tool was invoked")
		}
	}
}

func TestVerifyReportsDriftAndCallsOnlyVerifiedTools(t *testing.T) {
	var calls atomic.Int32
	remote := lookupTool()
	remote["description"] = "changed"
	server := toolsServer(t, remote, &calls, false)
	s, err := newClient(t, pin(server), server).Open(t.Context(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := lookupInput(lookupTool(), "a")
	if _, err := s.Call(t.Context(), Read, input); err == nil {
		t.Fatal("unverified tool called")
	}
	drift, err := s.Verify(t.Context(), map[string]string{"lookup": input.Hash, "gone": input.Hash})
	if err != nil || len(drift) != 2 || drift[0] != "gone" || drift[1] != "lookup" {
		t.Fatalf("drift=%v err=%v", drift, err)
	}
	if _, err := s.Call(t.Context(), Read, input); err == nil || !strings.Contains(err.Error(), "tool definition changed; reconnect to refresh") || calls.Load() != 0 {
		t.Fatalf("drifted tool called: %v", err)
	}
	// The live definition verifies under its own hash.
	if drift, err := s.Verify(t.Context(), map[string]string{"lookup": Digest(remote)}); err != nil || len(drift) != 0 {
		t.Fatalf("drift=%v err=%v", drift, err)
	}
	if _, err := s.Call(t.Context(), Read, lookupInput(remote, "a")); err != nil || calls.Load() != 1 {
		t.Fatalf("verified tool: %v", err)
	}
}

func TestOutputSchemaMismatchFails(t *testing.T) {
	server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		switch method {
		case "tools/list":
			return map[string]interface{}{"tools": []interface{}{lookupTool()}}
		case "tools/call":
			return map[string]interface{}{"content": []interface{}{}, "structuredContent": map[string]interface{}{"value": "seven"}}
		}
		return nil
	})
	if _, err := newClient(t, pin(server), server).Call(t.Context(), secrets, Read, lookupInput(lookupTool(), "a")); err == nil || !strings.Contains(err.Error(), "output schema") {
		t.Fatalf("mismatched structuredContent accepted: %v", err)
	}
}

func TestPaginationLimits(t *testing.T) {
	tool := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "inputSchema": map[string]interface{}{"type": "object"}}
	}
	for name, page := range map[string]func(cursor string) interface{}{
		"repeated cursor": func(string) interface{} {
			return map[string]interface{}{"tools": []interface{}{}, "nextCursor": "same"}
		},
		"duplicate name": func(cursor string) interface{} {
			if cursor == "" {
				return map[string]interface{}{"tools": []interface{}{tool("a")}, "nextCursor": "2"}
			}
			return map[string]interface{}{"tools": []interface{}{tool("a")}}
		},
		"too many pages": func(cursor string) interface{} {
			return map[string]interface{}{"tools": []interface{}{}, "nextCursor": cursor + "x"}
		},
		"too many tools": func(cursor string) interface{} {
			tools := []interface{}{}
			for i := 0; i < 200; i++ {
				tools = append(tools, tool(fmt.Sprintf("t%s-%d", cursor, i)))
			}
			return map[string]interface{}{"tools": tools, "nextCursor": cursor + "x"}
		},
		"empty cursor": func(string) interface{} {
			return map[string]interface{}{"tools": []interface{}{}, "nextCursor": ""}
		},
		"not a list": func(string) interface{} { return map[string]interface{}{"tools": "nope"} },
	} {
		server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
			if method == "tools/list" {
				cursor, _ := params["cursor"].(string)
				return page(cursor)
			}
			return nil
		})
		if _, err := newClient(t, pin(server), server).Discover(t.Context(), secrets); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestStreamingResponseDoesNotWaitForEOF(t *testing.T) {
	server := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]interface{}
		json.NewDecoder(r.Body).Decode(&msg)
		if msg["id"] == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		result := `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}`
		if msg["method"] == "tools/list" {
			result = `{"tools":[]}`
		}
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%v,\"result\":%s}\n\n", msg["id"], result)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	tools, err := newClient(t, pin(server), server).Discover(t.Context(), secrets)
	if err != nil || len(tools) != 0 {
		t.Fatal(tools, err)
	}
}

func TestRPCRejectsWrongIDsAndServerRequests(t *testing.T) {
	for _, body := range []string{`{"jsonrpc":"2.0","id":2,"result":{}}`, `{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"token-secret"}}`, `{"id":1,"result":{}}`} {
		_, _, err := response([]byte(body), 1)
		if err == nil || strings.Contains(err.Error(), "token-secret") {
			t.Fatal("invalid envelope accepted or error leaked")
		}
	}
}

func TestErrorsNeverCarryRemoteBodiesOrSecrets(t *testing.T) {
	for name, handle := range map[string]func(w http.ResponseWriter){
		"http error": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "internal secret-token failure")
		},
		"redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://elsewhere.example/secret-token")
			w.WriteHeader(http.StatusFound)
		},
		"oversized": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, strings.Repeat(" ", MaxBytes+1))
		},
		"wrong content type": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<p>secret-token</p>")
		},
	} {
		server := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handle(w) }))
		_, err := newClient(t, pin(server), server).Discover(t.Context(), secrets)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "failure") {
			t.Errorf("%s: %v", name, err)
		}
	}
	server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		if method == "tools/list" {
			return rpcError(-32000)
		}
		return nil
	})
	_, err := newClient(t, pin(server), server).Discover(t.Context(), secrets)
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "boom") {
		t.Fatalf("protocol error leaked: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		return map[string]interface{}{"tools": []interface{}{}}
	})
	expired := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer expired.Close()
	s, err := newClient(t, pin(expired), expired).Open(t.Context(), secrets)
	if err == nil {
		defer s.Close()
	}
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestMCP2026Metadata(t *testing.T) {
	tool := lookupTool()
	tool["inputSchema"] = schemaOf(`{"type":"object","properties":{"key":{"type":"string","x-mcp-header":"Key"}},"required":["key"]}`)
	var requests atomic.Int32
	server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		requests.Add(1)
		meta, _ := params["_meta"].(map[string]interface{})
		if method == "initialize" || r.Header.Get("Mcp-Method") != method || r.Header.Get("MCP-Protocol-Version") != ProtocolVersion2026 ||
			meta["io.modelcontextprotocol/protocolVersion"] != ProtocolVersion2026 || r.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("modern metadata missing or legacy initialize sent for %s", method)
		}
		if method == "tools/call" {
			if r.Header.Get("Mcp-Name") != "lookup" || r.Header.Get("Mcp-Param-Key") != "=?base64?SGVsbG8sIOS4lueVjA==?=" {
				t.Errorf("call headers: %v", r.Header)
			}
			return map[string]interface{}{"content": []interface{}{}, "structuredContent": map[string]interface{}{"value": 1}}
		}
		return map[string]interface{}{"tools": []interface{}{tool}}
	})
	config := pin(server)
	config.ProtocolVersion = ProtocolVersion2026
	if _, err := newClient(t, config, server).Call(t.Context(), secrets, Read, lookupInput(tool, "Hello, 世界")); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want tools/list and tools/call only", requests.Load())
	}
}

// sseServer is a legacy SSE server: GET opens the stream, POSTs to
// /messages are answered on it.
func sseServer(t *testing.T, endpoint string) *httptest.Server {
	events := make(chan string, 16)
	server := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case event := <-events:
					fmt.Fprint(w, event)
					w.(http.Flusher).Flush()
				}
			}
		}
		if r.URL.Path != "/messages" || r.URL.Query().Get("session") != "fixture" {
			t.Errorf("wrong legacy message endpoint %s", r.URL)
		}
		var msg map[string]interface{}
		json.NewDecoder(r.Body).Decode(&msg)
		result := ""
		switch msg["method"] {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}}}`
		case "tools/list":
			result = `{"tools":[]}`
		}
		if result != "" {
			events <- "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n"
			events <- fmt.Sprintf("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%v,\"result\":%s}\n\n", msg["id"], result)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLegacySSETransport(t *testing.T) {
	server := sseServer(t, "/messages?session=fixture")
	config := pin(server)
	config.Transport, config.ProtocolVersion = TransportSSE, "2024-11-05"
	s, err := newClient(t, config, server).Open(t.Context(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Tools(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestLegacySSEEndpointMustStayOnOrigin(t *testing.T) {
	server := sseServer(t, "https://elsewhere.example/messages")
	config := pin(server)
	config.Transport = TransportSSE
	if _, err := newClient(t, config, server).Open(t.Context(), secrets); err == nil {
		t.Fatal("cross-origin message endpoint accepted")
	}
}

func TestAuthenticationHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		auth    Auth
		headers []SecretHeader
		secrets Secrets
		want    map[string]string
	}{
		"none":    {Auth{Kind: AuthNone}, nil, nil, map[string]string{"Authorization": ""}},
		"bearer":  {Auth{Kind: AuthBearer}, nil, Secrets{SecretAccess: "tok"}, map[string]string{"Authorization": "Bearer tok"}},
		"oauth":   {Auth{Kind: AuthOAuth}, nil, Secrets{SecretAccess: "at"}, map[string]string{"Authorization": "Bearer at"}},
		"api-key": {Auth{Kind: AuthAPIKey, Header: "X-Api-Key", Prefix: "Key "}, nil, Secrets{SecretAccess: "k"}, map[string]string{"X-Api-Key": "Key k", "Authorization": ""}},
		"basic":   {Auth{Kind: AuthBasic}, []SecretHeader{{Name: "X-Workspace", Field: "workspace"}}, Secrets{SecretUsername: "kev", SecretPassword: "private", "workspace": "ws"}, map[string]string{"Authorization": "Basic a2V2OnByaXZhdGU=", "X-Workspace": "ws"}},
	} {
		server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
			for header, value := range tc.want {
				if r.Header.Get(header) != value {
					t.Errorf("%s: %s = %q, want %q", name, header, r.Header.Get(header), value)
				}
			}
			return map[string]interface{}{"tools": []interface{}{}}
		})
		config := pin(server)
		config.Auth, config.SecretHeaders = tc.auth, tc.headers
		if _, err := newClient(t, config, server).Discover(t.Context(), tc.secrets); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	server := mcpServer(t, func(*http.Request, string, map[string]interface{}) interface{} {
		t.Error("request sent with invalid secrets")
		return nil
	})
	config := pin(server)
	config.Auth = Auth{Kind: AuthBasic}
	for _, bad := range []Secrets{nil, {SecretUsername: "a:b", SecretPassword: "p"}, {SecretUsername: "u", SecretPassword: "p\r\nX-Evil: 1"}} {
		if _, err := newClient(t, config, server).Open(t.Context(), bad); err == nil {
			t.Errorf("secrets %v accepted", bad)
		}
	}
}

// Two sessions of one Client with different secrets run concurrently; every
// request carries only its own session's credential, and every result is the
// right tenant's.
func TestConcurrentSessionsNeverCrossSecrets(t *testing.T) {
	var mu sync.Mutex
	owners := map[string]string{}
	var next atomic.Int32
	tool := lookupTool()
	server := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		var msg map[string]interface{}
		json.NewDecoder(r.Body).Decode(&msg)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		session := r.Header.Get("Mcp-Session-Id")
		if msg["method"] == "initialize" {
			session = fmt.Sprintf("s%d", next.Add(1))
			mu.Lock()
			owners[session] = token
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		mu.Lock()
		owner := owners[session]
		mu.Unlock()
		if owner != token || r.Header.Get("X-Tenant") != strings.TrimSuffix(token, "-secret")+"-workspace" {
			t.Errorf("session %s owned by %q received %q", session, owner, token)
		}
		if msg["id"] == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result interface{}
		switch msg["method"] {
		case "initialize":
			result = map[string]interface{}{"protocolVersion": "2025-11-25", "capabilities": map[string]interface{}{"tools": map[string]interface{}{}}}
		case "tools/list":
			result = map[string]interface{}{"tools": []interface{}{tool}}
		case "tools/call":
			result = map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "text", "text": "for " + strings.TrimSuffix(token, "-secret")}}, "structuredContent": map[string]interface{}{"value": 1}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": msg["id"], "result": result})
	}))
	defer server.Close()
	config := pin(server)
	config.SecretHeaders = []SecretHeader{{Name: "X-Tenant", Field: "tenant"}}
	c := newClient(t, config, server)
	var wg sync.WaitGroup
	for _, tenant := range []string{"alpha", "beta"} {
		s, err := c.Open(t.Context(), Secrets{SecretAccess: tenant + "-secret", "tenant": tenant + "-workspace"})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if drift, err := s.Verify(t.Context(), map[string]string{"lookup": Digest(tool)}); err != nil || len(drift) != 0 {
			t.Fatal(drift, err)
		}
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := s.Call(t.Context(), Read, lookupInput(tool, "k"))
				if err != nil {
					t.Error(err)
					return
				}
				if got := result.Content[0].(map[string]interface{})["text"]; got != "for "+tenant {
					t.Errorf("%s session got %v", tenant, got)
				}
			}()
		}
	}
	wg.Wait()
}

func TestDiscoveryRedactionPreservesSchemaProperties(t *testing.T) {
	tool := map[string]interface{}{"inputSchema": schemaOf(`{"type":"object","properties":{"token":{"type":"string"}}}`), "description": "echo credential-value"}
	original := Digest(tool)
	safe := newRedactor("credential-value").value(tool, false).(map[string]interface{})
	if safe["description"] != "echo [REDACTED]" {
		t.Fatal("credential not redacted")
	}
	if _, err := compileSchema(safe["inputSchema"].(map[string]interface{})); err != nil {
		t.Fatal("schema corrupted by redaction", err)
	}
	if Digest(tool) != original {
		t.Fatal("redaction modified original tool contract")
	}
}

func TestNewClientRequiresHostHTTPClient(t *testing.T) {
	config := &BindingConfig{Endpoint: "https://mcp.example/mcp", Transport: TransportStreamableHTTP, Auth: Auth{Kind: AuthNone}}
	if _, err := NewClient(config, Options{}); err == nil {
		t.Fatal("client built without the host's HTTP client")
	}
}

// A tools/call may carry files (screenshots, PDFs) well past the 2 MiB bound
// of other messages, in its arguments and its result; the result names the
// tool's output schema so a host can keep the declared file fields.
func TestToolCallsCarryLargeFiles(t *testing.T) {
	image := strings.Repeat("A", 3<<20)
	tool := lookupTool()
	tool["inputSchema"] = schemaOf(`{"type":"object","properties":{"key":{"type":"string","contentEncoding":"base64"}},"required":["key"]}`)
	server := mcpServer(t, func(r *http.Request, method string, params map[string]interface{}) interface{} {
		switch method {
		case "tools/list":
			return map[string]interface{}{"tools": []interface{}{tool}}
		case "tools/call":
			arguments, _ := params["arguments"].(map[string]interface{})
			if arguments["key"] != image {
				t.Error("large argument lost")
			}
			return map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "image", "data": image, "mimeType": "image/png"}}, "structuredContent": map[string]interface{}{"value": 1}}
		}
		return nil
	})
	result, err := newClient(t, pin(server), server).Call(t.Context(), secrets, Read, lookupInput(tool, image))
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].(map[string]interface{})["data"] != image || result.OutputSchema["required"] == nil {
		t.Fatalf("result = %d content, output schema %v", len(result.Content), result.OutputSchema)
	}
	if messageLimit("tools/list") != MaxBytes || messageLimit("tools/call") != MaxCallBytes || MaxCallBytes < 600<<20 {
		t.Fatal("only tools/call takes large messages")
	}
}
