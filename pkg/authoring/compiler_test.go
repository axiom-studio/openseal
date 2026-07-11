package authoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
)

type staticGenerator struct {
	payload []byte
	err     error
}

type repairingGenerator struct {
	generated []byte
	repaired  []byte
	repairs   int
}

func (g *repairingGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	return g.generated, nil
}

func (g *repairingGenerator) Repair(_ context.Context, _ GenerateRequest, _ []byte, _ error) ([]byte, error) {
	g.repairs++
	return g.repaired, nil
}

func (g staticGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	return g.payload, g.err
}

func TestCompilerVerifiesPromptGeneratedWorkforceAndCapabilityGaps(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelExternal)
	payload, _ := json.Marshal(GenerationResponse{
		Candidate: candidate, Assumptions: []string{"Public replies use the configured brand identity", "Public replies use the configured brand identity"},
		Questions: []string{"Which communities are approved for outreach?"},
	})
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a GTM team that researches Reddit and follows up with qualified leads.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", CredentialKinds: []string{"reddit-oauth"}, MaximumRisk: capability.RiskLevelExternal},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.Validation) != 0 || len(result.Assumptions) != 1 || len(result.Questions) != 1 ||
		len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "credential" || result.MissingRequirements[0].ID != "reddit-oauth" {
		t.Fatalf("compile result = %#v", result)
	}
	if len(result.Diff) != 1 || result.Diff[0].Path != "workforce" {
		t.Fatalf("create diff = %#v", result.Diff)
	}
}

func TestCompilerProducesDeterministicAmendmentDiffAndRiskWidening(t *testing.T) {
	existing := marketingCandidate("1", capability.RiskLevelRead)
	candidate := marketingCandidate("2", capability.RiskLevelExternal)
	candidate.Team.Purpose = "Research demand and publish approved responses"
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeAmend, Prompt: "Allow approved public responses and strengthen the research objective.", Existing: &existing,
		Catalog: CapabilityCatalog{
			Skills:               map[string]SkillCapability{"reddit-research": {ID: "reddit-research", CredentialKinds: []string{"reddit-oauth"}}},
			AvailableCredentials: map[string]bool{"reddit-oauth": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.RiskChanges) != 2 || !result.RiskChanges[0].Widening || !result.RiskChanges[1].Widening {
		t.Fatalf("amendment result = %#v", result)
	}
	if len(result.Diff) == 0 || result.Diff[0].BeforeDigest == result.Diff[0].AfterDigest {
		t.Fatalf("amendment diff = %#v", result.Diff)
	}
}

func TestCompilerRejectsInvalidCompositionAndNonStrictGeneratorOutput(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Assignments[0].RoleID = "invented-role"
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a team", Catalog: CapabilityCatalog{
		Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}},
	}})
	if err != nil || result.Valid || len(result.Validation) == 0 || result.Validation[0].Code != "unknown_role" {
		t.Fatalf("invalid composition = %#v, err = %v", result, err)
	}
	strict, _ := NewCompiler(staticGenerator{payload: []byte(`{"candidate":{"agents":[]},"hiddenReasoning":"no"}`)})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a team"}); err == nil {
		t.Fatal("unknown generator output should fail closed")
	}
}

func TestCompilerPerformsOnlyOneStrictSchemaRepair(t *testing.T) {
	valid, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	generator := &repairingGenerator{generated: []byte(`{"candidate":{"agents":[]},"unknown":true}`), repaired: valid}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}},
	})
	if err != nil || !result.Valid || generator.repairs != 1 {
		t.Fatalf("repaired result = %#v, repairs = %d, err = %v", result, generator.repairs, err)
	}
	generator = &repairingGenerator{generated: []byte(`{"unknown":true}`), repaired: []byte(`{"stillUnknown":true}`)}
	compiler, _ = NewCompiler(generator)
	if _, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil || generator.repairs != 1 {
		t.Fatalf("second invalid output should fail after one repair, repairs = %d, err = %v", generator.repairs, err)
	}
}

func marketingCandidate(version string, risk capability.RiskLevel) WorkforceCandidate {
	agentDefinition := &agent.AgentDefinition{
		ID: "community-researcher", Version: version, DisplayName: "Community researcher", Purpose: "Find evidence-backed demand",
		SystemPrompt:      "Research permitted communities, preserve evidence, and never impersonate users.",
		SkillRequirements: []agent.SkillRequirement{{SkillID: "reddit-research", RequiredActions: []string{"search", "read"}}},
		Authority:         agent.AuthorityPolicy{MaximumRisk: risk, AllowedSkillIDs: []string{"reddit-research"}, MaxConcurrentRuns: 2, RequireApprovalAt: capability.RiskLevelExternal},
	}
	teamDefinition := &team.Definition{
		ID: "gtm-research", Version: version, DisplayName: "GTM research", Purpose: "Research demand and synthesize findings",
		Roles:        []team.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Collect evidence", MinimumMembers: 1, RequiredDefinitionIDs: []string{agentDefinition.ID}, ChannelParticipation: team.RoleChannelActive}},
		Coordination: team.CoordinationPolicy{Mode: team.CoordinationDynamic, MaximumSpeakersPerRound: 2, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:   team.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 2, AllowPeerDelegation: true, RequireAcceptance: true},
		Approvals:    team.ApprovalPolicy{MaximumRisk: risk},
	}
	return WorkforceCandidate{
		Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition,
		Assignments: []Assignment{{ID: "researcher", RoleID: "researcher", AgentDefinitionID: agentDefinition.ID, DisplayName: agentDefinition.DisplayName}},
	}
}
