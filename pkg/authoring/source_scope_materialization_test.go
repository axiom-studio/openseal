package authoring

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func sourceScopeCatalog() CapabilityCatalog {
	return CapabilityCatalog{
		Skills: map[string]SkillCapability{
			"reddit-search": {ID: "reddit-search", Version: "2.0.0", Actions: []string{"search"}, Readiness: SkillReadinessReady},
		},
		CapabilityNeeds: []CapabilityNeed{{
			ID: "reddit-access", Prompt: "How should Reddit be accessed?", WhyNeeded: "A verified source action is required.",
			SkillIDs: []string{"reddit-search"}, Priority: 900,
			SourceScope: &CapabilitySourceScopeRequirement{
				Prompt: "Which subreddits should be monitored?", WhyNeeded: "The source boundary must be explicit.",
				Minimum: 1, Maximum: 20, Priority: 950, MaterializationInputKeys: []string{"subreddit", "subreddits", "query"},
			},
		}},
	}
}

func sourceScopeAgent(version string, objectives ...workforce.ObjectiveTemplate) *agent.AgentDefinition {
	return &agent.AgentDefinition{
		ID: "researcher", Version: version, DisplayName: "Researcher", Purpose: "Monitor permitted communities",
		SystemPrompt:       "Monitor only explicitly permitted communities and retain evidence.",
		SkillRequirements:  []agent.SkillRequirement{{SkillID: "reddit-search", VersionConstraint: "2.0.0", RequiredActions: []string{"search"}}},
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: objectives,
	}
}

func sourceScopeCadenceObjective(id string, inputs map[string]interface{}) workforce.ObjectiveTemplate {
	return workforce.ObjectiveTemplate{
		ID: id, Title: "Monitor Reddit", Goal: "Collect relevant posts from permitted communities", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "interval", "intervalSeconds": float64(300),
			"runTemplate": map[string]interface{}{"capability": map[string]interface{}{
				"skillId": "reddit-search", "skillVersion": "2.0.0", "action": "search", "inputs": inputs,
			}},
		},
	}
}

func sourceScopeRequest(existing *WorkforceCandidate) GenerateRequest {
	return GenerateRequest{
		Mode: ModeAmend, Prompt: "Monitor Reddit every 5 minutes", Existing: existing, Catalog: sourceScopeCatalog(),
		Refinement: &RefinementContext{Answers: []RefinementResolvedAnswer{{
			QuestionID: CapabilitySourceScopeQuestionID("reddit-access"),
			Value:      RefinementProviderAnswerValue{Items: []string{"openclaw"}},
			Source:     RefinementAnswerSourceUser,
		}}},
	}
}

func TestCompilerMaterializesAnsweredSourceScopeAndPreservesDroppedObjective(t *testing.T) {
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", sourceScopeCadenceObjective("monitor", map[string]interface{}{"limit": float64(25)}))}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0")}}}
	payload, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	request := sourceScopeRequest(&existing)

	first, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid || !second.Valid || !reflect.DeepEqual(first.Candidate, second.Candidate) {
		t.Fatalf("regeneration is not deterministic: first=%#v second=%#v", first, second)
	}
	objectives := first.Candidate.Agents[0].ObjectiveTemplates
	if len(objectives) != 1 || objectives[0].ID != "monitor" {
		t.Fatalf("existing objective was not preserved: %#v", objectives)
	}
	invocation := objectives[0].Cadence["runTemplate"].(map[string]interface{})["capability"].(map[string]interface{})
	inputs := invocation["inputs"].(map[string]interface{})
	if inputs["subreddit"] != "openclaw" || inputs["limit"] != float64(25) {
		t.Fatalf("materialized inputs = %#v", inputs)
	}
	for _, issue := range first.Validation {
		if issue.Code == "source_scope_not_materialized" {
			t.Fatalf("valid answer remained blocked: %#v", first.Validation)
		}
	}
}

