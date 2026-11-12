package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

const stepModel = "deepseek-chat"

// fakeProvider is an httptest Chat Completions endpoint. reply answers each
// call; n counts calls; bodies keeps every request body.
type fakeProvider struct {
	*httptest.Server
	n      atomic.Int32
	bodies [][]byte
}

func newFakeProvider(t *testing.T, reply func(call int, w http.ResponseWriter, body map[string]interface{})) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer private-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing provider headers")
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode body: %v", err)
		}
		p.bodies = append(p.bodies, raw)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		reply(int(p.n.Add(1)), w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeProvider) host(t *testing.T, window int64) *ProviderStepHost {
	t.Helper()
	host, err := NewProviderStepHost(OpenAIProviderConfig{Endpoint: p.URL + "/v1", Key: "private-key", Model: stepModel, ContextWindowTokens: window})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func chatReply(w http.ResponseWriter, finish string, message map[string]interface{}, usage map[string]int) {
	response := map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": finish, "message": message}}}
	if usage != nil {
		response["usage"] = usage
	}
	_ = json.NewEncoder(w).Encode(response)
}

func chatCall(id, name, arguments string) map[string]interface{} {
	return map[string]interface{}{"id": id, "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}
}

func stepTool(name string) models.ModelStepTool {
	return models.ModelStepTool{Name: name, Description: "Look something up", Group: models.ModelStepToolHarness,
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}}}
}

func stepRequest(tools ...models.ModelStepTool) models.ModelStepRequest {
	return models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion, Purpose: models.ModelStepPurposeStep,
		Scope: models.Scope{Kind: "local", ID: "default"}, RunID: "run-1", StepID: "step-1", SessionID: "session-1", AgentID: "agent-1",
		System:  models.ModelStepSystem{AgentInstructions: []string{"Be brief."}},
		Tools:   tools,
		Entries: []models.SessionEntryView{userView("u1", 1, "What is two plus two?")},
	}
}

func failureOf(t *testing.T, err error) *modelstep.TurnHostFailure {
	t.Helper()
	var failure *modelstep.TurnHostFailure
	if !errors.As(err, &failure) {
		t.Fatalf("want *TurnHostFailure, got %T %v", err, err)
	}
	return failure
}

