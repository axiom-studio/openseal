package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type failureTestTransport func(*http.Request) (*http.Response, error)

func (f failureTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func failureTestResponse(t *testing.T, body interface{}, status int) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func failureTestTool(t *testing.T, execute func(context.Context, map[string]interface{}) (*ToolResult, error)) []*ToolDefinition {
	t.Helper()
	toolType := "failure_policy_" + t.Name()
	registry := GetGlobalToolRegistry()
	registry.Register(toolType, func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
		return &mockToolExecutor{name: def.Name, executeFunc: execute}, nil
	})
	t.Cleanup(func() { registry.mu.Lock(); defer registry.mu.Unlock(); delete(registry.factories, toolType) })
	return []*ToolDefinition{{Name: "test_action", Config: map[string]interface{}{"type": toolType}, Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}}
}

func failureTestProviderReply(provider string, calls int, text string) interface{} {
	switch provider {
	case "chat":
		message := map[string]interface{}{"content": text}
		if calls > 0 {
			tools := []interface{}{}
			for i := 0; i < calls; i++ {
				tools = append(tools, map[string]interface{}{"id": fmt.Sprintf("call_%d", i), "type": "function", "function": map[string]interface{}{"name": "test_action", "arguments": "{}"}})
			}
			message["tool_calls"] = tools
		}
		return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": message}}}
	case "responses":
		output := []interface{}{}
		if calls > 0 {
			for i := 0; i < calls; i++ {
				output = append(output, map[string]interface{}{"type": "function_call", "call_id": fmt.Sprintf("call_%d", i), "name": "test_action", "arguments": "{}"})
			}
		} else {
			output = append(output, map[string]interface{}{"type": "message", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}}})
		}
		return map[string]interface{}{"id": "response_test", "output": output}
	case "anthropic":
		content := []interface{}{}
		if calls > 0 {
			for i := 0; i < calls; i++ {
				content = append(content, map[string]interface{}{"type": "tool_use", "id": fmt.Sprintf("call_%d", i), "name": "test_action", "input": map[string]interface{}{}})
			}
		} else {
			content = append(content, map[string]interface{}{"type": "text", "text": text})
		}
		return map[string]interface{}{"content": content}
	case "gemini":
		parts := []interface{}{}
		if calls > 0 {
			for i := 0; i < calls; i++ {
				parts = append(parts, map[string]interface{}{"functionCall": map[string]interface{}{"name": "test_action", "args": map[string]interface{}{}}})
			}
		} else {
			parts = append(parts, map[string]interface{}{"text": text})
		}
		return map[string]interface{}{"candidates": []interface{}{map[string]interface{}{"content": map[string]interface{}{"role": "model", "parts": parts}}}}
	case "ollama":
		if calls > 0 {
			text = `{"tool_call":{"name":"test_action","arguments":{}}}`
		}
		return map[string]interface{}{"response": text}
	default:
		panic("unknown test provider")
	}
}

func failureTestRunProvider(provider string, client *http.Client, tools []*ToolDefinition, iterations int) (string, []map[string]interface{}, error) {
	executor := &AIExecutor{client: client}
	var text string
	var history []map[string]interface{}
	var err error
	switch provider {
	case "chat":
		text, _, history, err = (&ChatCompletionsProvider{client: client}).ExecuteToolLoop(context.Background(), "test", "Please help", "Be friendly.", 0, 100, "test-key", "https://test.invalid/chat/completions", tools, &mockResolver{}, iterations)
	case "responses":
		text, _, history, err = (&ResponsesProvider{client: client}).ExecuteToolLoop(context.Background(), "test", "Please help", "Be friendly.", 0, 100, "test-key", "https://test.invalid/responses", tools, &mockResolver{}, iterations)
	case "anthropic":
		text, _, history, err = executor.executeAnthropicToolLoop(context.Background(), "test", "Please help", "Be friendly.", 0, 100, "test-key", tools, &mockResolver{}, iterations)
	case "gemini":
		text, _, history, err = executor.executeGeminiToolLoop(context.Background(), "test", "Please help", "Be friendly.", 0, 100, "test-key", tools, &mockResolver{}, iterations)
	case "ollama":
		text, _, history, err = executor.executePromptToolLoop(context.Background(), "ollama", "test", "Please help", "Be friendly.", 0, 100, "test-key", "https://test.invalid", tools, &mockResolver{}, iterations)
	}
	return text, history, err
}

