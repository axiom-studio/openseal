package skillerror

import (
	"encoding/json"
	"strconv"
)

// ActionError retains a bounded source failure across Skill transports. Its
// fields are private so raw provider messages cannot become trusted feedback.
// It describes evidence only; it never grants a retry or alternate browser.
type ActionError struct {
	code    string
	message string
	details map[string]string
}

// NewActionError recognizes the portable browser failure vocabulary. Unknown
// errors return nil so callers can retain their existing transport behavior.
// The remote message is deliberately ignored for recognized failures.
func NewActionError(code, _ string, details map[string]string) *ActionError {
	message := actionErrorMessage(code)
	if message == "" {
		return nil
	}
	clean := map[string]string{"failureKind": code, "retryable": "false"}
	for key, maximum := range map[string]int{"httpStatus": 599, "retryAfterSeconds": 86400, "failedCount": 4, "totalCount": 4} {
		minimum := 0
		if key == "failedCount" || key == "totalCount" {
			minimum = 1
		}
		if value, ok := boundedActionErrorInteger(details[key], minimum, maximum); ok {
			clean[key] = strconv.Itoa(value)
		}
	}
	// HTTP 429 is source throttling, never evidence of an access challenge.
	if clean["httpStatus"] == "429" {
		code, message = "source_rate_limited", actionErrorMessage("source_rate_limited")
		clean["failureKind"] = code
	}
	if code == "source_rate_limited" {
		clean["httpStatus"] = "429"
	}
	if failures := boundedActionErrorFailures(details["failures"]); failures != "" {
		clean["failures"] = failures
	}
	return &ActionError{code: code, message: message, details: clean}
}

func (e *ActionError) Error() string { return e.message }
func (e *ActionError) Code() string  { return e.code }

// Details returns a defensive copy of sanitized, bounded metadata.
func (e *ActionError) Details() map[string]string {
	result := make(map[string]string, len(e.details))
	for key, value := range e.details {
		result[key] = value
	}
	return result
}

// HasRateLimitedSource identifies direct throttling or a throttled source in
// an all-failed batch. The constructor has already bounded and sanitized it.
func (e *ActionError) HasRateLimitedSource() bool {
	if e.code == "source_rate_limited" {
		return true
	}
	if e.code != "source_reads_failed" {
		return false
	}
	var items []actionErrorItem
	if json.Unmarshal([]byte(e.details["failures"]), &items) != nil {
		return false
	}
	for _, item := range items {
		if item.FailureKind == "source_rate_limited" && item.HTTPStatus != nil && *item.HTTPStatus == 429 {
			return true
		}
	}
	return false
}

func actionErrorMessage(code string) string {
	switch code {
	case "source_rate_limited":
		return "This source returned HTTP 429 (rate limited). Try again later."
	case "source_http_error":
		return "This source returned an unsuccessful HTTP response."
	case "source_access_challenge":
		return "This source requires an access check before it can be read."
	case "source_unavailable":
		return "This source could not be reached."
	case "source_invalid_response":
		return "This source returned a response that could not be read."
	case "source_empty_response":
		return "This source returned no readable content."
	case "browser_proxy_authentication_failed":
		return "The configured browser proxy rejected authentication."
	case "browser_proxy_unavailable":
		return "The configured browser proxy could not be reached."
	case "source_reads_failed":
		return "The requested sources could not be read."
	default:
		return ""
	}
}

func boundedActionErrorInteger(raw string, minimum, maximum int) (int, bool) {
	if raw == "" || len(raw) > 6 {
		return 0, false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value >= minimum && value <= maximum
}

type actionErrorItem struct {
	Index             *int   `json:"index"`
	FailureKind       string `json:"failureKind"`
	HTTPStatus        *int   `json:"httpStatus,omitempty"`
	RetryAfterSeconds *int   `json:"retryAfterSeconds,omitempty"`
}

func boundedActionErrorFailures(raw string) string {
	if raw == "" || len(raw) > 4096 {
		return ""
	}
	var items []actionErrorItem
	if json.Unmarshal([]byte(raw), &items) != nil || len(items) == 0 || len(items) > 4 {
		return ""
	}
	seen := map[int]bool{}
	for index := range items {
		item := &items[index]
		if item.Index == nil || *item.Index < 0 || *item.Index > 3 || seen[*item.Index] || actionErrorMessage(item.FailureKind) == "" {
			return ""
		}
		seen[*item.Index] = true
		if item.HTTPStatus != nil && (*item.HTTPStatus < 0 || *item.HTTPStatus > 599) {
			item.HTTPStatus = nil
		}
		if item.RetryAfterSeconds != nil && (*item.RetryAfterSeconds < 0 || *item.RetryAfterSeconds > 86400) {
			item.RetryAfterSeconds = nil
		}
		if item.HTTPStatus != nil && *item.HTTPStatus == 429 {
			item.FailureKind = "source_rate_limited"
		}
		if item.FailureKind == "source_rate_limited" {
			status := 429
			item.HTTPStatus = &status
		}
	}
	encoded, _ := json.Marshal(items)
	return string(encoded)
}
