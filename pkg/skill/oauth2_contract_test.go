package skill

import (
	"context"
	"testing"
)

func TestSkillCredentialDeclaresCanonicalOAuth2Authorization(t *testing.T) {
	definition := testSkillDefinition()
	definition.Actions["deploy"] = Action{
		Name: "deploy", Description: "Post a governed Slack reply",
		InputSchema: map[string]interface{}{"type": "object"},
		Risk:        RiskLevelExternal, SideEffect: SideEffectExternal,
		Idempotency: IdempotencyRequired, Retry: ActionRetryPolicy{MaxAttempts: 3},
		Credentials: []CredentialRequirement{{
			Name: "SLACK_CONNECTION", Kind: "slack-oauth",
			OAuth2: &OAuth2Requirement{
				Provider: "slack", Subject: OAuth2SubjectInstallation,
				Scopes: []string{"channels:history", "chat:write"},
			},
		}},
	}
	definition.Transport = TransportReference{Kind: "tool", Endpoint: "slack"}
	if err := NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}

	nonCanonical := cloneDefinition(definition)
	nonCanonical.ID = "slack-non-canonical"
	nonCanonical.Actions["deploy"].Credentials[0].OAuth2.Scopes = []string{"chat:write", "channels:history"}
	if err := NewCatalog().Register(context.Background(), nonCanonical); err == nil {
		t.Fatal("non-canonical scope ordering should be rejected")
	}
}
