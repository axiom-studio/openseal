package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type staticGenerator struct {
	payload []byte
	err     error
}

type repairingGenerator struct {
	generated            []byte
	repaired             []byte
	repairSequence       [][]byte
	repairs              int
	lastError            error
	repairInvocationKeys []string
}

func (g *repairingGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	return g.generated, nil
}

func (g *repairingGenerator) Repair(_ context.Context, request GenerateRequest, _ []byte, repairError error) ([]byte, error) {
	g.repairs++
	g.lastError = repairError
	g.repairInvocationKeys = append(g.repairInvocationKeys, request.InvocationKey)
	if len(g.repairSequence) >= g.repairs {
		return g.repairSequence[g.repairs-1], nil
	}
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
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, CredentialKinds: []string{"reddit-oauth"}, MaximumRisk: capability.RiskLevelExternal},
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
			Skills:               map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, CredentialKinds: []string{"reddit-oauth"}}},
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
		Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}},
	}})
	if err != nil || result.Valid || len(result.Validation) == 0 || result.Validation[0].Code != "unknown_role" {
		t.Fatalf("invalid composition = %#v, err = %v", result, err)
	}
	strict, _ := NewCompiler(staticGenerator{payload: []byte(`{"candidate":{"agents":[]},"hiddenReasoning":"no"}`)})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a team"}); err == nil {
		t.Fatal("unknown generator output should fail closed")
	}
}

func TestCompilerRequiresOneSpeakingRoleForPromptCreatedTeams(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Team.Roles[0].ChannelParticipation = team.RoleChannelObserveOnly
	issues := validateCandidate(&candidate, nil)
	if !hasValidationCode(issues, "no_speaking_role") {
		t.Fatalf("all-observe-only Team issues = %#v", issues)
	}

	candidate.Team.Roles = append(candidate.Team.Roles, team.RoleSlot{
		ID: "reviewer", DisplayName: "Reviewer", Purpose: "Share reviewed findings",
		ChannelParticipation: team.RoleChannelActive,
	})
	issues = validateCandidate(&candidate, nil)
	if hasValidationCode(issues, "no_speaking_role") {
		t.Fatalf("mixed-participation Team issues = %#v", issues)
	}
}

func TestCompilerPerformsBoundedStrictSchemaRepair(t *testing.T) {
	valid, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	generator := &repairingGenerator{generated: []byte(`{"candidate":{"agents":[]},"unknown":true}`), repaired: valid}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
	})
	if err != nil || !result.Valid || generator.repairs != 1 {
		t.Fatalf("repaired result = %#v, repairs = %d, err = %v", result, generator.repairs, err)
	}
	generator = &repairingGenerator{
		generated: []byte(`{"unknown":true}`), repairSequence: [][]byte{[]byte(`{"stillUnknown":true}`), valid},
	}
	compiler, _ = NewCompiler(generator)
	result, err = compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create", InvocationKey: "change-set:one:0",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
	})
	if err != nil || !result.Valid || generator.repairs != 2 || len(generator.repairInvocationKeys) != 2 || generator.repairInvocationKeys[0] == generator.repairInvocationKeys[1] {
		t.Fatalf("second bounded schema repair = %#v, repairs = %d, keys = %#v, err = %v", result, generator.repairs, generator.repairInvocationKeys, err)
	}

	generator = &repairingGenerator{
		generated: []byte(`{"unknown":true}`), repairSequence: [][]byte{[]byte(`{"stillUnknown":true}`), []byte(`{"alsoUnknown":true}`)},
	}
	compiler, _ = NewCompiler(generator)
	if _, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil || generator.repairs != maximumSchemaRepairAttempts {
		t.Fatalf("schema repair budget was not enforced, repairs = %d, err = %v", generator.repairs, err)
	} else {
		var schemaError *SchemaGenerationError
		if !errors.As(err, &schemaError) || schemaError.RepairAttempts != maximumSchemaRepairAttempts || schemaError.Diagnostic != "unknown field alsoUnknown" {
			t.Fatalf("schema failure diagnostic = %#v, err = %v", schemaError, err)
		}
	}
}

