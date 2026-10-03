package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestBatchEmbeddingProviderFailureIsNotRetried(t *testing.T) {
	for _, provider := range []string{"openai", "cohere", "gemini"} {
		for _, failure := range []string{"timeout", "429", "503"} {
			t.Run(provider+"/"+failure, func(t *testing.T) {
				requests := 0
				providerErr := errors.New("temporary timeout")
				executor := &PGVectorExecutor{client: &http.Client{Transport: failureTestTransport(func(req *http.Request) (*http.Response, error) {
					requests++
					if failure == "timeout" {
						return nil, providerErr
					}
					status := http.StatusTooManyRequests
					if failure == "503" {
						status = http.StatusServiceUnavailable
					}
					return failureTestResponse(t, map[string]interface{}{"error": failure}, status), nil
				})}}
				_, err := executor.generateBatchEmbeddings(context.Background(), []string{"one", "two"}, embeddingConfig{Provider: provider, APIKey: "test-key", BaseURL: "https://test.invalid"})
				if requests != 1 || err == nil {
					t.Fatalf("requests=%d error=%v; want one failed attempt", requests, err)
				}
				if failure == "timeout" && !errors.Is(err, providerErr) {
					t.Fatalf("original provider error lost: %v", err)
				}
			})
		}
	}
}
