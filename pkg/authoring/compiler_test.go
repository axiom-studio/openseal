package authoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/source"
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
	repairErrors         []error
	repairInvocationKeys []string
}

func (g *repairingGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	return g.generated, nil
}

func (g *repairingGenerator) Repair(_ context.Context, request GenerateRequest, _ []byte, repairError error) ([]byte, error) {
	g.repairs++
	g.lastError = repairError
	g.repairErrors = append(g.repairErrors, repairError)
	g.repairInvocationKeys = append(g.repairInvocationKeys, request.InvocationKey)
	if len(g.repairSequence) >= g.repairs {
		return g.repairSequence[g.repairs-1], nil
	}
	return g.repaired, nil
}

func (g staticGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	return g.payload, g.err
}

func testRefinementQuestion(id, prompt string) RefinementQuestion {
	return RefinementQuestion{
		ID:         id,
		Category:   RefinementCategoryOther,
		Prompt:     prompt,
		WhyNeeded:  "The workforce candidate requires an explicit answer.",
		Blocking:   []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:     RefinementAnswerSchema{Kind: RefinementAnswerText},
		Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
		Priority:   1,
	}
}

func TestCompilerVerifiesPromptGeneratedWorkforceAndCapabilityGaps(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelExternal)
	payload, _ := json.Marshal(GenerationResponse{
		Candidate: candidate, Assumptions: []string{"Public replies use the configured brand identity", "Public replies use the configured brand identity"},
		UnresolvedQuestions: []RefinementQuestion{
			testRefinementQuestion("approved-communities", "Which communities are approved for outreach?"),
		},
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
	if result.Valid || len(result.Validation) != 0 || len(result.Assumptions) != 1 || len(result.UnresolvedQuestions) != 1 ||
		len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "credential" || result.MissingRequirements[0].ID != "reddit-oauth" {
		t.Fatalf("compile result = %#v", result)
	}
	if len(result.Diff) != 1 || result.Diff[0].Path != "workforce" {
		t.Fatalf("create diff = %#v", result.Diff)
	}
}

func TestCompilerDefersExecutionCredentialSetupForExplicitlyInactiveAgent(t *testing.T) {
	candidate := WorkforceCandidate{
		Agents: []*agent.AgentDefinition{{
			ID: "security-reviewer", Version: "1", DisplayName: "Security reviewer",
			Purpose: "Review cluster security", SystemPrompt: "Review cluster security without acting until activated.",
			SkillRequirements: []agent.SkillRequirement{{
				SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
			}},
			Authority: agent.AuthorityPolicy{
				MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"posture"},
				MaxConcurrentRuns: 1, RequireApprovalAt: capability.RiskLevelWrite,
			},
		}},
		Activation: WorkforceActivationInactive,
	}
	credentialQuestion := RefinementQuestion{
		ID: "credential-toolweb", Category: RefinementCategoryCredential,
		Prompt: "Which authorized credential should be used?", WhyNeeded: "The Skill needs execution authority.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference},
		Provenance: []RefinementQuestionProvenance{{
			Kind: RefinementProvenanceCredential,
		}},
		Priority: 1,
	}
	payload, err := json.Marshal(GenerationResponse{
		Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{credentialQuestion},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{"posture": {
		ID: "posture", Version: "1.0.0", Actions: []string{"execute"},
		Readiness: SkillReadinessNeedsBinding, MaximumRisk: capability.RiskLevelExternal,
		Credentials: []SkillCredential{{
			Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
		}},
	}}}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create one security reviewer. Do not activate.", Catalog: catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.UnresolvedQuestions) != 0 || len(result.MissingRequirements) != 0 ||
		result.Candidate.Activation != WorkforceActivationInactive {
		t.Fatalf("inactive credential setup was not deferred = %#v", result)
	}

	candidate.Activation = WorkforceActivationActive
	activePayload, err := json.Marshal(GenerationResponse{
		Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{credentialQuestion},
	})
	if err != nil {
		t.Fatal(err)
	}
	activeCompiler, err := NewCompiler(staticGenerator{payload: activePayload})
	if err != nil {
		t.Fatal(err)
	}
	active, err := activeCompiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create and activate one security reviewer.", Catalog: catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.Valid || len(active.UnresolvedQuestions) != 1 {
		t.Fatalf("active credential question was not preserved = %#v", active)
	}
	foundCredential, foundBinding := false, false
	for _, missing := range active.MissingRequirements {
		foundCredential = foundCredential || missing.Kind == "credential" && missing.ID == "TOOLWEB_API_KEY"
		foundBinding = foundBinding || missing.Kind == "skill_binding" && missing.ID == "posture"
	}
	if !foundCredential || !foundBinding {
		t.Fatalf("active execution setup requirements = %#v", active.MissingRequirements)
	}
}

func TestCompilerAcceptsPromptAuthoredCallableRunbook(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "release-agent", Version: "1", DisplayName: "Release Agent",
		Purpose: "Prepare releases", SystemPrompt: "Prepare releases safely.",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "release", VersionConstraint: "1.0.0", RequiredActions: []string{"collect"},
		}},
		Authority: agent.AuthorityPolicy{
			MaximumRisk: capability.RiskLevelRead, AllowedSkillIDs: []string{"release"}, MaxConcurrentRuns: 1,
		},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "release-evidence", Version: "1", Name: "Release evidence",
			Entrypoints: map[string]string{"collect": "collect"},
			Interfaces: map[string]runbook.Interface{"collect": {
				Description: "Collect release evidence in the exact required order.",
				InputSchema: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{"release": map[string]interface{}{"type": "string"}},
					"required":   []interface{}{"release"},
				},
				OutputSchema: map[string]interface{}{"type": "object"},
			}},
			Steps: map[string]runbook.Step{
				"collect": {
					Kind: runbook.StepAction,
					Action: &runbook.ActionStep{
						SkillID: "release", SkillVersion: "1.0.0", Action: "collect",
						Arguments:  map[string]runbook.Value{"release": {Ref: "/input/release"}},
						ResultPath: "/results/collect", Next: "done",
					},
				},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		},
	}}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(t.Context(), GenerateRequest{
		Mode:   ModeCreate,
		Prompt: "Create one Agent with a deterministic repeatable operation that collects release evidence in an exact sequence.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"release": {ID: "release", Version: "1.0.0", Actions: []string{"collect"}, MaximumRisk: capability.RiskLevelRead},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.Candidate.Agents) != 1 || result.Candidate.Agents[0].Runbook == nil ||
		result.Candidate.Agents[0].Runbook.Interfaces["collect"].Description == "" {
		t.Fatalf("result=%#v", result)
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
		if !errors.As(err, &schemaError) || schemaError.RepairAttempts != maximumSchemaRepairAttempts || !strings.Contains(schemaError.Diagnostic, "unknown field alsoUnknown at alsoUnknown") {
			t.Fatalf("schema failure diagnostic = %#v, err = %v", schemaError, err)
		}
	}
}

func TestCompilerReportsCredentialFreeGenerationAndSchemaRepairPhases(t *testing.T) {
	valid, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	generator := &repairingGenerator{generated: []byte(`{"candidate":{"agents":"invalid"}}`), repaired: valid}
	compiler, _ := NewCompiler(generator)
	var progress []CompileProgress
	_, err := compiler.CompileWithProgress(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a marketing team", Catalog: CapabilityCatalog{
			Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}},
		},
	}, func(value CompileProgress) { progress = append(progress, value) })
	if err != nil {
		t.Fatal(err)
	}
	want := []CompileProgress{
		{Phase: CompilePhaseCapabilityResolve, Attempt: 1, MaximumAttempts: 1},
		{Phase: CompilePhaseProviderRequest, Attempt: 1, MaximumAttempts: 1},
		{Phase: CompilePhaseCandidateValidate, Attempt: 1, MaximumAttempts: 1},
		{Phase: CompilePhaseSchemaRepair, Attempt: 1, MaximumAttempts: maximumSchemaRepairAttempts},
		{Phase: CompilePhaseCandidateValidate, Attempt: 1, MaximumAttempts: 1},
	}
	if !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %#v, want %#v", progress, want)
	}
}

