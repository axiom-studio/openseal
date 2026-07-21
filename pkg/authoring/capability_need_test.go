package authoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func capabilityNeedCandidate() WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "researcher", Version: "1.0.0", DisplayName: "Researcher",
		Purpose: "Research permitted sources", SystemPrompt: "Research permitted sources with evidence.",
		Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}}}
}

func capabilityNeedCatalog(choiceRequired bool, skillIDs ...string) CapabilityCatalog {
	return CapabilityCatalog{
		Skills: map[string]SkillCapability{
			"openseal.source": {
				ID: "openseal.source", Version: "1.0.0", Name: "RSS source observer",
				Description: "Read permitted RSS feeds without an API credential.", Actions: []string{"observe_feed"}, Readiness: SkillReadinessReady,
			},
			"reddit-post-search": {
				ID: "reddit-post-search", Version: "2.1.0", SourceIdentity: "https://skills.example::reddit-post-search",
				Name: "Reddit post search", Description: "Search Reddit through its API.", Actions: []string{"search"},
				CredentialKinds: []string{"reddit-oauth"}, Readiness: SkillReadinessNeedsInstallation,
				Compatibility: []SkillCompatibility{
					{Requirement: "installation", Compatible: false, Evidence: "Verified artifact is not installed", Reference: "needs_installation"},
					{Requirement: "source_digest", Compatible: true, Evidence: "Verified compilation receipt", Reference: "sha256:verified"},
				},
			},
		},
		CapabilityNeeds: []CapabilityNeed{{
			ID: "reddit-access", Prompt: "How should Reddit be accessed?",
			WhyNeeded: "More than one verified Skill can provide the requested source access.",
			SkillIDs:  skillIDs, ChoiceRequired: choiceRequired, Priority: 900,
		}},
	}
}

