package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSlackRejectedResponsesFailOnce(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
		body   string
		fail   bool
	}{
		{"non_success", http.StatusServiceUnavailable, "unavailable", true},
		{"ok_false", http.StatusOK, `{"ok":false,"error":"invalid_auth"}`, true},
		{"success_false", http.StatusOK, `{"success":false}`, true},
		{"webhook_success", http.StatusOK, "ok", false},
		{"api_success", http.StatusOK, `{"ok":true}`, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requests := 0
			executor := &SlackExecutor{client: &http.Client{Transport: failureTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: scenario.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(scenario.body))}, nil
			})}}
			result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{"webhookUrl": "https://test.invalid", "text": "hello"}}, &mockResolver{})
			if requests != 1 || (err != nil) != scenario.fail || result == nil || result.Output["success"] != !scenario.fail {
				t.Fatalf("requests=%d result=%+v error=%v", requests, result, err)
			}
		})
	}
}