func TestCompilerRepairsLiveUnknownIDWithExactSchemaPath(t *testing.T) {
	valid, err := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	if err != nil {
		t.Fatal(err)
	}
	// Reproduces the live provider's ambiguous `unknown field id` response. ID
	// is valid throughout the document, but not in a SkillRequirement.
	invalid := bytes.Replace(valid, []byte(`"skillId":"reddit-research"`), []byte(`"id":"reddit-research"`), 1)
	generator := &repairingGenerator{generated: invalid, repaired: valid}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a market research Team", InvocationKey: "change-set:live-unknown-id:0",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result == nil || !result.Valid || generator.repairs != 1 {
		t.Fatalf("path-guided repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	diagnostic := generator.repairErrors[0].Error()
	if !strings.Contains(diagnostic, "candidate.agents[0].skillRequirements[0].id") ||
		!strings.Contains(diagnostic, "allowed:") || !strings.Contains(diagnostic, "skillId") {
		t.Fatalf("unknown-field repair diagnostic = %q", diagnostic)
	}
	if strings.Contains(string(valid), "credential://") || strings.Contains(diagnostic, "credential://") {
		t.Fatal("schema repair diagnostic exposed credential material")
	}
}

func deterministicRunbookPayloads(t *testing.T) (valid, invalid []byte) {
	t.Helper()
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "publisher", Version: "1", DisplayName: "Publisher",
		Purpose: "Render reports", SystemPrompt: "Render approved reports.",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "openseal.document", VersionConstraint: "1.0.2", RequiredActions: []string{"render_pdf"},
		}},
		Authority: agent.AuthorityPolicy{
			MaximumRisk: capability.RiskLevelWrite, AllowedSkillIDs: []string{"openseal.document"}, MaxConcurrentRuns: 1,
		},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "render-report", Version: "1", Name: "Render report",
			Entrypoints: map[string]string{"render_report": "render"},
			Interfaces: map[string]runbook.Interface{"render_report": {
				Description:  "Render an approved report as a PDF.",
				InputSchema:  map[string]interface{}{"type": "object"},
				OutputSchema: map[string]interface{}{"type": "object"},
			}},
			Steps: map[string]runbook.Step{
				"render": {
					Kind: runbook.StepAction,
					Action: &runbook.ActionStep{
						SkillID: "openseal.document", SkillVersion: "1.0.2", Action: "render_pdf",
						ResultPath: "/results/report", Next: "done",
					},
				},
				"done": {
					Kind: runbook.StepEnd,
					End: &runbook.EndStep{Outputs: map[string]runbook.Value{
						"artifact": {Ref: "/results/report"},
					}},
				},
			},
		},
	}}}
	valid, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(valid, &document); err != nil {
		t.Fatal(err)
	}
	agents := document["candidate"].(map[string]interface{})["agents"].([]interface{})
	steps := agents[0].(map[string]interface{})["runbook"].(map[string]interface{})["steps"].(map[string]interface{})
	render := steps["render"].(map[string]interface{})
	action := render["action"].(map[string]interface{})
	render["resultPath"] = action["resultPath"]
	delete(action, "resultPath")
	invalid, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return valid, invalid
}

func TestCompilerRepairsUnknownFieldInsideTypedRunbookStepMap(t *testing.T) {
	valid, invalid := deterministicRunbookPayloads(t)
	generator := &repairingGenerator{generated: invalid, repairSequence: [][]byte{invalid, valid}}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(t.Context(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a deterministic report publisher.", InvocationKey: "change-set:runbook-map:0",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.document": {
				ID: "openseal.document", Version: "1.0.2", Actions: []string{"render_pdf"},
				MaximumRisk: capability.RiskLevelWrite,
			},
		}},
	})
	if err != nil || result == nil || !result.Valid || generator.repairs != 2 {
		t.Fatalf("path-guided runbook repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	for attempt, repairError := range generator.repairErrors {
		diagnostic := repairError.Error()
		if !strings.Contains(diagnostic, "candidate.agents[0].runbook.steps.render.resultPath") ||
			!strings.Contains(diagnostic, "allowed:") ||
			!strings.Contains(diagnostic, "move to candidate.agents[0].runbook.steps.render.action.resultPath") {
			t.Fatalf("runbook repair diagnostic %d = %q", attempt+1, diagnostic)
		}
	}
}

func TestCompilerRejectsPersistentlyInvalidRunbookStepPlacement(t *testing.T) {
	_, invalid := deterministicRunbookPayloads(t)
	generator := &repairingGenerator{
		generated: invalid, repairSequence: [][]byte{invalid, invalid},
	}
	compiler, _ := NewCompiler(generator)
	_, err := compiler.Compile(t.Context(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a deterministic report publisher.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.document": {
				ID: "openseal.document", Version: "1.0.2", Actions: []string{"render_pdf"},
				MaximumRisk: capability.RiskLevelWrite,
			},
		}},
	})
	var schemaError *SchemaGenerationError
	if !errors.As(err, &schemaError) || generator.repairs != maximumSchemaRepairAttempts ||
		!strings.Contains(schemaError.Diagnostic, "move to candidate.agents[0].runbook.steps.render.action.resultPath") {
		t.Fatalf("persistent invalid runbook error=%#v repairs=%d err=%v", schemaError, generator.repairs, err)
	}
}