func TestMaterializesSourceScopeThroughScheduledRunbookEntrypoint(t *testing.T) {
	catalog := sourceScopeCatalog()
	catalog.CapabilityNeeds[0].SourceScope.Targets = []string{"r/openclaw", "r/selfhosted"}
	definition := sourceScopeAgent("1.0.0", workforce.ObjectiveTemplate{
		ID: "monitor", Title: "Monitor Reddit", Goal: "Review permitted communities", Priority: 1,
		Cadence: map[string]interface{}{
			"type": "daily", "timeOfDay": "00:00", "timezone": "UTC", "assignedAgentId": "researcher",
			"runTemplate": map[string]interface{}{"entrypoint": "review"},
		},
	})
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "review", Version: "1.0.0", Name: "Review",
		Entrypoints: map[string]string{"review": "search"},
		Interfaces: map[string]runbook.Interface{"review": {
			Description: "Review configured communities.",
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}, "required": []interface{}{}},
		}},
		Steps: map[string]runbook.Step{
			"search": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reddit-search", SkillVersion: "2.0.0", Action: "search", ResultPath: "/results/search", Next: "review"}},
			"review": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{AgentID: literalActionValue("researcher"), Goal: literalActionValue("Review the bounded source results."), ResultPath: "/results/review", Next: "done"}},
			"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}}
	request := GenerateRequest{Mode: ModeCreate, Prompt: "Monitor both communities daily", Catalog: catalog}
	if issues := materializeAnsweredCapabilitySourceScopes(&candidate, request); len(issues) != 0 {
		t.Fatalf("Runbook source materialization issues = %#v", issues)
	}

	template := definition.ObjectiveTemplates[0].Cadence["runTemplate"].(map[string]interface{})
	contextValues := template["context"].(map[string]interface{})
	if !reflect.DeepEqual(contextValues["subreddits"], []string{"r/openclaw", "r/selfhosted"}) {
		t.Fatalf("Runbook schedule source context = %#v", contextValues)
	}
	contract := definition.Runbook.Interfaces["review"]
	if !stringSet(schemaStringList(contract.InputSchema["required"]))["subreddits"] {
		t.Fatalf("Runbook source interface = %#v", contract.InputSchema)
	}
	if reference := definition.Runbook.Steps["review"].Delegate.Context["subreddits"].Ref; reference != "/input/subreddits" {
		t.Fatalf("delegate source reference = %q", reference)
	}
	if !capabilitySourceScopeMaterialized(&candidate, catalog.CapabilityNeeds[0], catalog.CapabilityNeeds[0].SourceScope.Targets, catalog.CapabilityNeeds[0].SourceScope.MaterializationInputKeys, catalog) {
		t.Fatal("Runbook source scope was not recognized as durable")
	}
}

func TestCompilerMaterializesHostExtractedSourceScopeWithoutAskingAgain(t *testing.T) {
	catalog := sourceScopeCatalog()
	catalog.CapabilityNeeds[0].SourceScope.Targets = []string{"r/vibecoding"}
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{
		sourceScopeAgent("1.0.0", sourceScopeCadenceObjective("monitor", map[string]interface{}{"limit": float64(1)})),
	}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{
		sourceScopeAgent("1.1.0"),
	}}}
	payload, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeAmend, Prompt: "Monitor r/vibecoding", Existing: &existing, Catalog: catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.UnresolvedQuestions) != 0 {
		t.Fatalf("host-extracted scope remained unresolved: valid=%v questions=%#v validation=%#v", result.Valid, result.UnresolvedQuestions, result.Validation)
	}
	invocations := candidateObjectiveCapabilityInvocations(&result.Candidate)
	if len(invocations) != 1 {
		t.Fatalf("materialized invocations = %#v candidate=%#v", invocations, result.Candidate)
	}
	inputs := invocations[0].invocation["inputs"].(map[string]interface{})
	if inputs["subreddit"] != "r/vibecoding" {
		t.Fatalf("materialized prompt scope = %#v", inputs)
	}
}

