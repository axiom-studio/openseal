package httpaction

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExecutorPerformsAuthorizedReadAndScrubsCredential(t *testing.T) {
	const secret = "live-secret-value"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/search" || request.URL.Query().Get("keyword") != "agents" || request.URL.Query().Get("token") != secret {
			t.Fatalf("request = %s %s", request.Method, request.URL.String())
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]interface{}{"items": []interface{}{"result", secret}})
	}))
	defer server.Close()
	authorized := false
	executor, err := NewExecutor(server.Client().Transport, RequestPolicyFunc(func(_ context.Context, invocation Invocation) error {
		authorized = invocation.BaseURL == server.URL && invocation.Method == "GET" && invocation.Path == "/v1/search"
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return time.Unix(1, 2).UTC() }
	output, err := executor.Execute(context.Background(), Invocation{
		BaseURL: server.URL, Method: "GET", Path: "/v1/search",
		Parameters:        map[string]interface{}{"keyword": "agents"},
		ParameterContract: []Parameter{{Name: "keyword", Location: "query", Required: true}},
		CredentialName:    "API_TOKEN", CredentialParameter: "token",
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(output)
	if !authorized || output["statusCode"] != 200 || strings.Contains(string(encoded), secret) || !strings.Contains(string(encoded), "[REDACTED]") ||
		strings.Contains(string(encoded), "token=") {
		t.Fatalf("governed output = %s, authorized=%v", encoded, authorized)
	}
}

func TestExecutorRequiresPolicyAndRejectsRedirect(t *testing.T) {
	if _, err := NewExecutor(nil, nil); err == nil {
		t.Fatal("executor accepted a missing egress policy")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://elsewhere.example/", http.StatusFound)
	}))
	defer server.Close()
	executor, err := NewExecutor(server.Client().Transport, RequestPolicyFunc(func(context.Context, Invocation) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), Invocation{
		BaseURL: server.URL, Method: "GET", Path: "/v1/search", Parameters: map[string]interface{}{"keyword": "agents"},
		ParameterContract: []Parameter{{Name: "keyword", Location: "query", Required: true}},
		CredentialName:    "API_TOKEN", CredentialParameter: "token",
	}, "secret")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect result = %v", err)
	}

	denied, _ := NewExecutor(server.Client().Transport, RequestPolicyFunc(func(context.Context, Invocation) error { return errors.New("egress denied") }))
	if _, err := denied.Execute(context.Background(), Invocation{
		BaseURL: server.URL, Method: "GET", Path: "/v1/search", Parameters: map[string]interface{}{"keyword": "agents"},
		ParameterContract: []Parameter{{Name: "keyword", Location: "query", Required: true}},
		CredentialName:    "API_TOKEN", CredentialParameter: "token",
	}, "secret"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("policy denial = %v", err)
	}
}