func capabilityNeedPayload(t *testing.T) []byte {
	t.Helper()
	scope := RefinementQuestion{
		ID: "subreddits", Category: RefinementCategoryScope, Prompt: "Which subreddits should be monitored?",
		WhyNeeded: "The permitted source scope must be explicit.", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerStringList, Minimum: 1, Maximum: 20},
		Priority: 1000, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
	}
	payload, err := json.Marshal(GenerationResponse{Candidate: capabilityNeedCandidate(), UnresolvedQuestions: []RefinementQuestion{scope}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestCompilerSynthesizesVerifiedSkillChoiceBeforeProviderScope(t *testing.T) {
	catalog := capabilityNeedCatalog(false, "openseal.source", "reddit-post-search")
	compiler, err := NewCompiler(staticGenerator{payload: capabilityNeedPayload(t)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor permitted Reddit communities", Catalog: catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UnresolvedQuestions) != 2 {
		t.Fatalf("questions = %#v", result.UnresolvedQuestions)
	}
	choice, scope := result.UnresolvedQuestions[0], result.UnresolvedQuestions[1]
	if choice.ID != CapabilityNeedQuestionID("reddit-access") || choice.Category != RefinementCategorySkill ||
		choice.Answer.Kind != RefinementAnswerSkillSelection || choice.Answer.Minimum != 1 || choice.Answer.Maximum != 1 ||
		len(choice.Answer.Options) != 2 || choice.Answer.Options[0].ID != "openseal.source" || choice.Answer.Options[1].ID != "reddit-post-search" ||
		choice.Priority != 900 || len(choice.Provenance) != 1 || choice.Provenance[0].Kind != RefinementProvenanceCatalog || choice.Provenance[0].Reference != "reddit-access" {
		t.Fatalf("deterministic Skill choice = %#v", choice)
	}
	if len(scope.DependsOn) != 1 || scope.DependsOn[0].QuestionID != choice.ID {
		t.Fatalf("scope dependency = %#v", scope.DependsOn)
	}
	refinement := ChangeSetRefinement{Questions: result.UnresolvedQuestions}
	if next := refinement.NextQuestion(); next == nil || next.ID != choice.ID {
		t.Fatalf("first sequential question = %#v", next)
	}
}

func TestCapabilityNeedAnswerSurvivesRecompileAndUnlocksScope(t *testing.T) {
	catalog := capabilityNeedCatalog(false, "openseal.source", "reddit-post-search")
	compiler, _ := NewCompiler(staticGenerator{payload: capabilityNeedPayload(t)})
	first, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Monitor Reddit", Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	choiceID := CapabilityNeedQuestionID("reddit-access")
	current := ChangeSetRefinement{
		Questions: first.UnresolvedQuestions,
		Answers:   []RefinementAnswerEvent{{QuestionID: choiceID, Value: RefinementAnswerValue{SkillIDs: []string{"openseal.source"}}}},
	}
	repeatedPayload, err := json.Marshal(GenerationResponse{
		Candidate: capabilityNeedCandidate(), UnresolvedQuestions: []RefinementQuestion{first.UnresolvedQuestions[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ = NewCompiler(staticGenerator{payload: repeatedPayload})
	second, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor Reddit", Catalog: catalog, Refinement: providerRefinementContext(current),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.UnresolvedQuestions) != 1 || second.UnresolvedQuestions[0].ID != "subreddits" || len(second.UnresolvedQuestions[0].DependsOn) != 0 {
		t.Fatalf("post-choice questions = %#v", second.UnresolvedQuestions)
	}
	reconciled := reconcileRefinement(current, second)
	if reconciled.CurrentAnswer(choiceID) == nil {
		t.Fatal("audited Skill choice was lost during reconciliation")
	}
	if next := reconciled.NextQuestion(); next == nil || next.ID != "subreddits" {
		t.Fatalf("next question after RSS choice = %#v", next)
	}
}

func TestCapabilityNeedOnlyAsksForOneChoiceWhenHostRequiresIt(t *testing.T) {
	payload := capabilityNeedPayload(t)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	implicit, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor RSS", Catalog: capabilityNeedCatalog(false, "openseal.source"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(implicit.UnresolvedQuestions) != 1 || implicit.UnresolvedQuestions[0].ID != "subreddits" {
		t.Fatalf("single unambiguous Skill produced a choice = %#v", implicit.UnresolvedQuestions)
	}
	explicit, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Let me choose the source", Catalog: capabilityNeedCatalog(true, "openseal.source"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.UnresolvedQuestions) != 2 || explicit.UnresolvedQuestions[0].ID != CapabilityNeedQuestionID("reddit-access") {
		t.Fatalf("explicit server choice = %#v", explicit.UnresolvedQuestions)
	}
}

func TestCapabilityNeedReplacesMalformedProviderSkillQuestionBeforeValidation(t *testing.T) {
	malformed := RefinementQuestion{
		ID: "provider-sentiment-install", Category: RefinementCategorySkill,
		Prompt: "Install sentiment?", WhyNeeded: "Provider inferred a Skill choice.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerSingleSelect, Options: []RefinementQuestionOption{{ID: "reddit-post-search", Label: "Search"}}},
		Priority: 1000, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	payload, err := json.Marshal(GenerationResponse{
		Candidate: capabilityNeedCandidate(), UnresolvedQuestions: []RefinementQuestion{malformed},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor Reddit", Catalog: capabilityNeedCatalog(false, "openseal.source", "reddit-post-search"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UnresolvedQuestions) != 1 || result.UnresolvedQuestions[0].ID != CapabilityNeedQuestionID("reddit-access") ||
		result.UnresolvedQuestions[0].Answer.Kind != RefinementAnswerSkillSelection {
		t.Fatalf("server-owned replacement = %#v", result.UnresolvedQuestions)
	}
}

func TestCompilerSynthesizesSourceScopeAfterSkillAndCredentialPrerequisites(t *testing.T) {
	catalog := capabilityNeedCatalog(false, "openseal.source", "reddit-post-search")
	catalog.CapabilityNeeds[0].SourceScope = &CapabilitySourceScopeRequirement{
		Prompt: "Which subreddits should be monitored?", WhyNeeded: "Monitoring targets must be explicit.",
		Minimum: 1, Maximum: 20, Priority: 950, RequireSourceMonitor: true,
	}
	credential := RefinementQuestion{
		ID: "reddit-credential", Category: RefinementCategoryCredential,
		Prompt: "Which Reddit credential should be used?", WhyNeeded: "API access requires an authorized credential.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference},
		Priority: 1000, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}},
	}
	payload, err := json.Marshal(GenerationResponse{Candidate: capabilityNeedCandidate(), UnresolvedQuestions: []RefinementQuestion{credential}})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Monitor Reddit", Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UnresolvedQuestions) != 3 {
		t.Fatalf("questions = %#v", result.UnresolvedQuestions)
	}
	choice, credential, scope := result.UnresolvedQuestions[0], result.UnresolvedQuestions[1], result.UnresolvedQuestions[2]
	if next := (ChangeSetRefinement{Questions: result.UnresolvedQuestions}).NextQuestion(); next == nil || next.ID != choice.ID {
		t.Fatalf("first question = %#v", next)
	}
	if len(credential.DependsOn) != 1 || credential.DependsOn[0].QuestionID != choice.ID {
		t.Fatalf("credential dependencies = %#v", credential.DependsOn)
	}
	if scope.ID != CapabilitySourceScopeQuestionID("reddit-access") || scope.Category != RefinementCategoryScope ||
		scope.Answer.Kind != RefinementAnswerStringList || scope.Answer.Minimum != 1 || scope.Answer.Maximum != 20 ||
		len(scope.DependsOn) != 2 || scope.DependsOn[0].QuestionID != choice.ID || scope.DependsOn[1].QuestionID != credential.ID {
		t.Fatalf("deterministic source scope = %#v", scope)
	}
}

func TestAnsweredSourceScopeCannotProduceCandidateWithoutMonitor(t *testing.T) {
	catalog := capabilityNeedCatalog(false, "openseal.source")
	catalog.CapabilityNeeds[0].SourceScope = &CapabilitySourceScopeRequirement{
		Prompt: "Which subreddits should be monitored?", WhyNeeded: "Monitoring targets must be explicit.",
		Minimum: 1, Maximum: 20, Priority: 950, RequireSourceMonitor: true,
	}
	payload, err := json.Marshal(GenerationResponse{Candidate: capabilityNeedCandidate()})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	request := GenerateRequest{
		Mode: ModeAmend, Prompt: "Monitor Reddit", Existing: ptrWorkforceCandidate(capabilityNeedCandidate()), Catalog: catalog,
		Refinement: &RefinementContext{Answers: []RefinementResolvedAnswer{{
			QuestionID: CapabilitySourceScopeQuestionID("reddit-access"),
			Value:      RefinementProviderAnswerValue{Items: []string{"openclaw", "selfhosted"}},
			Source:     RefinementAnswerSourceUser,
		}}},
	}
	result, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UnresolvedQuestions) != 0 {
		t.Fatalf("answered scope was asked again: %#v", result.UnresolvedQuestions)
	}
	found := false
	for _, issue := range result.Validation {
		found = found || issue.Code == "source_scope_not_materialized" && issue.Path == "initiative.sourceMonitors"
	}
	if !found || result.Valid {
		t.Fatalf("missing source monitor did not fail closed: valid=%v validation=%#v", result.Valid, result.Validation)
	}
}

func ptrWorkforceCandidate(value WorkforceCandidate) *WorkforceCandidate { return &value }