func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestStepHostSendsRenderedRequestAndReturnsOneAssistantEntry(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{"content": "Looking it  up\nnow.", "tool_calls": []interface{}{chatCall("call-a", "lookup", `{ "q" : "2+2" }`)}},
			map[string]int{"prompt_tokens": 120, "completion_tokens": 7})
	})
	req := stepRequest(stepTool("lookup"))
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{}
	_ = json.Unmarshal(provider.bodies[0], &body)
	if got, want := keysOf(body), []string{"messages", "model", "stream", "tool_choice", "tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body keys %v, want %v (no max_completion_tokens when 0)", got, want)
	}
	if body["model"] != stepModel || body["stream"] != false || body["tool_choice"] != "auto" {
		t.Fatalf("body: %v", body)
	}
	render, err := renderOpenAIStep(req, stepModel)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages, _ := json.Marshal(render.Messages)
	gotMessages, _ := json.Marshal(body["messages"])
	var a, b interface{}
	_ = json.Unmarshal(wantMessages, &a)
	_ = json.Unmarshal(gotMessages, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("messages are not the rendered path:\n%s\n%s", gotMessages, wantMessages)
	}
	if strings.Contains(string(provider.bodies[0]), "private-key") {
		t.Fatal("credential leaked into the request body")
	}

	if result.APIVersion != models.ModelStepAPIVersion || result.StepID != "step-1" {
		t.Fatalf("identity: %+v", result)
	}
	assistant := result.Assistant
	if assistant.Provider != models.ModelStepProviderChat || assistant.Model != stepModel || assistant.StopReason != models.ModelStopToolCalls ||
		assistant.Text != "Looking it  up\nnow." || assistant.SystemDigest == "" || assistant.ToolsDigest == "" || assistant.Limits != nil {
		t.Fatalf("assistant: %+v", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-a" || assistant.ToolCalls[0].Name != "lookup" ||
		string(assistant.ToolCalls[0].Arguments) != `{"q":"2+2"}` || assistant.ToolCalls[0].ArgumentsError != "" {
		t.Fatalf("tool calls: %+v", assistant.ToolCalls)
	}
	if result.Narration != "Looking it up now." {
		t.Fatalf("narration %q", result.Narration)
	}
	if assistant.Usage.InputTokens != 120 || assistant.Usage.OutputTokens != 7 || assistant.Usage.Attempts != 1 {
		t.Fatalf("usage: %+v", assistant.Usage)
	}
	if result.Model.Provider != models.ModelStepProviderChat || result.Model.Model != stepModel || result.Model.ContextTokens != defaultContextWindowTokens {
		t.Fatalf("model info: %+v", result.Model)
	}
	if result.Render.EstimatedInputTokens != int64(len(provider.bodies[0])+3)/4 {
		t.Fatalf("estimate %d for %d bytes", result.Render.EstimatedInputTokens, len(provider.bodies[0]))
	}
	if err := modelstep.ValidateModelStepResult(req, result); err != nil {
		t.Fatal(err)
	}

	// A Run limit is sent as max_completion_tokens.
	req.MaxOutputTokens = 2048
	if _, err := provider.host(t, 0).ExecuteModelStep(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(provider.bodies[1], &body)
	if body["max_completion_tokens"] != float64(2048) {
		t.Fatalf("max_completion_tokens %v", body["max_completion_tokens"])
	}
}

func TestStepHostToolFreeStepOmitsToolsAndToolChoice(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		// A stray call on a tool-free step is dropped.
		chatReply(w, "stop", map[string]interface{}{"content": "Four.", "tool_calls": []interface{}{chatCall("x", "lookup", `{}`)}}, nil)
	})
	req := stepRequest()
	req.Purpose = models.ModelStepPurposeExplanation
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{}
	_ = json.Unmarshal(provider.bodies[0], &body)
	if got, want := keysOf(body), []string{"messages", "model", "stream"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body keys %v, want %v", got, want)
	}
	if result.Assistant.Text != "Four." || len(result.Assistant.ToolCalls) != 0 || result.Assistant.StopReason != models.ModelStopEnd || result.Narration != "" {
		t.Fatalf("result: %+v", result)
	}
}

func TestStepHostMapsFinishReasonsToStopReasons(t *testing.T) {
	for _, tc := range []struct {
		finish string
		calls  bool
		want   models.ModelStopReason
	}{
		{"stop", false, models.ModelStopEnd},
		{"", false, models.ModelStopEnd},
		{"something_else", false, models.ModelStopEnd},
		{"length", false, models.ModelStopMaxTokens},
		{"content_filter", false, models.ModelStopRefusal},
		{"tool_calls", true, models.ModelStopToolCalls},
		{"stop", true, models.ModelStopToolCalls},
	} {
		t.Run(tc.finish+map[bool]string{true: "+calls"}[tc.calls], func(t *testing.T) {
			provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
				message := map[string]interface{}{"content": "Partial answer"}
				if tc.calls {
					message["tool_calls"] = []interface{}{chatCall("c1", "lookup", `{"q":"x"}`)}
				}
				chatReply(w, tc.finish, message, nil)
			})
			result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
			if err != nil {
				t.Fatal(err)
			}
			if result.Assistant.StopReason != tc.want {
				t.Fatalf("stop %q, want %q", result.Assistant.StopReason, tc.want)
			}
		})
	}
}

func TestStepHostSynthesizesDeterministicToolCallIDs(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{"content": nil, "tool_calls": []interface{}{
			chatCall("", "lookup", `{"q":"a"}`), chatCall("same", "lookup", `{"q":"b"}`), chatCall("same", "lookup", `{"q":"c"}`),
		}}, nil)
	})
	host := provider.host(t, 0)
	var ids [][]string
	for range 2 {
		result, err := host.ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
		if err != nil {
			t.Fatal(err)
		}
		var step []string
		for _, call := range result.Assistant.ToolCalls {
			step = append(step, call.ID)
		}
		ids = append(ids, step)
	}
	want := []string{"call_step-1_0", "same", "call_step-1_2"}
	if !reflect.DeepEqual(ids[0], want) || !reflect.DeepEqual(ids[1], want) {
		t.Fatalf("ids %v, want %v twice", ids, want)
	}
}

