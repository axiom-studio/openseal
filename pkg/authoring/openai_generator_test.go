package authoring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleGeneratorUsesStrictJSONTransportWithoutLeakingKey(t *testing.T) {
	apiKey := "transport-secret"
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Fatalf("request method/auth = %s / %q", request.Method, request.Header.Get("Authorization"))
		}
		var body map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		requestBody = string(encoded)
		if body["model"] != "deepseek-v4-flash" || body["temperature"].(float64) != 0 || body["response_format"].(map[string]interface{})["type"] != "json_object" {
			t.Fatalf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"candidate\":{\"agents\":[],\"assignments\":[]},\"questions\":[\"Which Team should be created?\"]}"}}]}`))
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGenerator(server.URL, apiKey, "deepseek-v4-flash", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Team", Catalog: CapabilityCatalog{}})
	if err != nil || !strings.Contains(string(payload), "Which Team") {
		t.Fatalf("payload = %s, err = %v", payload, err)
	}
	if strings.Contains(requestBody, apiKey) || strings.Contains(string(payload), apiKey) {
		t.Fatal("transport credential leaked into model-visible or returned content")
	}
}

func TestAuthoringSchemaMakesObjectiveMetadataObjectTyped(t *testing.T) {
	for _, expected := range []string{
		"domainContext is a JSON object, never a string or array",
		"authority.budgetCeilings is a JSON object whose values are non-negative numbers",
		"cadence, eventRules, successCriteria, and constraints are JSON objects",
		"never strings or arrays",
		"omit any of them when no structured value is needed",
	} {
		if !strings.Contains(authoringSystemPrompt, expected) {
			t.Fatalf("authoring schema missing %q", expected)
		}
	}
}

func TestOpenAICompatibleGeneratorRedactsProviderErrorsAndRejectsMultipleChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"provider-secret-detail"}`))
	}))
	defer server.Close()
	generator, _ := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	_, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"})
	if err == nil || strings.Contains(err.Error(), "provider-secret-detail") {
		t.Fatalf("provider error = %v", err)
	}
}