func TestCompilerNormalizesOnlyDeclaredHumanDurationFields(t *testing.T) {
	payload := []byte(`{"candidate":{"agents":[{"id":"worker","version":"1","displayName":"Worker","purpose":"Work safely","systemPrompt":"Do the work.","authority":{"maximumRisk":"read","maxConcurrentRuns":1},"memory":{"retention":"30d","maximumBytes":1024},"escalation":{"afterDuration":"2h"}}],"team":{"id":"workers","version":"1","displayName":"Workers","purpose":"Coordinate work","roles":[{"id":"worker","displayName":"Worker","purpose":"Perform work","minimumMembers":1,"maximumMembers":1,"channelParticipation":"active"}],"coordination":{"mode":"dynamic"},"sharedContext":{"retention":"2w","maximumBytes":2048},"approvals":{"maximumRisk":"read"}},"assignments":[{"id":"worker","roleId":"worker","agentDefinitionId":"worker","displayName":"Worker"}]}}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Team"})
	if err != nil || !result.Valid {
		t.Fatalf("normalized duration compile = %#v, err = %v", result, err)
	}
	if got := result.Candidate.Agents[0].Memory.Retention; got != 30*24*time.Hour {
		t.Fatalf("agent retention = %s", got)
	}
	if got := result.Candidate.Agents[0].Escalation.AfterDuration; got != 2*time.Hour {
		t.Fatalf("escalation duration = %s", got)
	}
	if got := result.Candidate.Team.SharedContext.Retention; got != 14*24*time.Hour {
		t.Fatalf("team retention = %s", got)
	}

	strict, _ := NewCompiler(staticGenerator{payload: []byte(`{"candidate":{"agents":[]},"questions":"tomorrow"}`)})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil {
		t.Fatal("normalization must not coerce undeclared fields")
	}
	invalid, _ := NewCompiler(staticGenerator{payload: []byte(`{"candidate":{"agents":[{"memory":{"retention":"someday"}}]}}`)})
	if _, err := invalid.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil {
		t.Fatal("ambiguous duration must fail closed")
	}
}

func TestCompilerNormalizesOnlyCanonicalRefinementProvenanceShorthand(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"communities","category":"scope","prompt":"Which communities are permitted?","whyNeeded":"Monitoring needs an explicit source scope.","blocking":["apply"],"answer":{"kind":"string_list"},"provenance":"prompt","priority":100},{"id":"skill","category":"skill","prompt":"Which Skill should be used?","whyNeeded":"Execution needs a compatible Skill.","blocking":["apply"],"answer":{"kind":"skill_selection","options":[{"id":"reddit-research","label":"Reddit research"}]},"provenance":["catalog","skill"],"priority":90}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || len(result.UnresolvedQuestions) != 2 {
		t.Fatalf("normalized refinement result = %#v, err = %v", result, err)
	}
	if got := result.UnresolvedQuestions[0].Provenance; len(got) != 1 || got[0].Kind != RefinementProvenancePrompt {
		t.Fatalf("single provenance shorthand = %#v", got)
	}
	if got := result.UnresolvedQuestions[1].Provenance; len(got) != 2 || got[0].Kind != RefinementProvenanceCatalog || got[1].Kind != RefinementProvenanceSkill {
		t.Fatalf("provenance shorthand list = %#v", got)
	}

	strictPayload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Scope?","whyNeeded":"Required.","blocking":["apply"],"answer":{"kind":"text"},"provenance":"invented","priority":1}]}`)
	strict, _ := NewCompiler(staticGenerator{payload: strictPayload})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil {
		t.Fatal("unknown refinement provenance shorthand must fail closed")
	}
}

