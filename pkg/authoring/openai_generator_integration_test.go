//go:build integration

package authoring

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestLiveGeneratorCompilesRepresentativeMarketingTeam(t *testing.T) {
	endpoint, apiKey, model := os.Getenv("OPENSEAL_LLM_BASE_URL"), os.Getenv("OPENAI_API_KEY"), os.Getenv("OPENSEAL_LLM_MODEL")
	if endpoint == "" || apiKey == "" || model == "" {
		t.Skip("OpenSeal live authoring provider is not configured")
	}
	generator, err := NewOpenAICompatibleGenerator(endpoint, apiKey, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(generator)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := compiler.Compile(ctx, GenerateRequest{
		Mode:   ModeCreate,
		Prompt: "Create a calm three-Agent marketing research Team. It monitors approved Reddit communities for competitor pain points, preserves cited evidence, drafts a weekly report, and prepares replies but requires human approval before posting. Ask about any missing community allowlist or identity policy.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, CredentialKinds: []string{"reddit-oauth"}, MaximumRisk: capability.RiskLevelExternal},
			"report-artifact": {ID: "report-artifact", MaximumRisk: capability.RiskLevelWrite},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidate.Team == nil || len(result.Candidate.Agents) != 3 || len(result.Candidate.Assignments) != 3 {
		t.Fatalf("candidate shape = %#v", result.Candidate)
	}
	if len(result.Validation) != 0 {
		t.Fatalf("candidate validation = %#v", result.Validation)
	}
	if len(result.Questions) == 0 {
		t.Fatal("ambiguous outreach authority should produce a question")
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte(apiKey)) {
		t.Fatal("provider API key leaked into authoring result")
	}
}