func TestCompilerCanonicalizesUnambiguousRawRunbookValues(t *testing.T) {
	valid, _ := deterministicRunbookPayloads(t)
	var document map[string]interface{}
	if err := json.Unmarshal(valid, &document); err != nil {
		t.Fatal(err)
	}
	agents := document["candidate"].(map[string]interface{})["agents"].([]interface{})
	steps := agents[0].(map[string]interface{})["runbook"].(map[string]interface{})["steps"].(map[string]interface{})
	outputs := steps["done"].(map[string]interface{})["end"].(map[string]interface{})["outputs"].(map[string]interface{})
	outputs["artifact"] = "/results/report"
	outputs["filename"] = "report.pdf"
	invalid, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	generator := &repairingGenerator{generated: invalid}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(t.Context(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a deterministic report publisher.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.document": {
				ID: "openseal.document", Version: "1.0.2", Actions: []string{"render_pdf"},
				MaximumRisk: capability.RiskLevelWrite,
			},
		}},
	})
	if err != nil || result == nil || !result.Valid || generator.repairs != 0 {
		t.Fatalf("Runbook Value repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	output := result.Candidate.Agents[0].Runbook.Steps["done"].End.Outputs["artifact"]
	if output.Ref != "/results/report" || len(output.Literal) != 0 || len(output.Template) != 0 {
		t.Fatalf("canonicalized Runbook Value = %#v", output)
	}
	filename := result.Candidate.Agents[0].Runbook.Steps["done"].End.Outputs["filename"]
	if string(filename.Literal) != `"report.pdf"` || filename.Ref != "" || len(filename.Template) != 0 {
		t.Fatalf("canonicalized literal Runbook Value = %#v", filename)
	}
}

func TestNormalizeGeneratedRunbookValuesCanonicalizesTransformTargets(t *testing.T) {
	payload := []byte(`{"candidate":{"agents":[{"runbook":{"steps":{"extract":{"kind":"transform","transform":{"assignments":{"usernameRef":{"ref":"/results/snapshot/elements/0/ref"},"/state/existing":{"literal":true},"ambiguous/target":{"literal":false}},"next":"done"}}}}}]}}`)

	normalized := normalizeGeneratedRunbookValues(payload)
	var document map[string]interface{}
	if err := json.Unmarshal(normalized, &document); err != nil {
		t.Fatal(err)
	}
	agents := document["candidate"].(map[string]interface{})["agents"].([]interface{})
	steps := agents[0].(map[string]interface{})["runbook"].(map[string]interface{})["steps"].(map[string]interface{})
	assignments := steps["extract"].(map[string]interface{})["transform"].(map[string]interface{})["assignments"].(map[string]interface{})
	if _, ok := assignments["/results/usernameRef"]; !ok {
		t.Fatalf("canonicalized assignments = %#v", assignments)
	}
	if _, ok := assignments["usernameRef"]; ok {
		t.Fatalf("bare transform target survived normalization: %#v", assignments)
	}
	if _, ok := assignments["/state/existing"]; !ok {
		t.Fatalf("valid JSON Pointer changed during normalization: %#v", assignments)
	}
	if _, ok := assignments["ambiguous/target"]; !ok {
		t.Fatalf("ambiguous invalid target was guessed during normalization: %#v", assignments)
	}
}

func TestCompilerPreservesStrictRunbookValueObjectDiagnostics(t *testing.T) {
	valid, _ := deterministicRunbookPayloads(t)
	var document map[string]interface{}
	if err := json.Unmarshal(valid, &document); err != nil {
		t.Fatal(err)
	}
	agents := document["candidate"].(map[string]interface{})["agents"].([]interface{})
	steps := agents[0].(map[string]interface{})["runbook"].(map[string]interface{})["steps"].(map[string]interface{})
	outputs := steps["done"].(map[string]interface{})["end"].(map[string]interface{})["outputs"].(map[string]interface{})
	outputs["artifact"] = map[string]interface{}{"reff": "/results/report"}
	invalid, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	generator := &repairingGenerator{generated: invalid, repairSequence: [][]byte{invalid, invalid}}
	compiler, _ := NewCompiler(generator)
	_, err = compiler.Compile(t.Context(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a deterministic report publisher.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.document": {
				ID: "openseal.document", Version: "1.0.2", Actions: []string{"render_pdf"},
				MaximumRisk: capability.RiskLevelWrite,
			},
		}},
	})
	var schemaError *SchemaGenerationError
	if !errors.As(err, &schemaError) || !strings.Contains(schemaError.Diagnostic, "unknown field reff") {
		t.Fatalf("misspelled Runbook Value field error=%#v err=%v", schemaError, err)
	}
}