func TestCompilerNormalizesOnlyCanonicalRefinementBlockingShorthand(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"communities","category":"scope","prompt":"Which communities are permitted?","whyNeeded":"Monitoring needs an explicit source scope.","blocking":"apply","answer":{"kind":"string_list"},"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"})
	if err != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("normalized refinement result = %#v, err = %v", result, err)
	}
	if got := result.UnresolvedQuestions[0].Blocking; len(got) != 1 || got[0] != RefinementBlocksApply {
		t.Fatalf("blocking shorthand = %#v", got)
	}

	strictPayload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Scope?","whyNeeded":"Required.","blocking":"later","answer":{"kind":"text"},"priority":1}]}`)
	strict, _ := NewCompiler(staticGenerator{payload: strictPayload})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil {
		t.Fatal("unknown refinement blocking shorthand must fail closed")
	}
}

func TestCompilerPerformsOneDeterministicContractRepair(t *testing.T) {
	invalid := marketingCandidate("1", capability.RiskLevelRead)
	invalid.Assignments[0].RoleID = "invented-role"
	generated, _ := json.Marshal(GenerationResponse{Candidate: invalid, Questions: []string{"Would you prefer another role?"}})
	repaired, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead), Assumptions: []string{"Used the declared researcher role."}})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || !result.Valid || generator.repairs != 1 || len(result.Validation) != 0 || len(result.Questions) != 0 || len(result.Assumptions) != 1 {
		t.Fatalf("contract-repaired result = %#v, repairs = %d, err = %v", result, generator.repairs, err)
	}
}

func TestCompilerValidatesInitiativeBlueprintAndExactMonitorCapability(t *testing.T) {
	candidate := researchInitiativeCandidate()
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}},
	}, SourcePolicies: map[string]SourcePolicyCapability{
		"approved-communities": {Reference: "approved-communities", Sources: []SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5},
	}}
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a continuing market research initiative", Catalog: catalog})
	if err != nil || !result.Valid || len(result.Validation) != 0 || len(result.MissingRequirements) != 0 {
		t.Fatalf("valid Initiative compile = %#v, err = %v", result, err)
	}

	wrongOwner := researchInitiativeCandidate()
	wrongOwner.Initiative.SourceMonitors[0].ObjectiveRef = WorkforceObjectiveKey(InitiativeOwnerAgent, "community-researcher", "collect")
	wrongOwnerPayload, _ := json.Marshal(GenerationResponse{Candidate: wrongOwner})
	compiler, _ = NewCompiler(staticGenerator{payload: wrongOwnerPayload})
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create", Catalog: catalog})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "source_monitor_owner_mismatch") {
		t.Fatalf("wrong monitor owner = %#v, err = %v", result, err)
	}

	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	catalog.Skills["community-source"] = SkillCapability{ID: "community-source", Version: "2.0.0", Actions: []string{"observe"}}
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "version" || result.MissingRequirements[0].ID != "community-source@1.2.3" {
		t.Fatalf("monitor version mismatch = %#v, err = %v", result, err)
	}

	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	catalog.Skills["community-source"] = SkillCapability{ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}}
	delete(catalog.SourcePolicies, "approved-communities")
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "source_policy" || result.MissingRequirements[0].ID != "approved-communities" {
		t.Fatalf("missing source policy = %#v, err = %v", result, err)
	}

	outOfScope := researchInitiativeCandidate()
	monitorTemplate := &outOfScope.Team.ObjectiveTemplates[0]
	monitorTemplate.Cadence["runTemplate"].(map[string]interface{})["capability"].(map[string]interface{})["inputs"] = map[string]interface{}{
		"url": "https://attacker.example/feed", "maxItems": float64(5),
	}
	outOfScopePayload, _ := json.Marshal(GenerationResponse{Candidate: outOfScope})
	compiler, _ = NewCompiler(staticGenerator{payload: outOfScopePayload})
	catalog.SourcePolicies["approved-communities"] = SourcePolicyCapability{Reference: "approved-communities", Sources: []SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5}
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "source_scope" {
		t.Fatalf("out-of-policy monitor source = %#v, err = %v", result, err)
	}
}

func TestCompilerRejectsCadenceThatCannotExecute(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "weekly", Title: "Weekly", Goal: "Report weekly", Priority: 1,
		Cadence: map[string]interface{}{"assignedAgentId": candidate.Agents[0].ID, "interval": float64(604800000000000)},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "invalid_objective_cadence") {
		t.Fatalf("non-executable cadence = %#v, err = %v", result, err)
	}
}

