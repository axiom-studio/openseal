package skill

import "testing"

func TestDiscoveryNormalizesOAuth2RequirementsWithoutConnectionIdentity(t *testing.T) {
	page, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, &DiscoveryPage{Items: []DiscoveryCandidate{{
		ID: "slack", Version: "1.0.0", Name: "Slack", Readiness: DiscoveryReadinessBindable,
		Credentials: []DiscoveryCredential{{
			Name: "SLACK_CONNECTION", Kind: "slack-oauth", Configured: true,
			OAuth2: &OAuth2Requirement{
				Provider: "slack", Subject: OAuth2SubjectInstallation,
				Scopes: []string{"chat:write", "channels:history"},
			},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	scopes := page.Items[0].Credentials[0].OAuth2.Scopes
	if len(scopes) != 2 || scopes[0] != "channels:history" || scopes[1] != "chat:write" {
		t.Fatalf("normalized discovery scopes = %#v", scopes)
	}
}
