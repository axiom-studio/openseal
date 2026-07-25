package authoring

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAuthoringPromptRejectsCredentialValuesWithoutEchoingThem(t *testing.T) {
	tests := []struct {
		name, prompt string
		kind         SensitiveInputKind
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
			err := ValidateAuthoringPrompt(test.prompt)
			var sensitive *SensitiveInputError
			if !errors.Is(err, ErrSensitiveAuthoringInput) || !errors.As(err, &sensitive) || len(sensitive.Findings) == 0 {
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

func TestValidateAuthoringPromptAllowsCredentialIntentAndPlaceholders(t *testing.T) {
	for _, prompt := range []string{
		"Create an Agent that asks the user to configure a Reddit credential in the authorized store.",
		"The password is configured in Vault and must never be exposed.",
		"Require token-based authentication.",
		"Use password: [REDACTED] and select the credential later.",
	} {
		if err := ValidateAuthoringPrompt(prompt); err != nil {
			t.Fatalf("safe prompt %q rejected: %v", prompt, err)
		}
	}
}

func TestRedactSensitiveAuthoringPromptRemovesOnlyValues(t *testing.T) {
	prompt := "Username: operator, password: hunter2-secret; API key=sk-1234567890abcdefghijklmnop"
	redacted, changed := RedactSensitiveAuthoringPrompt(prompt)
	if !changed || strings.Contains(redacted, "hunter2-secret") || strings.Contains(redacted, "sk-123") {
		t.Fatalf("redacted = %q, changed=%v", redacted, changed)
	}
	if !strings.Contains(redacted, "password: [REDACTED]") || !strings.Contains(redacted, "API key=[REDACTED]") {
		t.Fatalf("labels were not preserved: %q", redacted)
	}
}