func TestCompilerRejectsHostedCadenceBudgetBelowPortableFloor(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "weekly", Title: "Weekly", Goal: "Synthesize retained evidence", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "interval", "intervalSeconds": float64(3600),
			"runBudget": map[string]interface{}{
				"maxAttempts": float64(3), "maxTurns": float64(3), "maxInputTokens": float64(5000),
				"maxOutputTokens": float64(10000), "maxTotalTokens": float64(15000),
			},
		},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "invalid_objective_cadence") ||
		!validationMessageContains(result.Validation, "maxInputTokens must be zero (unbounded) or at least 16000") {
		t.Fatalf("impossible hosted budget = %#v, err = %v", result, err)
	}
}

func TestCompilerAcceptsBoundedHostedEvidenceProjection(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "weekly", Title: "Weekly", Goal: "Synthesize retained evidence", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "interval", "intervalSeconds": float64(3600),
			"runBudget": map[string]interface{}{
				"maxAttempts": float64(5), "maxTurns": float64(4), "maxInputTokens": float64(32000),
				"maxOutputTokens": float64(30000), "maxTotalTokens": float64(62000),
			},
			"runTemplate": map[string]interface{}{"evidenceProjection": map[string]interface{}{
				"maximumObservations": float64(7), "maximumSummaryRunes": float64(600), "maximumTotalRunes": float64(4200),
			}},
		},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || !result.Valid {
		t.Fatalf("bounded hosted evidence candidate = %#v, err = %v", result, err)
	}
	projection := result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence["runTemplate"].(map[string]interface{})["evidenceProjection"].(map[string]interface{})
	if projection["maximumObservations"] != float64(7) || projection["maximumSummaryRunes"] != float64(600) || projection["maximumTotalRunes"] != float64(4200) {
		t.Fatalf("evidence projection did not round-trip: %#v", projection)
	}
}

func TestCompilerRejectsEvidenceProjectionBudgetWithoutReviewAndRepairCapacity(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "weekly", Title: "Weekly", Goal: "Synthesize retained evidence", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "interval", "intervalSeconds": float64(3600),
			"runBudget": map[string]interface{}{
				"maxAttempts": float64(5), "maxTurns": float64(3), "maxInputTokens": float64(16000),
				"maxOutputTokens": float64(10000), "maxTotalTokens": float64(26000),
			},
			"runTemplate": map[string]interface{}{"evidenceProjection": map[string]interface{}{
				"maximumObservations": float64(7), "maximumSummaryRunes": float64(600), "maximumTotalRunes": float64(4200),
			}},
		},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"})
	if err != nil || result.Valid || !validationMessageContains(result.Validation, "maxTurns must be zero (unbounded) or at least 4") {
		t.Fatalf("insufficient evidence-grounding budget = %#v, err = %v", result, err)
	}
}

func TestCompilerRejectsEvidenceProjectionBudgetWithoutRetryCapacity(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "weekly", Title: "Weekly", Goal: "Synthesize retained evidence", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "interval", "intervalSeconds": float64(3600),
			"runBudget": map[string]interface{}{
				"maxAttempts": float64(4), "maxTurns": float64(4), "maxInputTokens": float64(32000),
				"maxOutputTokens": float64(30000), "maxTotalTokens": float64(62000),
			},
			"runTemplate": map[string]interface{}{"evidenceProjection": map[string]interface{}{
				"maximumObservations": float64(7), "maximumSummaryRunes": float64(600), "maximumTotalRunes": float64(4200),
			}},
		},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"})
	if err != nil || result.Valid || !validationMessageContains(result.Validation, "maxAttempts must be zero (unbounded) or at least 5") {
		t.Fatalf("insufficient evidence-grounding attempt budget = %#v, err = %v", result, err)
	}
}

