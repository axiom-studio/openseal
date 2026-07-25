package capability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeOAuth2RequirementProducesStableLeastPrivilegeContract(t *testing.T) {
	normalized, err := NormalizeOAuth2Requirement(&OAuth2Requirement{
		Provider: "slack", Subject: OAuth2SubjectInstallation,
		Scopes: []string{"channels:history", "chat:write", "chat:write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(normalized.Scopes, ",") != "channels:history,chat:write" {
		t.Fatalf("normalized scopes = %#v", normalized.Scopes)
	}
	if _, err = NormalizeOAuth2Requirement(&OAuth2Requirement{
		Provider: "Slack", Subject: OAuth2SubjectInstallation, Scopes: []string{"chat:write"},
	}); err == nil {
		t.Fatal("mixed-case provider should be rejected")
	}
	if _, err = NormalizeOAuth2Requirement(&OAuth2Requirement{
		Provider: "slack", Subject: "workspace", Scopes: []string{"chat:write"},
	}); err == nil {
		t.Fatal("unknown subject should be rejected")
	}
}

func TestOAuth2GrantSatisfiesExactIdentityAndScopeSuperset(t *testing.T) {
	requirement := &OAuth2Requirement{
		Provider: "slack", Subject: OAuth2SubjectInstallation,
		Scopes: []string{"channels:history", "chat:write"},
	}
	grant := &OAuth2GrantSummary{
		Provider: "slack", Subject: OAuth2SubjectInstallation,
		Scopes: []string{"app_mentions:read", "channels:history", "chat:write"},
	}
	if !OAuth2GrantSatisfies(requirement, grant) {
		t.Fatal("scope superset should satisfy the requirement")
	}
	for name, mutate := range map[string]func(*OAuth2GrantSummary){
		"provider": func(value *OAuth2GrantSummary) { value.Provider = "microsoft" },
		"subject":  func(value *OAuth2GrantSummary) { value.Subject = OAuth2SubjectUser },
		"resource": func(value *OAuth2GrantSummary) { value.Resource = "https://slack.com/api" },
		"scope":    func(value *OAuth2GrantSummary) { value.Scopes = []string{"chat:write"} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *grant
			candidate.Scopes = append([]string(nil), grant.Scopes...)
			mutate(&candidate)
			if OAuth2GrantSatisfies(requirement, &candidate) {
				t.Fatalf("%s mismatch satisfied requirement", name)
			}
		})
	}
}

func TestOAuth2ContractsCannotCarryTokenMaterial(t *testing.T) {
	encoded, err := json.Marshal(struct {
		Requirement *OAuth2Requirement  `json:"requirement"`
		Grant       *OAuth2GrantSummary `json:"grant"`
	}{
		Requirement: &OAuth2Requirement{Provider: "slack", Subject: OAuth2SubjectInstallation, Scopes: []string{"chat:write"}},
		Grant:       &OAuth2GrantSummary{Provider: "slack", Subject: OAuth2SubjectInstallation, Scopes: []string{"chat:write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "clientSecret", "authorizationUrl", "subjectId"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("OAuth 2 contract exposed %s: %s", forbidden, encoded)
		}
	}
}

func TestOAuth2RequirementJSONRoundTripPreservesCanonicalContract(t *testing.T) {
	original := CredentialRequirement{
		Name: "SLACK_CONNECTION", Kind: "slack-oauth",
		OAuth2: &OAuth2Requirement{
			Provider: "slack", Subject: OAuth2SubjectInstallation,
			Scopes:   []string{"channels:history", "chat:write"},
			Resource: "https://slack.com/api",
		},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored CredentialRequirement
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Name != original.Name || restored.Kind != original.Kind || restored.OAuth2 == nil ||
		restored.OAuth2.Provider != "slack" || restored.OAuth2.Subject != OAuth2SubjectInstallation ||
		strings.Join(restored.OAuth2.Scopes, ",") != "channels:history,chat:write" ||
		restored.OAuth2.Resource != "https://slack.com/api" {
		t.Fatalf("restored OAuth 2 requirement = %#v", restored)
	}
}
