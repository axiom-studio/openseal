package authoring

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIResponsesGeneratorClassifiesTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantError  interface{}
	}{
		{name: "refusal", statusCode: http.StatusOK, response: `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`, wantError: &ProviderRefusalError{}},
		{name: "incomplete", statusCode: http.StatusOK, response: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, wantError: &ProviderIncompleteError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/responses" {
					t.Fatalf("request path = %q", request.URL.Path)
				}
				response.WriteHeader(test.statusCode)
				_, _ = response.Write([]byte(test.response))
			}))
			defer server.Close()
			generator, err := NewOpenAIResponsesGeneratorWithOptions(server.URL+"/v1", "secret", "gpt-5.4", server.Client(), OpenAICompatibleGeneratorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = generator.completeContract(t.Context(), "invocation", []map[string]string{{"role": "user", "content": "work"}}, authoringResultContract())
			switch test.wantError.(type) {
			case *ProviderRefusalError:
				var target *ProviderRefusalError
				if !errors.As(err, &target) {
					t.Fatalf("error = %v", err)
				}
			case *ProviderIncompleteError:
				var target *ProviderIncompleteError
				if !errors.As(err, &target) || target.FinishReason != "max_output_tokens" {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
}

func TestOpenAIResponsesGeneratorSanitizesProviderError(t *testing.T) {
	const secretDetail = "sensitive-provider-detail"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"error":{"message":"` + secretDetail + `"}}`))
	}))
	defer server.Close()
	generator, err := NewOpenAIResponsesGeneratorWithOptions(server.URL, "secret", "gpt-5.4", server.Client(), OpenAICompatibleGeneratorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.completeContract(t.Context(), "", []map[string]string{{"role": "user", "content": "work"}}, authoringResultContract())
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), secretDetail) {
		t.Fatalf("provider error = %v", err)
	}
}
