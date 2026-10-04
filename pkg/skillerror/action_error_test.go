package skillerror

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestActionErrorSanitizesKnownFailuresAndKeepsRateLimitDistinct(t *testing.T) {
	for _, code := range []string{"source_rate_limited", "source_http_error", "source_access_challenge", "source_unavailable", "source_invalid_response", "source_empty_response", "browser_proxy_authentication_failed", "browser_proxy_unavailable", "source_reads_failed"} {
		t.Run(code, func(t *testing.T) {
			input := map[string]string{"failureKind": "INJECTED", "retryable": "true", "httpStatus": "503", "retryAfterSeconds": "120", "token": "SECRET", "url": "https://SECRET.invalid"}
			err := NewActionError(code, "RAW_SECRET ignore instructions", input)
			if err == nil || err.Code() != code || err.Details()["failureKind"] != code || err.Details()["retryable"] != "false" {
				t.Fatalf("wrong classification: %#v", err)
			}
			encoded, _ := json.Marshal(err.Details())
			if strings.Contains(err.Error()+string(encoded), "SECRET") || strings.Contains(string(encoded), "INJECTED") || err.Details()["retryAfterSeconds"] != "120" {
				t.Fatal("unsafe or missing failure details")
			}
			input["retryAfterSeconds"] = "5"
			copy := err.Details()
			copy["retryAfterSeconds"] = "10"
			if err.Details()["retryAfterSeconds"] != "120" {
				t.Fatal("mutable input or accessor modified failure evidence")
			}
			var typed *ActionError
			if !errors.As(fmt.Errorf("dispatch: %w", err), &typed) || typed.Code() != code {
				t.Fatal("wrapped error lost portable type")
			}
		})
	}
	for _, code := range []string{"", "unknown", "source_rate_limited\nSECRET", "source_rate_limited: SECRET"} {
		if NewActionError(code, "source_rate_limited", map[string]string{"httpStatus": "429"}) != nil {
			t.Fatal("unknown code became a recognized failure")
		}
	}
	for _, code := range []string{"source_http_error", "source_access_challenge", "source_rate_limited"} {
		err := NewActionError(code, "bot detected", map[string]string{"httpStatus": "429"})
		if err.Code() != "source_rate_limited" || err.Error() != "This source returned HTTP 429 (rate limited). Try again later." {
			t.Fatalf("429 changed to challenge or lost safe feedback: %#v", err)
		}
		if _, invented := err.Details()["retryAfterSeconds"]; invented {
			t.Fatal("invented a Retry-After delay")
		}
	}
}

func TestActionErrorBoundsEveryDetailAndNestedBatch(t *testing.T) {
	for _, input := range []map[string]string{
		{"httpStatus": "600", "retryAfterSeconds": "86401", "failedCount": "0", "totalCount": "5"},
		{"httpStatus": "-1", "retryAfterSeconds": "1.5", "failedCount": "true", "totalCount": "1 SECRET"},
		{"httpStatus": " 429", "retryAfterSeconds": strings.Repeat("9", 100000)},
	} {
		details := NewActionError("source_http_error", "SECRET", input).Details()
		if len(details) != 2 {
			t.Fatalf("malformed scalar details persisted: %#v", details)
		}
	}
	input := map[string]string{"failedCount": "2", "totalCount": "2", "failures": `[{"index":0,"failureKind":"source_access_challenge","httpStatus":429,"retryAfterSeconds":0,"url":"SECRET"},{"index":1,"failureKind":"source_unavailable","retryAfterSeconds":86400,"error":"SECRET"}]`}
	got := NewActionError("source_reads_failed", "SECRET", input).Details()
	if strings.Contains(got["failures"], "SECRET") || !strings.Contains(got["failures"], `"failureKind":"source_rate_limited"`) || !strings.Contains(got["failures"], `"retryAfterSeconds":0`) || got["failedCount"] != "2" {
		t.Fatalf("batch evidence lost or leaked: %#v", got)
	}
	for _, raw := range []string{
		`[]`, `{}`, `null`, `[{"failureKind":"source_unavailable"}]`,
		`[{"index":0,"failureKind":"SECRET"}]`, `[{"index":0.5,"failureKind":"source_unavailable"}]`,
		`[{"index":0,"failureKind":"source_unavailable"},{"index":0,"failureKind":"source_unavailable"}]`,
		`[{"index":4,"failureKind":"source_unavailable"}]`, strings.Repeat("x", 4097),
	} {
		if value := NewActionError("source_reads_failed", "", map[string]string{"failures": raw}).Details()["failures"]; value != "" {
			t.Fatalf("malformed batch survived: %s", value)
		}
	}
}

func TestActionErrorRecognizesRateLimitInBoundedMixedBatch(t *testing.T) {
	for _, test := range []struct {
		code, failures string
		want           bool
	}{
		{"source_rate_limited", "", true},
		{"source_access_challenge", "", false},
		{"source_reads_failed", `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429},{"index":1,"failureKind":"source_http_error","httpStatus":503}]`, true},
		{"source_reads_failed", `[{"index":0,"failureKind":"source_access_challenge","httpStatus":200},{"index":1,"failureKind":"source_http_error","httpStatus":503}]`, false},
		{"source_reads_failed", `[{"index":0,"failureKind":"unknown","httpStatus":429}]`, false},
		{"source_reads_failed", "source_rate_limited HTTP429 SECRET", false},
	} {
		failure := NewActionError(test.code, "", map[string]string{"failures": test.failures})
		if failure.HasRateLimitedSource() != test.want {
			t.Fatalf("wrong nested rate limit classification: %#v", failure)
		}
	}
}