func TestCompilerMaterializesAnsweredSourceScopeIntoEventRule(t *testing.T) {
	objective := workforce.ObjectiveTemplate{
		ID: "monitor-events", Title: "Monitor events", Goal: "Search after a permitted wake", Priority: 1,
		EventRules: map[string]interface{}{"version": "1", "rules": []interface{}{map[string]interface{}{
			"id": "wake", "eventType": "source.poll", "runTemplate": map[string]interface{}{"capability": map[string]interface{}{
				"skillId": "reddit-search", "skillVersion": "2.0.0", "action": "search", "inputs": map[string]interface{}{},
			}},
		}}},
	}
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", objective)}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0", objective)}}}
	payload, _ := json.Marshal(generated)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	request := sourceScopeRequest(&existing)
	request.Prompt = "Handle permitted Reddit source events"
	result, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	rules := result.Candidate.Agents[0].ObjectiveTemplates[0].EventRules["rules"].([]interface{})
	invocation := rules[0].(map[string]interface{})["runTemplate"].(map[string]interface{})["capability"].(map[string]interface{})
	if got := invocation["inputs"].(map[string]interface{})["subreddit"]; got != "openclaw" || !result.Valid {
		t.Fatalf("event materialization = %#v, valid=%v, issues=%#v", got, result.Valid, result.Validation)
	}
}

func TestCompilerBlocksAmbiguousSourceScopeMaterialization(t *testing.T) {
	objectives := []workforce.ObjectiveTemplate{
		sourceScopeCadenceObjective("primary", map[string]interface{}{}),
		sourceScopeCadenceObjective("secondary", map[string]interface{}{}),
	}
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", objectives...)}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0", objectives...)}}}
	payload, _ := json.Marshal(generated)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), sourceScopeRequest(&existing))
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || !hasValidationCode(result.Validation, "source_scope_action_ambiguous") {
		t.Fatalf("ambiguous actions did not stay blocked: valid=%v issues=%#v", result.Valid, result.Validation)
	}
}

func TestCompilerBlocksSourceScopeWithoutMatchingAction(t *testing.T) {
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0")}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0")}}}
	payload, _ := json.Marshal(generated)
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), sourceScopeRequest(&existing))
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || !hasValidationCode(result.Validation, "source_scope_action_not_found") {
		t.Fatalf("missing action did not stay blocked: valid=%v issues=%#v", result.Valid, result.Validation)
	}
}

func TestChangeSetPersistsMaterializedSourceScopeAcrossRegeneration(t *testing.T) {
	initialCandidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", sourceScopeCadenceObjective("monitor", map[string]interface{}{"limit": float64(25)}))}}
	regeneratedCandidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0")}}
	initialPayload, _ := json.Marshal(GenerationResponse{Candidate: initialCandidate})
	regeneratedPayload, _ := json.Marshal(GenerationResponse{Candidate: regeneratedCandidate})
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{initialPayload, regeneratedPayload}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}

	created, replay, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Monitor Reddit every 5 minutes", Catalog: sourceScopeCatalog(),
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-source-monitor",
	})
	if err != nil || replay || created.Status != ChangeSetBlocked || created.Refinement.NextQuestion() == nil ||
		created.Refinement.NextQuestion().ID != CapabilitySourceScopeQuestionID("reddit-access") {
		t.Fatalf("created=%#v replay=%v err=%v", created, replay, err)
	}
	answered, replay, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision,
		QuestionID: CapabilitySourceScopeQuestionID("reddit-access"), Value: RefinementAnswerValue{Items: []string{"openclaw"}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "answer-source-scope",
	})
	if err != nil || replay || answered.Status != ChangeSetEvaluating {
		t.Fatalf("answered=%#v replay=%v err=%v", answered, replay, err)
	}
	completed, err := service.GeneratePrepared(context.Background(), scope, created.ID, answered.Revision)
	if err != nil || completed.Status != ChangeSetReview || !completed.Result.Valid {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	persisted, err := service.Get(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	objective := persisted.Result.Candidate.Agents[0].ObjectiveTemplates[0]
	inputs := objective.Cadence["runTemplate"].(map[string]interface{})["capability"].(map[string]interface{})["inputs"].(map[string]interface{})
	if inputs["subreddit"] != "openclaw" || inputs["limit"] != float64(25) || persisted.CandidateDigest == created.CandidateDigest {
		t.Fatalf("persisted source materialization = %#v digest=%s", inputs, persisted.CandidateDigest)
	}
}
