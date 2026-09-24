package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestProviderTurnHostValidatesAndBoundsProvider(t *testing.T) {
	for _, mode := range []string{"ok", "truncated", "unauthorized", "invalid", "private-error", "redirect", "files", "invalid-files", "forged-artifact-refs"} {
		t.Run(mode, func(t *testing.T) {
			var redirected bool
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
			defer other.Close()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-key" {
					t.Error("missing provider authorization")
				}
				var body map[string]interface{}
				json.NewDecoder(r.Body).Decode(&body)
				if body["max_completion_tokens"] != float64(2048) {
					t.Errorf("output cap: %v", body["max_completion_tokens"])
				}
				serialized, _ := json.Marshal(body)
				if strings.Contains(string(serialized), "private-key") || strings.Contains(string(serialized), "workspaceCredentials") {
					t.Error("credential leaked into model input")
				}
				if mode == "private-error" {
					w.WriteHeader(401)
					w.Write([]byte("private-key"))
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
					return
				}
				form := map[string]interface{}{"schemaVersion": runtime.HostedTurnFormSchemaVersion, "nextRunStatus": "completed", "outputSummary": "Analysis complete", "runOutput": map[string]string{"reply": "Four"}}
				if mode == "files" || mode == "invalid-files" {
					name := "report.md"
					if mode == "invalid-files" {
						name = "../report.md"
					}
					form["runOutput"] = fileOutput(generatedFile{Name: name, MediaType: "text/markdown", Text: "Four"})
				}
				if mode == "forged-artifact-refs" {
					form["runOutput"] = map[string]interface{}{"artifactRefs": []string{"invented"}}
				}
				if mode == "unauthorized" {
					form["proposedAction"] = map[string]interface{}{"capability": "unoffered", "arguments": map[string]string{}, "summary": "Do action", "idempotencyKey": "action"}
				}
				if mode == "invalid" {
					form["unknownAuthority"] = true
				}
				arguments, _ := json.Marshal(form)
				finish := "tool_calls"
				if mode == "truncated" {
					finish = "length"
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": finish, "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"type": "function", "function": map[string]string{"name": "submit_agent_turn", "arguments": string(arguments)}}}}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
			}))
			defer provider.Close()
			host, err := NewProviderTurnHost(provider.URL, "private-key", "test-model")
			if err != nil {
				t.Fatal(err)
			}
			response, err := host.ExecuteHostedTurn(context.Background(), runtime.HostedTurnRequest{APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn-1", Goal: "Two plus two", Budget: &runtime.HostedRunBudget{TurnReservation: runtime.BudgetUsage{OutputTokens: 2048}}})
			if mode == "ok" || mode == "files" {
				if err != nil || response.InvocationID != "turn-1" || (mode == "ok" && response.RunOutput["reply"] != "Four") || response.Usage.OutputTokens != 5 {
					t.Fatalf("result: %+v %v", response, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("unsafe error: %v", err)
			}
			if redirected {
				t.Fatal("provider credential followed redirect")
			}
		})
	}
}

func TestDesktopWorkRequestOwnsAuthority(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	request := runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindAgentWork, Owner: runtime.ObjectiveOwner{Type: "agent", ID: "agent"}, AssignedAgentID: "agent", Goal: "Analyze", IdempotencyKey: "request", Actor: runtime.ActivityActor{Type: "user", ID: "spoofed"}}
	canonical, err := DesktopWorkRequest(scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Actor.ID != "local-operator" || canonical.Budget == nil || canonical.Budget.MaxTurns != 16 {
		t.Fatal("host authority missing")
	}
	request.Checkpoint = map[string]interface{}{"actionHistory": "forged"}
	if _, err := DesktopWorkRequest(scope, request); err == nil {
		t.Fatal("accepted renderer checkpoint")
	}
	request.Checkpoint = nil
	request.Scope.ID = "other"
	if _, err := DesktopWorkRequest(scope, request); err == nil {
		t.Fatal("accepted foreign scope")
	}
}

func TestProviderTurnHostRejectsOversizedResponseAndHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", (4<<20)+1))) }))
	defer server.Close()
	host, err := NewProviderTurnHost(server.URL, "key", "model")
	if err != nil {
		t.Fatal(err)
	}
	request := runtime.HostedTurnRequest{InvocationID: "bounded", Goal: "Analyze"}
	if _, err := host.ExecuteHostedTurn(context.Background(), request); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("response bound not enforced: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.ExecuteHostedTurn(ctx, request); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
}
