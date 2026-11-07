package validation

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNoSensitiveInputRejectsCredentialValuesWithoutEchoingThem(t *testing.T) {
	tests := []struct {
		name, value string
		kind        SensitiveInputKind
	}{
		{"password", "Create an Agent. Username: operator, Password: hunter2-secret", SensitiveInputPassword},
		{"json password", `Create an Agent with {"password":"not-for-the-model"}`, SensitiveInputPassword},
		{"api key", "Use the endpoint, api key: sk-1234567890abcdefghijklmnop", SensitiveInputAPIKey},
		{"bearer", "Call it with Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload", SensitiveInputToken},
		{"known token", "Use github_pat_1234567890abcdefghijklmnop", SensitiveInputToken},
		{"uri password", "Read https://agent:do-not-store-me@example.com/feed", SensitiveInputPassword},
		{"private key", "Use -----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----", SensitiveInputPrivateKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateNoSensitiveInput(test.value)
			var sensitive *SensitiveInputError
			if !errors.Is(err, ErrSensitiveInput) || !errors.As(err, &sensitive) || len(sensitive.Findings) == 0 {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "1234567890") || strings.Contains(err.Error(), "do-not-store") {
				t.Fatalf("error echoed credential: %q", err)
			}
			found := false
			for _, finding := range sensitive.Findings {
				found = found || finding.Kind == test.kind
			}
			if !found {
				t.Fatalf("findings = %#v; want %s", sensitive.Findings, test.kind)
			}
		})
	}
}

func TestValidateNoSensitiveInputReportsEachKindOnceInOrder(t *testing.T) {
	err := ValidateNoSensitiveInput("password: hunter2-secret, token=abcdefgh, password=another-one")
	var sensitive *SensitiveInputError
	if !errors.As(err, &sensitive) {
		t.Fatalf("error = %#v", err)
	}
	want := []SensitiveInputFinding{{Kind: SensitiveInputPassword}, {Kind: SensitiveInputToken}}
	if len(sensitive.Findings) != len(want) || sensitive.Findings[0] != want[0] || sensitive.Findings[1] != want[1] {
		t.Fatalf("findings = %#v; want %#v", sensitive.Findings, want)
	}
}

func TestValidateNoSensitiveInputAllowsCredentialIntentAndPlaceholders(t *testing.T) {
	for _, value := range []string{
		"",
		"Create an Agent that asks the user to configure a Reddit credential in the authorized store.",
		"The password is configured in the authorized credential store and must never be exposed.",
		"Require token-based authentication.",
		"Use password: [REDACTED] and select the credential later.",
	} {
		if err := ValidateNoSensitiveInput(value); err != nil {
			t.Fatalf("safe text %q rejected: %v", value, err)
		}
	}
}

func TestRedactSensitiveInputRemovesOnlyValues(t *testing.T) {
	value := "Username: operator, password: hunter2-secret; API key=sk-1234567890abcdefghijklmnop"
	redacted, changed := RedactSensitiveInput(value)
	if !changed || strings.Contains(redacted, "hunter2-secret") || strings.Contains(redacted, "sk-123") {
		t.Fatalf("redacted = %q, changed=%v", redacted, changed)
	}
	if !strings.Contains(redacted, "password: [REDACTED]") || !strings.Contains(redacted, "API key=[REDACTED]") {
		t.Fatalf("labels were not preserved: %q", redacted)
	}
	if same, changed := RedactSensitiveInput("nothing secret here"); changed || same != "nothing secret here" {
		t.Fatalf("safe text changed: %q %v", same, changed)
	}
}