func TestProviderToolFailureExplainsOnceWithoutRetry(t *testing.T) {
	for _, provider := range []string{"chat", "responses", "anthropic", "gemini", "ollama"} {
		for _, scenario := range []string{"explanation", "tool_again", "provider_failure", "transport_failure"} {
			t.Run(provider+"/"+scenario, func(t *testing.T) {
				executions, requests := 0, 0
				tools := failureTestTool(t, func(context.Context, map[string]interface{}) (*ToolResult, error) {
					executions++
					return nil, errors.New("the image request was rejected")
				})
				batchSize := 2
				if provider == "ollama" {
					batchSize = 1
				}
				client := &http.Client{Transport: failureTestTransport(func(req *http.Request) (*http.Response, error) {
					requests++
					if requests > 2 {
						t.Fatal("made more than one final provider request")
					}
					var body map[string]interface{}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if requests == 1 {
						return failureTestResponse(t, failureTestProviderReply(provider, batchSize, ""), http.StatusOK), nil
					}
					if _, present := body["tools"]; present {
						t.Fatal("final failure explanation still exposes tools")
					}
					raw, _ := json.Marshal(body)
					if !strings.Contains(string(raw), "this attempt has stopped") {
						t.Fatal("final request lacks failure explanation instruction")
					}
					if provider == "chat" || provider == "responses" {
						if body["tool_choice"] != "none" {
							t.Fatal("final request did not forbid tool choice")
						}
					}
					if provider == "ollama" && strings.Contains(fmt.Sprint(body["system"]), "test_action") {
						t.Fatal("final prompt still includes tool definitions")
					}
					switch scenario {
					case "tool_again":
						return failureTestResponse(t, failureTestProviderReply(provider, 1, ""), http.StatusOK), nil
					case "provider_failure":
						return failureTestResponse(t, map[string]interface{}{"error": "unavailable"}, http.StatusServiceUnavailable), nil
					case "transport_failure":
						return nil, errors.New("connection failed")
					default:
						return failureTestResponse(t, failureTestProviderReply(provider, 0, "I couldn’t generate the image because the request was rejected."), http.StatusOK), nil
					}
				})}
				// One normal iteration still allows exactly one failure explanation.
				text, history, err := failureTestRunProvider(provider, client, tools, 1)
				if requests != 2 || executions != 1 {
					t.Fatalf("requests=%d tool executions=%d; want 2 and 1", requests, executions)
				}
				if len(history) != batchSize {
					t.Fatalf("history=%d; want %d", len(history), batchSize)
				}
				if scenario == "explanation" {
					if err != nil || !strings.Contains(text, "request was rejected") {
						t.Fatalf("text=%q error=%v", text, err)
					}
				} else if err == nil {
					t.Fatal("expected the final response failure to terminate the attempt")
				}
			})
		}
	}
}

func TestProviderSuccessfulToolsContinueNormally(t *testing.T) {
	for _, provider := range []string{"chat", "responses", "anthropic", "gemini", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			requests, executions := 0, 0
			tools := failureTestTool(t, func(context.Context, map[string]interface{}) (*ToolResult, error) {
				executions++
				return &ToolResult{Result: "done"}, nil
			})
			client := &http.Client{Transport: failureTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests > 3 {
					t.Fatal("unexpected extra request")
				}
				var body map[string]interface{}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if provider != "ollama" {
					if _, exists := body["tools"]; !exists {
						t.Fatal("successful continuation lost its tools")
					}
				}
				calls := 1
				if requests == 3 {
					calls = 0
				}
				return failureTestResponse(t, failureTestProviderReply(provider, calls, "All done."), http.StatusOK), nil
			})}
			text, history, err := failureTestRunProvider(provider, client, tools, 4)
			if err != nil || text != "All done." || requests != 3 || executions != 2 || len(history) != 2 {
				t.Fatalf("text=%q error=%v requests=%d executions=%d history=%d", text, err, requests, executions, len(history))
			}
		})
	}
}

func TestToolFailureStopsRemainingBatch(t *testing.T) {
	for _, scenario := range []string{"go_error", "result_error", "no_result", "unknown_tool", "factory_error"} {
		t.Run(scenario, func(t *testing.T) {
			executions := 0
			tools := failureTestTool(t, func(context.Context, map[string]interface{}) (*ToolResult, error) {
				executions++
				switch scenario {
				case "go_error":
					return nil, errors.New("rejected")
				case "result_error":
					return &ToolResult{Error: "rejected"}, nil
				case "no_result":
					return nil, nil
				default:
					return &ToolResult{Result: "done"}, nil
				}
			})
			firstName := "test_action"
			if scenario == "unknown_tool" {
				firstName = "missing"
			}
			if scenario == "factory_error" {
				tools[0].Config["type"] = "missing_factory"
			}
			calls := []*ToolCall{{ID: "first", Name: firstName}, {ID: "later", Name: "test_action"}}
			results, failed, err := executeToolCallsUntilFailure(context.Background(), calls, tools, &mockResolver{})
			expectedExecutions := 1
			if scenario == "unknown_tool" || scenario == "factory_error" {
				expectedExecutions = 0
			}
			if err != nil || !failed || len(results) != 2 || results[0].Error == "" || !strings.Contains(results[1].Error, "Not executed") || executions != expectedExecutions {
				t.Fatalf("results=%+v failed=%v err=%v executions=%d", results, failed, err, executions)
			}
		})
	}
}