func TestStepHostAcceptsArrayContentAndEmptyArguments(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{
			"content":    []interface{}{map[string]string{"type": "text", "text": " Checking "}, map[string]string{"type": "image_url", "text": "ignored"}, map[string]string{"type": "text", "text": "the list."}},
			"tool_calls": []interface{}{map[string]interface{}{"id": "c1", "function": map[string]string{"name": "lookup", "arguments": ""}}},
		}, nil)
	})
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
	if err != nil {
		t.Fatal(err)
	}
	if result.Assistant.Text != "Checking the list." || string(result.Assistant.ToolCalls[0].Arguments) != `{}` || result.Assistant.ToolCalls[0].ArgumentsError != "" {
		t.Fatalf("result: %+v", result.Assistant)
	}

	invalid := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "stop", map[string]interface{}{"content": map[string]string{"text": "object content"}}, nil)
	})
	_, err = invalid.host(t, 0).ExecuteModelStep(context.Background(), stepRequest())
	if failureOf(t, err).Code != stepHostInvalidResponse {
		t.Fatalf("object content: %v", err)
	}
}

func TestStepHostRetriesMalformedArgumentsTwiceThenFlagsThem(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{"content": "Looking.", "tool_calls": []interface{}{
			chatCall("good", "lookup", `{"q":"x"}`), chatCall("bad", "lookup", `{"q":`), chatCall("list", "lookup", `[1]`),
		}}, map[string]int{"prompt_tokens": 100, "completion_tokens": 10})
	})
	req := stepRequest(stepTool("lookup"))
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if provider.n.Load() != 3 {
		t.Fatalf("provider calls %d, want 3", provider.n.Load())
	}
	if string(provider.bodies[0]) != string(provider.bodies[1]) || string(provider.bodies[1]) != string(provider.bodies[2]) {
		t.Fatal("a correction retry must re-send the identical request")
	}
	calls := result.Assistant.ToolCalls
	if len(calls) != 3 || calls[0].ArgumentsError != "" || string(calls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("calls: %+v", calls)
	}
	for _, call := range calls[1:] {
		if string(call.Arguments) != `{}` || !strings.HasPrefix(call.ArgumentsError, "lookup: arguments are not a JSON object (") {
			t.Fatalf("malformed call: %+v", call)
		}
	}
	if u := result.Assistant.Usage; u.Attempts != 3 || u.InputTokens != 300 || u.OutputTokens != 30 {
		t.Fatalf("usage of all attempts: %+v", u)
	}
	if err := modelstep.ValidateModelStepResult(req, result); err != nil {
		t.Fatal(err)
	}
}

func TestStepHostTruncatedToolCallFailsWithoutRetry(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "length", map[string]interface{}{"content": "", "tool_calls": []interface{}{chatCall("c1", "lookup", `{"q":"unfinish`)}}, nil)
	})
	_, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
	if failure := failureOf(t, err); failure.Code != stepHostOutputTruncated || failure.Retryable {
		t.Fatalf("failure: %+v", failure)
	}
	if provider.n.Load() != 1 {
		t.Fatalf("provider calls %d, want 1", provider.n.Load())
	}
}

func TestStepHostRetriesEmptyAnswerOnceThenFails(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "stop", map[string]interface{}{"content": "  "}, nil)
	})
	_, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest())
	if failure := failureOf(t, err); failure.Code != stepHostEmptyResponse || failure.Retryable || provider.n.Load() != 2 {
		t.Fatalf("failure %+v after %d calls", failure, provider.n.Load())
	}
}

func TestStepHostClassifiesProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		code      string
		retryable bool
	}{
		{"rate-limited", 429, `{"error":{"message":"slow down"}}`, stepHostHTTP, true},
		{"unavailable", 503, `private-key upstream`, stepHostHTTP, true},
		{"context-code", 400, `{"error":{"code":"context_length_exceeded","message":"x"}}`, modelstep.TurnHostContextExceeded, false},
		{"context-text", 400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, modelstep.TurnHostContextExceeded, false},
		{"context-413", 413, `Prompt is too long`, modelstep.TurnHostContextExceeded, false},
		{"bad-request", 400, `{"error":{"message":"unknown parameter"}}`, stepHostHTTP, false},
		{"unauthorized", 401, `private-key rejected`, stepHostHTTP, false},
		{"not-json", 200, `not json`, stepHostInvalidResponse, false},
		{"two-choices", 200, `{"choices":[{},{}]}`, stepHostInvalidResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := provider.host(t, 9000).ExecuteModelStep(context.Background(), stepRequest())
			failure := failureOf(t, err)
			if failure.Code != tc.code || failure.Retryable != tc.retryable || errors.Is(err, modelstep.ErrTurnHostUnavailable) != tc.retryable {
				t.Fatalf("failure %+v, want %s retryable=%v", failure, tc.code, tc.retryable)
			}
			if strings.Contains(failure.Message, "private-key") {
				t.Fatalf("provider body leaked: %q", failure.Message)
			}
			if tc.code == modelstep.TurnHostContextExceeded {
				if failure.ContextPressure == nil || failure.ContextPressure.LimitTokens != 9000 || failure.ContextPressure.EstimatedTokens <= 0 {
					t.Fatalf("context pressure: %+v", failure.ContextPressure)
				}
			}
		})
	}

	t.Run("transport", func(t *testing.T) {
		provider := newFakeProvider(t, func(int, http.ResponseWriter, map[string]interface{}) {})
		host := provider.host(t, 0)
		provider.Close()
		_, err := host.ExecuteModelStep(context.Background(), stepRequest())
		if failure := failureOf(t, err); failure.Code != stepHostUnavailable || !failure.Retryable {
			t.Fatalf("failure: %+v", failure)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Bool
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
		defer other.Close()
		provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
			w.Header().Set("Location", other.URL)
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		_, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest())
		if failure := failureOf(t, err); failure.Code != stepHostHTTP || redirected.Load() {
			t.Fatalf("redirect followed or misclassified: %+v %v", failure, redirected.Load())
		}
	})
}

func TestStepHostEstimatesMissingUsage(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{"content": "Four", "tool_calls": []interface{}{chatCall("c1", "lookup", `{"q":"x"}`)}}, nil)
	})
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(provider.bodies[0])+3) / 4; result.Assistant.Usage.InputTokens != want {
		t.Fatalf("input estimate %d, want %d", result.Assistant.Usage.InputTokens, want)
	}
	if want := int64(len("Four") + len(`{"q":"x"}`)); result.Assistant.Usage.OutputTokens != want {
		t.Fatalf("output estimate %d, want %d", result.Assistant.Usage.OutputTokens, want)
	}
}

func TestStepHostCapturesReasoningContent(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "tool_calls", map[string]interface{}{"content": "Checking.", "reasoning_content": " I should look it up. ",
			"tool_calls": []interface{}{chatCall("c1", "lookup", `{"q":"x"}`)}}, nil)
	})
	result, err := provider.host(t, 0).ExecuteModelStep(context.Background(), stepRequest(stepTool("lookup")))
	if err != nil {
		t.Fatal(err)
	}
	native := result.Assistant.Native
	if native == nil || native.ChatReasoningField != "reasoning_content" || native.ChatReasoning != "I should look it up." || len(native.Anthropic)+len(native.Responses) != 0 {
		t.Fatalf("native: %+v", native)
	}

	// The captured entry replays its reasoning on the next step of the same loop.
	entry := models.SessionEntryView{ID: "a1", Sequence: 2, Type: models.SessionEntryAssistant, Assistant: &result.Assistant}
	tool := toolView("t1", 3, "c1", models.ToolEntryCompleted, textPart("4"))
	next := stepRequest(stepTool("lookup"))
	next.StepID = "step-2"
	next.Entries = append(next.Entries, entry, tool)
	if _, err := provider.host(t, 0).ExecuteModelStep(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(provider.bodies[1]), `"reasoning_content":"I should look it up."`) {
		t.Fatalf("reasoning not replayed: %s", provider.bodies[1])
	}
}