func TestCompilerRepairsLiveRefinementMissingFieldsWithExactQuestionPath(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalid, err := json.Marshal(GenerationResponse{
		Candidate: candidate,
		UnresolvedQuestions: []RefinementQuestion{{
			Prompt:   "Which authorized communities should be monitored?",
			Blocking: []RefinementBlockingScope{RefinementBlocksApply},
			Answer:   RefinementAnswerSchema{Kind: RefinementAnswerStringList},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stillInvalid, err := json.Marshal(GenerationResponse{
		Candidate: candidate,
		UnresolvedQuestions: []RefinementQuestion{{
			ID: "communities", Category: RefinementCategoryScope,
			Prompt:    "Which authorized communities should be monitored?",
			WhyNeeded: "Monitoring targets must remain bounded.",
			Blocking:  []RefinementBlockingScope{RefinementBlocksApply},
			Answer:    RefinementAnswerSchema{Kind: RefinementAnswerStringList},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := json.Marshal(GenerationResponse{
		Candidate: candidate,
		UnresolvedQuestions: []RefinementQuestion{{
			ID: "communities", Category: RefinementCategoryScope,
			Prompt:     "Which authorized communities should be monitored?",
			WhyNeeded:  "Monitoring targets must remain bounded.",
			Blocking:   []RefinementBlockingScope{RefinementBlocksApply},
			Answer:     RefinementAnswerSchema{Kind: RefinementAnswerStringList},
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
			Priority:   100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	generator := &repairingGenerator{generated: invalid, repairSequence: [][]byte{stillInvalid, repaired}}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team that summarizes product feedback", InvocationKey: "change-set:live-refinement:0",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result == nil || result.Valid || len(result.UnresolvedQuestions) != 1 || generator.repairs != 2 {
		t.Fatalf("question-path repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	if first := generator.repairErrors[0].Error(); !strings.Contains(first, "unresolvedQuestions[0] missing or invalid required fields: id, whyNeeded, priority") {
		t.Fatalf("first refinement diagnostic = %q", first)
	}
	if second := generator.repairErrors[1].Error(); !strings.Contains(second, "unresolvedQuestions[0] missing or invalid required fields: priority") {
		t.Fatalf("second refinement diagnostic = %q", second)
	}
}

func TestCompilerNormalizesOnlyDefinitionVersionNumbers(t *testing.T) {
	payload := []byte(`{"candidate":{"agents":[{"id":"worker","version":1.0,"displayName":"Worker","purpose":"Work safely","systemPrompt":"Do the work.","authority":{"maximumRisk":"read","maxConcurrentRuns":1}}],"team":{"id":"workers","version":2,"displayName":"Workers","purpose":"Coordinate work","roles":[{"id":"worker","displayName":"Worker","purpose":"Perform work","minimumMembers":1,"maximumMembers":1,"channelParticipation":"active"}],"coordination":{},"approvals":{"maximumRisk":"read"}},"assignments":[{"id":"worker","roleId":"worker","agentDefinitionId":"worker"}]}}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Team"})
	if err != nil || !result.Valid {
		t.Fatalf("normalized definition versions compile = %#v, err = %v", result, err)
	}
	if result.Candidate.Agents[0].Version != "1.0" || result.Candidate.Team.Version != "2" {
		t.Fatalf("definition versions = agent %q, team %q", result.Candidate.Agents[0].Version, result.Candidate.Team.Version)
	}

	strictPayload := bytes.Replace(payload, []byte(`"displayName":"Worker"`), []byte(`"displayName":7`), 1)
	strict, _ := NewCompiler(staticGenerator{payload: strictPayload})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Team"}); err == nil || !strings.Contains(err.Error(), "displayName") {
		t.Fatalf("non-version scalar mismatch must remain strict, got %v", err)
	}
}

func TestCompilerNormalizesOnlyDeclaredHumanDurationFields(t *testing.T) {
	payload := []byte(`{"candidate":{"agents":[{"id":"worker","version":"1","displayName":"Worker","purpose":"Work safely","systemPrompt":"Do the work.","authority":{"maximumRisk":"read","maxConcurrentRuns":1},"memory":{"retention":"30d","maximumBytes":1024},"escalation":{"afterDuration":"2h"}}],"team":{"id":"workers","version":"1","displayName":"Workers","purpose":"Coordinate work","roles":[{"id":"worker","displayName":"Worker","purpose":"Perform work","minimumMembers":1,"maximumMembers":1,"channelParticipation":"active"}],"coordination":{},"sharedContext":{"retention":"2w","maximumBytes":2048},"approvals":{"maximumRisk":"read"}},"assignments":[{"id":"worker","roleId":"worker","agentDefinitionId":"worker","displayName":"Worker"}]}}`)
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

func TestCompilerNormalizesOnlyUnambiguousRefinementProvenanceTypeAlias(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Which scope is permitted?","whyNeeded":"Execution needs an explicit scope.","blocking":["apply"],"answer":{"kind":"text"},"provenance":[{"type":"catalog","reference":"openseal.reddit@1.0.0","evidence":"compatible"}],"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"})
	if err != nil || result == nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("normalized provenance alias result = %#v, err = %v", result, err)
	}
	got := result.UnresolvedQuestions[0].Provenance
	if len(got) != 1 || got[0].Kind != RefinementProvenanceCatalog || got[0].Reference != "openseal.reddit@1.0.0" || got[0].Evidence != "compatible" {
		t.Fatalf("provenance alias = %#v", got)
	}

	invalid := []string{
		`{"type":"invented"}`,
		`{"type":"catalog","extra":"value"}`,
		`{"kind":"catalog","type":"catalog"}`,
		`{"type":" catalog"}`,
	}
	for _, provenance := range invalid {
		strictPayload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Scope?","whyNeeded":"Required.","blocking":["apply"],"answer":{"kind":"text"},"provenance":[` + provenance + `],"priority":1}]}`)
		strict, _ := NewCompiler(staticGenerator{payload: strictPayload})
		if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil {
			t.Fatalf("ambiguous provenance alias %s must fail closed", provenance)
		}
	}
}

func TestCompilerNormalizesOnlyExactRefinementProvenanceSourceAlias(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduces live ChangeSet ce762d1d: every provenance entry used source
	// where the canonical contract requires kind.
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Which scope is permitted?","whyNeeded":"Execution needs an explicit scope.","blocking":["apply"],"answer":{"kind":"text"},"provenance":[{"source":"prompt"},{"source":"catalog"}],"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"})
	if err != nil || result == nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("normalized source alias result = %#v, err = %v", result, err)
	}
	got := result.UnresolvedQuestions[0].Provenance
	if len(got) != 2 || got[0].Kind != RefinementProvenancePrompt || got[1].Kind != RefinementProvenanceCatalog {
		t.Fatalf("source provenance aliases = %#v", got)
	}

	invalid := []string{
		`{"source":"invented"}`,
		`{"source":"prompt","evidence":"ambiguous"}`,
		`{"kind":"prompt","source":"prompt"}`,
		`{"source":" prompt"}`,
		`{"source":7}`,
	}
	for _, provenance := range invalid {
		strictPayload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Scope?","whyNeeded":"Required.","blocking":["apply"],"answer":{"kind":"text"},"provenance":[` + provenance + `],"priority":1}]}`)
		strict, _ := NewCompiler(staticGenerator{payload: strictPayload})
		if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"}); err == nil || !strings.Contains(err.Error(), "provenance[0].source") {
			t.Fatalf("ambiguous source alias %s must fail closed, got %v", provenance, err)
		}
	}
}

func TestCompilerNormalizesOnlyCanonicalRefinementBlockingShorthand(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"communities","category":"scope","prompt":"Which communities are permitted?","whyNeeded":"Monitoring needs an explicit source scope.","blocking":"apply","answer":{"kind":"string_list"},"provenance":"prompt","priority":100}]}`)
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

func TestCompilerNormalizesOnlyCanonicalRefinementDependencyShorthand(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"communities","category":"scope","prompt":"Which communities are permitted?","whyNeeded":"Monitoring needs an explicit source scope.","blocking":["apply"],"answer":{"kind":"string_list"},"provenance":[{"kind":"prompt"}],"priority":100},{"id":"analysis-skill","category":"skill","prompt":"Which Skill should analyze the evidence?","whyNeeded":"Analysis requires an authorized Skill.","blocking":["apply"],"answer":{"kind":"skill_selection","options":[{"id":"reddit-research","label":"Reddit research"}]},"dependsOn":["communities"],"provenance":[{"kind":"catalog"}],"priority":90}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || result == nil || len(result.UnresolvedQuestions) != 2 {
		t.Fatalf("normalized dependency result = %#v, err = %v", result, err)
	}
	if got := result.UnresolvedQuestions[1].DependsOn; len(got) != 1 || got[0].QuestionID != "communities" || len(got[0].RequiredOptionIDs) != 0 {
		t.Fatalf("dependency shorthand = %#v", got)
	}

	// Conversion does not make an invented reference authoritative: the normal
	// dependency validator must still reject it.
	missing := bytes.Replace(payload, []byte(`"dependsOn":["communities"]`), []byte(`"dependsOn":["invented"]`), 1)
	strict, _ := NewCompiler(staticGenerator{payload: missing})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"}); err == nil || !strings.Contains(err.Error(), "invalid dependency") {
		t.Fatalf("invented dependency shorthand must fail closed, got %v", err)
	}

	// Whitespace changes the identifier and is not an exact shorthand.
	malformed := bytes.Replace(payload, []byte(`"dependsOn":["communities"]`), []byte(`"dependsOn":[" communities "]`), 1)
	strict, _ = NewCompiler(staticGenerator{payload: malformed})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"}); err == nil || !strings.Contains(err.Error(), "dependsOn") {
		t.Fatalf("ambiguous dependency shorthand must remain strict, got %v", err)
	}
}

func TestCompilerDiscardsProviderCredentialOptionsAtDecodeBoundary(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	payload, err := json.Marshal(GenerationResponse{
		Candidate: candidate,
		UnresolvedQuestions: []RefinementQuestion{{
			ID: "email-credential", Category: RefinementCategoryCredential,
			Prompt: "Which email credential should be configured?", WhyNeeded: "Delivery requires an authorized credential.",
			Blocking: []RefinementBlockingScope{RefinementBlocksApply},
			Answer: RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference, Options: []RefinementQuestionOption{
				{ID: "credential-reference-opaque-42", Label: "Production email"},
			}},
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}}, Priority: 100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team that emails its report."})
	if err != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("credential refinement result=%#v err=%v", result, err)
	}
	question := result.UnresolvedQuestions[0]
	if question.Answer.Kind != RefinementAnswerCredentialReference || len(question.Answer.Options) != 0 || len(question.Provenance) != 1 || question.Provenance[0].Kind != RefinementProvenanceCredential {
		t.Fatalf("normalized credential question=%#v", question)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "credential-reference-opaque-42") {
		t.Fatalf("provider credential identity persisted: %s", encoded)
	}
}

func TestCompilerDerivesOpaqueCredentialQuestionProvenance(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, _ := json.Marshal(candidate)
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"slack-credential","category":"credential","prompt":"Which authorized Slack connection should be used?","whyNeeded":"Slack access requires a configured connection.","blocking":["apply"],"answer":{"kind":"credential_reference"},"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Slack Agent."})
	if err != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("credential refinement result=%#v err=%v", result, err)
	}
	provenance := result.UnresolvedQuestions[0].Provenance
	if len(provenance) != 1 || provenance[0].Kind != RefinementProvenanceCredential || provenance[0].Reference != "" {
		t.Fatalf("derived opaque credential provenance=%#v", provenance)
	}

	invalid := bytes.Replace(payload, []byte(`"category":"credential"`), []byte(`"category":"other"`), 1)
	strict, _ := NewCompiler(staticGenerator{payload: invalid})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent."}); err == nil || !strings.Contains(err.Error(), "requires provenance") {
		t.Fatalf("missing non-credential provenance must remain strict, got %v", err)
	}
}

func TestCompilerReplacesProviderCredentialQuestionProvenanceWithHostOwnedFact(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, _ := json.Marshal(candidate)
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"credential-skill-browser","category":"credential","prompt":"Which authorized browser credential should be configured?","whyNeeded":"Browser access requires an authorized credential.","blocking":["apply"],"answer":{"kind":"credential_reference"},"provenance":[{"kind":"skill-browser","reference":"credential/opaque","evidence":"provider supplied"}],"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})

	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a browser Agent."})
	if err != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("credential refinement result=%#v err=%v", result, err)
	}
	provenance := result.UnresolvedQuestions[0].Provenance
	if len(provenance) != 1 || provenance[0].Kind != RefinementProvenanceCredential || provenance[0].Reference != "" || provenance[0].Evidence != "" {
		t.Fatalf("host-owned credential provenance=%#v", provenance)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "credential/opaque") || strings.Contains(string(encoded), "provider supplied") {
		t.Fatalf("provider credential provenance persisted: %s", encoded)
	}
}

func TestCompilerNormalizesDefinitionProvenanceKindAlias(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].Provenance.Source = "prompt"
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	payload = bytes.Replace(payload, []byte(`"provenance":{"source":"prompt"`), []byte(`"provenance":{"kind":"prompt"`), 1)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent."})
	if err != nil || result.Candidate.Agents[0].Provenance.Source != "prompt" {
		t.Fatalf("definition provenance result=%#v err=%v", result, err)
	}

	ambiguous := bytes.Replace(payload, []byte(`"kind":"prompt"`), []byte(`"kind":"prompt","source":"catalog"`), 1)
	strict, _ := NewCompiler(staticGenerator{payload: ambiguous})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent."}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("ambiguous definition provenance must remain strict, got %v", err)
	}
}

func TestCompilerLiftsUnambiguousCandidateResponseMetadata(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	agentCount := 1
	payload, _ := json.Marshal(GenerationResponse{
		Candidate: candidate, Commitments: PromptCommitments{AgentCount: &agentCount},
		Assumptions: []string{"The operator will review the proposal."},
		UnresolvedQuestions: []RefinementQuestion{{
			ID: "scope", Category: RefinementCategoryScope, Prompt: "Which scope?", WhyNeeded: "Execution must be bounded.",
			Blocking: []RefinementBlockingScope{RefinementBlocksApply}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerText},
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}}, Priority: 1,
		}},
	})
	var document map[string]interface{}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"commitments", "assumptions", "unresolvedQuestions"} {
		document["candidate"].(map[string]interface{})[field] = document[field]
		delete(document, field)
	}
	payload, _ = json.Marshal(document)

	result, err := decodeGenerationResponse(payload)
	if err != nil || result.Commitments.AgentCount == nil || *result.Commitments.AgentCount != 1 ||
		len(result.Assumptions) != 1 || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("lifted response metadata result=%#v err=%v", result, err)
	}

	document["unresolvedQuestions"] = document["candidate"].(map[string]interface{})["unresolvedQuestions"]
	ambiguous, _ := json.Marshal(document)
	if _, err := decodeGenerationResponse(ambiguous); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("ambiguous response metadata placement must remain strict, got %v", err)
	}
}

func TestCompilerNormalizesNonCredentialProvenanceValueAlias(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidateJSON, _ := json.Marshal(candidate)
	payload := []byte(`{"candidate":` + string(candidateJSON) + `,"unresolvedQuestions":[{"id":"scope","category":"scope","prompt":"Which channel is allowed?","whyNeeded":"A destination is required.","blocking":["apply"],"answer":{"kind":"text"},"provenance":[{"kind":"skill","value":"skill-slack"}],"priority":100}]}`)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Slack Agent."})
	if err != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("refinement provenance result=%#v err=%v", result, err)
	}
	provenance := result.UnresolvedQuestions[0].Provenance
	if len(provenance) != 1 || provenance[0].Kind != RefinementProvenanceSkill || provenance[0].Reference != "skill-slack" {
		t.Fatalf("normalized provenance=%#v", provenance)
	}

	credential := bytes.Replace(payload, []byte(`"kind":"skill"`), []byte(`"kind":"credential"`), 1)
	strict, _ := NewCompiler(staticGenerator{payload: credential})
	if _, err := strict.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent."}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("credential provenance value must remain strict, got %v", err)
	}
}

func TestCompilerDoesNotDiscardOptionsForOtherInvalidAnswerKinds(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	payload, _ := json.Marshal(GenerationResponse{
		Candidate: candidate,
		UnresolvedQuestions: []RefinementQuestion{{
			ID: "invalid-text", Category: RefinementCategoryOther, Prompt: "Explain?", WhyNeeded: "Required.",
			Blocking:   []RefinementBlockingScope{RefinementBlocksCandidate},
			Answer:     RefinementAnswerSchema{Kind: RefinementAnswerText, Options: []RefinementQuestionOption{{ID: "invented", Label: "Invented"}}},
			Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}}, Priority: 1,
		}},
	})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team"})
	var contractError *ContractGenerationError
	if result != nil || !errors.As(err, &contractError) || !strings.Contains(contractError.Diagnostic, "only select and Skill-selection answers may declare options") {
		t.Fatalf("result=%#v contractError=%#v err=%v", result, contractError, err)
	}
}

func TestCompilerPerformsOneDeterministicContractRepair(t *testing.T) {
	invalid := marketingCandidate("1", capability.RiskLevelRead)
	invalid.Assignments[0].RoleID = "invented-role"
	generated, _ := json.Marshal(GenerationResponse{
		Candidate:           invalid,
		UnresolvedQuestions: []RefinementQuestion{testRefinementQuestion("alternate-role", "Would you prefer another role?")},
	})
	repaired, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead), Assumptions: []string{"Used the declared researcher role."}})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
		}},
	})
	if err != nil || !result.Valid || generator.repairs != 1 || len(result.Validation) != 0 || len(result.UnresolvedQuestions) != 0 || len(result.Assumptions) != 1 {
		t.Fatalf("contract-repaired result = %#v, repairs = %d, err = %v", result, generator.repairs, err)
	}
}

func TestCompilerRepairsAgentAuthorityToSelectedActionRisk(t *testing.T) {
	underAuthorized := marketingCandidate("1", capability.RiskLevelWrite)
	generated, _ := json.Marshal(GenerationResponse{Candidate: underAuthorized})
	corrected := marketingCandidate("1", capability.RiskLevelExternal)
	repaired, _ := json.Marshal(GenerationResponse{Candidate: corrected})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(t.Context(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a researcher that searches public communities.",
		Catalog: CapabilityCatalog{
			Skills: map[string]SkillCapability{
				"reddit-research": {
					ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"},
					ActionRisks: map[string]capability.RiskLevel{
						"read": capability.RiskLevelRead, "search": capability.RiskLevelExternal,
					},
					MaximumRisk: capability.RiskLevelDestructive,
				},
			},
			AuthorityConstraint: &AuthorityConstraint{
				ID: "tenant-authority", Version: "1", MaximumRisk: capability.RiskLevelDestructive,
				RequireApprovalAt: capability.RiskLevelWrite,
			},
		},
	})
	if err != nil || result == nil || !result.Valid || generator.repairs != 1 {
		t.Fatalf("action-risk repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	if result.Candidate.Agents[0].Authority.MaximumRisk != capability.RiskLevelExternal ||
		result.Candidate.Agents[0].Authority.RequireApprovalAt != capability.RiskLevelWrite {
		t.Fatalf("repaired authority = %#v", result.Candidate.Agents[0].Authority)
	}
	if diagnostic := generator.repairErrors[0].Error(); !strings.Contains(diagnostic, "skill_action_risk_exceeded") ||
		!strings.Contains(diagnostic, "action search requires external risk") {
		t.Fatalf("action-risk repair diagnostic = %q", diagnostic)
	}
}

func TestCompilerValidatesProjectBlueprintAndExactMonitorCapability(t *testing.T) {
	candidate := researchProjectCandidate()
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}},
	}, SourcePolicies: map[string]SourcePolicyCapability{
		"approved-communities": {Reference: "approved-communities", Sources: []SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5},
	}}
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a continuing market research project that runs every hour", Catalog: catalog})
	if err != nil || !result.Valid || len(result.Validation) != 0 || len(result.MissingRequirements) != 0 {
		t.Fatalf("valid Project compile = %#v, err = %v", result, err)
	}

	wrongOwner := researchProjectCandidate()
	wrongOwner.Project.SourceMonitors[0].ObjectiveRef = WorkforceObjectiveKey(ProjectOwnerAgent, "community-researcher", "collect")
	wrongOwnerPayload, _ := json.Marshal(GenerationResponse{Candidate: wrongOwner})
	compiler, _ = NewCompiler(staticGenerator{payload: wrongOwnerPayload})
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create it every hour", Catalog: catalog})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "source_monitor_owner_mismatch") {
		t.Fatalf("wrong monitor owner = %#v, err = %v", result, err)
	}

	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	catalog.Skills["community-source"] = SkillCapability{ID: "community-source", Version: "2.0.0", Actions: []string{"observe"}}
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create it every hour", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "version" || result.MissingRequirements[0].ID != "community-source@1.2.3" {
		t.Fatalf("monitor version mismatch = %#v, err = %v", result, err)
	}

	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	catalog.Skills["community-source"] = SkillCapability{ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}}
	delete(catalog.SourcePolicies, "approved-communities")
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create it every hour", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "source_policy" || result.MissingRequirements[0].ID != "approved-communities" {
		t.Fatalf("missing source policy = %#v, err = %v", result, err)
	}

	outOfScope := researchProjectCandidate()
	outOfScope.Agents[0].Runbook.Steps["observe"].Action.Arguments["url"] = literalActionValue("https://attacker.example/feed")
	outOfScopePayload, _ := json.Marshal(GenerationResponse{Candidate: outOfScope})
	compiler, _ = NewCompiler(staticGenerator{payload: outOfScopePayload})
	catalog.SourcePolicies["approved-communities"] = SourcePolicyCapability{Reference: "approved-communities", Sources: []SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5}
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create it every hour", Catalog: catalog})
	if err != nil || result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "source_scope" {
		t.Fatalf("out-of-policy monitor source = %#v, err = %v", result, err)
	}
}

func TestCompilerSurfacesExactCatalogOwnedSourcePolicyProposalWithoutGrantingAuthority(t *testing.T) {
	candidate := researchProjectCandidate()
	candidate.Project.SourceMonitors[0].SourcePolicyRef = "approved-communities@2026-07-22"
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	generator := &repairingGenerator{generated: payload, repaired: payload}
	compiler, _ := NewCompiler(generator)
	policy := source.Policy{
		ID: "approved-communities", Version: "2026-07-22", Enabled: true,
		Sources:      []source.PolicySource{{Host: "community.example", PathPrefixes: []string{"/feed"}, Methods: []string{"GET"}}},
		MaximumItems: 5, RetentionDays: 30, ApprovalPolicy: "source-policy-admin",
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a continuing market research project that runs every hour",
		Catalog: CapabilityCatalog{
			Skills: map[string]SkillCapability{"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}}},
			CapabilityNeeds: []CapabilityNeed{{
				ID: "community-research", Prompt: "Which research capability?", WhyNeeded: "A verified capability is required.",
				SkillIDs: []string{"community-source"}, Priority: 100,
				SourcePolicyProposal: &CapabilitySourcePolicyProposal{Policy: policy, Reason: "Permit bounded read-only collection from the reviewed community feed.", SkillIDs: []string{"community-source"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.MissingRequirements) != 1 || result.MissingRequirements[0].Kind != "source_policy" {
		t.Fatalf("missing authority = %#v", result.MissingRequirements)
	}
	if len(result.SourcePolicyProposals) != 1 {
		t.Fatalf("proposals = %#v", result.SourcePolicyProposals)
	}
	if generator.repairs != 0 {
		t.Fatalf("reviewable policy proposal unexpectedly triggered %d provider repairs", generator.repairs)
	}
	proposal := result.SourcePolicyProposals[0]
	if proposal.Reference != "approved-communities@2026-07-22" || proposal.Policy.ID != policy.ID ||
		!proposal.RequiresApproval || len(proposal.RequiredBy) != 1 {
		t.Fatalf("proposal = %#v", proposal)
	}

	// Once an authorized host activates the exact immutable version, a fresh
	// catalog projection resolves the requirement and no longer emits a draft.
	active := CapabilityCatalog{
		Skills: map[string]SkillCapability{"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}}},
		SourcePolicies: map[string]SourcePolicyCapability{
			"approved-communities@2026-07-22": {Reference: "approved-communities@2026-07-22", Sources: []SourcePolicySourceCapability{{Host: "community.example", PathPrefixes: []string{"/feed"}}}, MaximumItems: 5},
		},
	}
	result, err = compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create it every hour", Catalog: active})
	if err != nil || !result.Valid || len(result.SourcePolicyProposals) != 0 {
		t.Fatalf("active authority result = %#v, err = %v", result, err)
	}
}

func TestCompilerRejectsGovernedSourceActionWithoutProjectMonitor(t *testing.T) {
	candidate := researchProjectCandidate()
	candidate.Project = nil
	action := candidate.Agents[0].Runbook.Steps["observe"].Action
	action.SkillID, action.SkillVersion, action.Action = source.SkillID, source.SkillVersion, source.ObserveFeed
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Monitor a public feed every hour", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
		source.SkillID: {ID: source.SkillID, Version: source.SkillVersion, Actions: []string{source.ObserveFeed}},
	}}})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "source_action_requires_monitor") {
		t.Fatalf("unprojected source action result = %#v, err = %v", result, err)
	}
}

func TestCompilerMaterializesCatalogOwnedSourceMonitorWithoutGrantingAuthority(t *testing.T) {
	candidate := researchProjectCandidate()
	candidate.Project.SourceMonitors = nil
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	policy := source.Policy{
		ID: "approved-communities", Version: "proposal-v1", Enabled: true,
		Sources:      []source.PolicySource{{Host: "community.example", PathPrefixes: []string{"/feed"}, Methods: []string{"GET"}}},
		MaximumItems: 5, RetentionDays: 30, ApprovalPolicy: "source-policy-admin",
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor community.example every hour", Catalog: CapabilityCatalog{
			Skills: map[string]SkillCapability{"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}}},
			CapabilityNeeds: []CapabilityNeed{{
				ID: "community-research", Prompt: "Which source?", WhyNeeded: "Source access must be explicit.", SkillIDs: []string{"community-source"},
				SourceScope:          &CapabilitySourceScopeRequirement{Prompt: "Which community?", WhyNeeded: "Scope must be bounded.", Minimum: 1, Maximum: 1, Priority: 90, MaterializationInputKeys: []string{"url"}},
				SourcePolicyProposal: &CapabilitySourcePolicyProposal{Policy: policy, Reason: "Permit reviewed collection.", SkillIDs: []string{"community-source"}}, Priority: 100,
			}},
		}, Refinement: &RefinementContext{Answers: []RefinementResolvedAnswer{
			{QuestionID: CapabilityNeedQuestionID("community-research"), Value: RefinementProviderAnswerValue{SkillIDs: []string{"community-source"}}, Source: RefinementAnswerSourceUser},
			{QuestionID: CapabilitySourceScopeQuestionID("community-research"), Value: RefinementProviderAnswerValue{Items: []string{"community.example"}}, Source: RefinementAnswerSourceUser},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidate.Project.SourceMonitors) != 1 || result.Candidate.Project.SourceMonitors[0].SourcePolicyRef != "approved-communities@proposal-v1" ||
		len(result.SourcePolicyProposals) != 1 || result.SourcePolicyProposals[0].Reference != "approved-communities@proposal-v1" || result.Valid {
		t.Fatalf("deterministic inert source monitor = %#v", result)
	}
	if hasValidationCode(result.Validation, "source_action_requires_monitor") {
		t.Fatalf("materialized monitor was not recognized: %#v", result.Validation)
	}
}

func TestCompilerMaterializesExactRequiredSkillAuthorityDefault(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{
		{SkillID: "reddit-research", VersionConstraint: "1.0.0", RequiredActions: []string{"read"}},
		{SkillID: "optional-export", VersionConstraint: "1.0.0", Optional: true},
	}
	candidate.Agents[0].Authority.AllowedSkillIDs = nil
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
			"optional-export": {ID: "optional-export", Version: "1.0.0"},
		}},
	})
	if err != nil || !result.Valid || len(result.Candidate.Agents[0].Authority.AllowedSkillIDs) != 1 ||
		result.Candidate.Agents[0].Authority.AllowedSkillIDs[0] != "reddit-research" {
		t.Fatalf("required Skill authority default = %#v, err = %v", result, err)
	}

	candidate.Agents[0].Authority.AllowedSkillIDs = []string{}
	payload, _ = json.Marshal(GenerationResponse{Candidate: candidate})
	payload = bytes.Replace(payload, []byte(`"authority":{`), []byte(`"authority":{"allowedSkillIds":[],`), 1)
	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	result, err = compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
			"optional-export": {ID: "optional-export", Version: "1.0.0"},
		}},
	})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "required_skill_not_authorized") {
		t.Fatalf("explicitly restricted required Skill = %#v, err = %v", result, err)
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

func researchProjectCandidate() WorkforceCandidate {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	agentDefinition := candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: "community-source", VersionConstraint: "1.2.3", RequiredActions: []string{"observe"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{"community-source"}
	agentDefinition.ObjectiveTemplates = []workforce.ObjectiveTemplate{{ID: "collect", Title: "Collect evidence", Goal: "Collect permitted community evidence", Priority: 1}}
	candidate.Team.ObjectiveTemplates = []workforce.ObjectiveTemplate{
		{ID: "monitor", Title: "Monitor communities", Goal: "Monitor approved sources over time", Priority: 1},
		{ID: "report", Title: "Publish report", Goal: "Synthesize a cited report", Priority: 2},
	}
	agentDefinition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "community-monitor", Version: "1.0.0", Name: "Community monitor",
		Entrypoints: map[string]string{"monitor": "observe"},
		Triggers: map[string]runbook.Trigger{"hourly": {
			Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 */1 * * *", Timezone: "UTC"}, Entrypoint: "monitor",
			ObjectiveID: WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "monitor"), MaximumConcurrent: 1,
			Budget: &runbook.BudgetAllocation{MaxTurns: 2, MaxActions: 1, MaxDurationMS: 60000},
		}},
		Steps: map[string]runbook.Step{
			"observe": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe",
				Arguments:  map[string]runbook.Value{"url": literalActionValue("https://community.example/feed"), "maxItems": literalActionValue(5)},
				ResultPath: "/results/observe", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	candidate.Project = &ProjectBlueprint{
		ID: "market-intelligence", Title: "Market intelligence", Purpose: "Continuously understand user pain points",
		Owner: ProjectOwnerReference{Type: ProjectOwnerTeam, DefinitionID: candidate.Team.ID},
		ObjectiveRefs: []string{
			WorkforceObjectiveKey(ProjectOwnerAgent, agentDefinition.ID, "collect"),
			WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "monitor"),
			WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "report"),
		},
		Milestones: []ProjectMilestoneBlueprint{{ID: "baseline", Title: "Establish baseline", ObjectiveRefs: []string{WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "monitor")}}},
		Hypotheses: []ProjectHypothesisBlueprint{{ID: "setup-friction", Statement: "Setup friction is a leading adoption barrier", Confidence: 0.5}},
		SourceMonitors: []ProjectSourceMonitorBlueprint{{
			ID: "community-listening", ObjectiveRef: WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "monitor"), AssignedAgentDefinitionID: agentDefinition.ID,
			SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe", SourcePolicyRef: "approved-communities", Deduplication: ProjectDeduplicateStableSourceAndContent,
		}},
		Deliverables: []ProjectDeliverableBlueprint{{ID: "monthly-report", Title: "Monthly cited report", ObjectiveRefs: []string{WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, "report")}}},
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
		Coordination: team.CoordinationPolicy{MaximumSpeakersPerRound: 2, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:   team.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 2, AllowPeerDelegation: true, RequireAcceptance: true},
		Approvals:    team.ApprovalPolicy{MaximumRisk: risk},
	}
	return WorkforceCandidate{
		Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition,
		Assignments: []Assignment{{ID: "researcher", RoleID: "researcher", AgentDefinitionID: agentDefinition.ID, DisplayName: agentDefinition.DisplayName}},
	}
}

func TestTeamRoleSkillGrantsUseCatalogIdentityAndRejectModelRuntimeIdentity(t *testing.T) {
	candidate := WorkforceCandidate{Team: &team.Definition{
		ID: "research", Version: "1", DisplayName: "Research", Purpose: "Summarize evidence",
		Roles: []team.RoleSlot{{
			ID: "analyst", DisplayName: "Analyst", Purpose: "Analyze", ChannelParticipation: team.RoleChannelActive,
			SkillGrants: []team.RoleSkillGrant{{SkillID: "clawhub-listing", SkillVersion: "1.0.0", AllowedActions: []string{"execute"}, MaximumRisk: capability.RiskLevelRead}},
		}},
		Coordination: team.CoordinationPolicy{}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"clawhub-listing": {ID: "clawhub-listing", Version: "1.0.0", Actions: []string{"execute"}, MaximumRisk: capability.RiskLevelRead},
	}}
	if missing := missingRequirements(&candidate, catalog); len(missing) != 0 {
		t.Fatalf("truthful role grant missing=%#v", missing)
	}
	candidate.Team.Roles[0].SkillGrants[0].EnablePrompt = true
	if missing := missingRequirements(&candidate, catalog); len(missing) != 1 || missing[0].Kind != "prompt" || missing[0].ID != "clawhub-listing" {
		t.Fatalf("unsupported role prompt missing=%#v", missing)
	}
	capabilityWithPrompt := catalog.Skills["clawhub-listing"]
	capabilityWithPrompt.PromptAvailable = true
	catalog.Skills["clawhub-listing"] = capabilityWithPrompt
	if missing := missingRequirements(&candidate, catalog); len(missing) != 0 {
		t.Fatalf("supported role prompt missing=%#v", missing)
	}
	candidate.Team.Roles[0].SkillGrants[0].AllowedActions = []string{"publish"}
	if missing := missingRequirements(&candidate, catalog); len(missing) != 1 || missing[0].Kind != "action" {
		t.Fatalf("unsupported role action missing=%#v", missing)
	}
	candidate.Team.Roles[0].SkillGrants[0].AllowedActions = []string{"execute"}
	identity := capability.NewSkillIdentity("summarize", "1.0.0+source.0123456789ab", "clawhub::@alice/summarize")
	candidate.Team.Roles[0].SkillGrants[0].RuntimeIdentity = &identity
	issues := validateCandidate(&candidate, nil)
	found := false
	for _, issue := range issues {
		found = found || issue.Code == "server_owned"
	}
	if !found {
		t.Fatalf("model-supplied runtime identity was accepted: %#v", issues)
	}
}