func TestCompilerRejectsEventCapabilityBudgetThatCannotComplete(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].ObjectiveTemplates = []workforce.ObjectiveTemplate{{
		ID: "warnings", Title: "Warnings", Goal: "Inspect warning evidence", Priority: 1,
		EventRules: map[string]interface{}{"version": "1", "rules": []interface{}{map[string]interface{}{
			"id": "warning", "eventType": "kubernetes.warning", "assignedAgentId": candidate.Agents[0].ID,
			"runBudget": map[string]interface{}{"maxAttempts": float64(1), "maxTurns": float64(3), "maxActions": float64(1)},
			"runTemplate": map[string]interface{}{"capability": map[string]interface{}{
				"skillId": "reddit-research", "skillVersion": "1.0.0", "action": "read",
			}},
		}}},
	}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "invalid_objective_event_rules") {
		t.Fatalf("non-completable event capability = %#v, err = %v", result, err)
	}
}

func hasValidationCode(issues []ValidationIssue, code string) bool {
	for _, value := range issues {
		if value.Code == code {
			return true
		}
	}
	return false
}

func validationMessageContains(issues []ValidationIssue, fragment string) bool {
	for _, value := range issues {
		if strings.Contains(value.Message, fragment) {
			return true
		}
	}
	return false
}

func researchInitiativeCandidate() WorkforceCandidate {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	agentDefinition := candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: "community-source", VersionConstraint: "1.2.3", RequiredActions: []string{"observe"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{"community-source"}
	agentDefinition.ObjectiveTemplates = []workforce.ObjectiveTemplate{{ID: "collect", Title: "Collect evidence", Goal: "Collect permitted community evidence", Priority: 1}}
	candidate.Team.ObjectiveTemplates = []workforce.ObjectiveTemplate{
		{
			ID: "monitor", Title: "Monitor communities", Goal: "Monitor approved sources over time", Priority: 1,
			Cadence: map[string]interface{}{
				"type": "interval", "intervalSeconds": float64(3600), "assignedAgentId": "community-researcher", "maximumConcurrent": float64(1),
				"runBudget": map[string]interface{}{"maxTurns": float64(2), "maxActions": float64(1), "maxDurationMs": float64(60000)},
				"runTemplate": map[string]interface{}{
					"entrypoint": "monitor",
					"context":    map[string]interface{}{"initiativeId": "market-intelligence", "sourceMonitorId": "community-listening"},
					"policy":     map[string]interface{}{"sourcePolicyRef": "approved-communities"},
					"capability": map[string]interface{}{"skillId": "community-source", "skillVersion": "1.2.3", "action": "observe", "inputs": map[string]interface{}{"url": "https://community.example/feed", "maxItems": float64(5)}},
				},
			},
		},
		{ID: "report", Title: "Publish report", Goal: "Synthesize a cited report", Priority: 2},
	}
	candidate.Initiative = &InitiativeBlueprint{
		ID: "market-intelligence", Title: "Market intelligence", Purpose: "Continuously understand user pain points",
		Owner: InitiativeOwnerReference{Type: InitiativeOwnerTeam, DefinitionID: candidate.Team.ID},
		ObjectiveRefs: []string{
			WorkforceObjectiveKey(InitiativeOwnerAgent, agentDefinition.ID, "collect"),
			WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, "monitor"),
			WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, "report"),
		},
		Milestones: []InitiativeMilestoneBlueprint{{ID: "baseline", Title: "Establish baseline", ObjectiveRefs: []string{WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, "monitor")}}},
		Hypotheses: []InitiativeHypothesisBlueprint{{ID: "setup-friction", Statement: "Setup friction is a leading adoption barrier", Confidence: 0.5}},
		SourceMonitors: []InitiativeSourceMonitorBlueprint{{
			ID: "community-listening", ObjectiveRef: WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, "monitor"), AssignedAgentDefinitionID: agentDefinition.ID,
			SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe", SourcePolicyRef: "approved-communities", Deduplication: InitiativeDeduplicateStableSourceAndContent,
		}},
		Deliverables: []InitiativeDeliverableBlueprint{{ID: "monthly-report", Title: "Monthly cited report", ObjectiveRefs: []string{WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, "report")}}},
		Policy:       map[string]interface{}{"outreachApproval": "required"},
	}
	return candidate
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