func TestStepHostRejectsMediaAndDeploymentCredentials(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "stop", map[string]interface{}{"content": "unused"}, nil)
	})
	host := provider.host(t, 0)

	credential := stepRequest()
	credential.ModelCredential = &capability.CredentialReference{Kind: "vault", ID: "model"}
	_, err := host.ExecuteModelStep(context.Background(), credential)
	if failure := failureOf(t, err); failure.Code != stepHostUnsupportedBinding || failure.Retryable {
		t.Fatalf("credential: %+v", failure)
	}

	media := stepRequest()
	media.Entries = []models.SessionEntryView{{ID: "u1", Sequence: 1, Type: models.SessionEntryUser, User: &models.UserEntryView{Content: []models.ContentPart{
		{Type: models.ContentPartText, Text: "What is this?"},
		{Type: models.ContentPartImage, MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString([]byte("png"))},
	}}}}
	_, err = host.ExecuteModelStep(context.Background(), media)
	if failure := failureOf(t, err); failure.Code != openAIRenderUnsupportedMedia || failure.Retryable {
		t.Fatalf("media: %+v", failure)
	}

	invalid := stepRequest()
	invalid.StepID = ""
	_, err = host.ExecuteModelStep(context.Background(), invalid)
	if failure := failureOf(t, err); failure.Code != stepHostInvalidRequest {
		t.Fatalf("invalid request: %+v", failure)
	}
	if provider.n.Load() != 0 {
		t.Fatalf("refused steps reached the provider %d times", provider.n.Load())
	}
}

func TestStepHostRejectsOversizedResponseAndHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxProviderResponseBytes+1)))
	}))
	defer server.Close()
	host, err := NewProviderStepHost(OpenAIProviderConfig{Endpoint: server.URL, Key: "key", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.ExecuteModelStep(context.Background(), stepRequest())
	if failure := failureOf(t, err); failure.Code != stepHostInvalidResponse || !strings.Contains(failure.Message, "size limit") {
		t.Fatalf("response bound not enforced: %+v", failure)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.ExecuteModelStep(ctx, stepRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestStepHostHasNoHostTools(t *testing.T) {
	host, err := NewProviderStepHost(OpenAIProviderConfig{Endpoint: "http://localhost:1", Key: "key", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.ExecuteHostTool(context.Background(), models.HostToolRequest{Operation: "workspace.read_file"})
	if !errors.Is(err, models.ErrHostToolUnsupported) || !reflect.DeepEqual(result, models.HostToolResult{}) {
		t.Fatalf("host tool: %+v %v", result, err)
	}
}

func TestStepHostReportsConfiguredOrDefaultWindow(t *testing.T) {
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) {
		chatReply(w, "stop", map[string]interface{}{"content": "Four."}, nil)
	})
	for _, tc := range []struct{ configured, want int64 }{{0, defaultContextWindowTokens}, {32000, 32000}} {
		result, err := provider.host(t, tc.configured).ExecuteModelStep(context.Background(), stepRequest())
		if err != nil {
			t.Fatal(err)
		}
		if result.Model.ContextTokens != tc.want || result.Model.InputLimitTokens != 0 {
			t.Fatalf("window %+v, want context %d", result.Model, tc.want)
		}
	}
}

func TestNewProviderStepHostValidatesConfig(t *testing.T) {
	for _, cfg := range []OpenAIProviderConfig{
		{Endpoint: "ftp://example.com", Key: "k", Model: "m"},
		{Endpoint: "https://user:pass@example.com", Key: "k", Model: "m"},
		{Endpoint: "https://example.com/v1?x=1", Key: "k", Model: "m"},
		{Endpoint: "https://example.com", Key: " ", Model: "m"},
		{Endpoint: "https://example.com", Key: "k", Model: ""},
		{Endpoint: "https://example.com", Key: "k", Model: "m", ContextWindowTokens: -1},
	} {
		if _, err := NewProviderStepHost(cfg); err == nil || err.Error() != "task provider requires a valid HTTP API URL, credential, and model" {
			t.Fatalf("%+v: %v", cfg, err)
		}
	}
	host, err := NewProviderStepHost(OpenAIProviderConfig{Endpoint: "https://example.com/v1/", Key: "k", Model: " m "})
	if err != nil || host.client.endpoint != "https://example.com/v1/chat/completions" || host.client.model != "m" {
		t.Fatalf("host: %+v %v", host, err)
	}
}
