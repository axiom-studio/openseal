package authoring

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHardenLegacySensitiveAgentDefinitionReplacesCredentialParaphrase(t *testing.T) {
	definition := &agent.AgentDefinition{
		ID:           "legacy-agent",
		Version:      "1",
		DisplayName:  "Legacy Agent",
		Purpose:      "Perform authorized work",
		SystemPrompt: "Sign in with the supplied username and password credential-value-that-must-go.",
		Authority: agent.AuthorityPolicy{
			MaximumRisk:       capability.RiskLevelRead,
			MaxConcurrentRuns: 1,
		},
		Digest: "legacy-digest",
	}

	changed, err := HardenLegacySensitiveAgentDefinition(definition)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Contains(definition.SystemPrompt, "credential-value-that-must-go") ||
		definition.SystemPrompt != hardenedLegacyAgentPrompt || definition.Digest == "legacy-digest" {
		t.Fatalf("definition was not safely hardened: %#v", definition)
	}
}
