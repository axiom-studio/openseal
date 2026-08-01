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
		Skills: map[string]SkillCapability{"reddit-search": {ID: "reddit-search", Version: "2.0.0", Actions: []string{"search"}, Readiness: SkillReadinessReady}},
		CapabilityNeeds: []CapabilityNeed{{
			ID: "reddit-access", Prompt: "How should Reddit be accessed?", WhyNeeded: "A verified source action is required.", SkillIDs: []string{"reddit-search"}, Priority: 900,
			SourceScope: &CapabilitySourceScopeRequirement{Prompt: "Which subreddits should be monitored?", WhyNeeded: "The source boundary must be explicit.", Minimum: 1, Maximum: 20, Priority: 950, MaterializationInputKeys: []string{"subreddit", "subreddits"}},
		}},
	}
}

func sourceScopeAgent(version string, withAction bool) *agent.AgentDefinition {
	definition := &agent.AgentDefinition{
		ID: "researcher", Version: version, DisplayName: "Researcher", Purpose: "Monitor permitted communities.", SystemPrompt: "Use only explicitly permitted communities.",
		SkillRequirements:  []agent.SkillRequirement{{SkillID: "reddit-search", VersionConstraint: "2.0.0", RequiredActions: []string{"search"}}},
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1, AllowedSkillIDs: []string{"reddit-search"}},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "monitor", Title: "Monitor Reddit", Goal: "Collect relevant posts from permitted communities", Priority: 1}},
	}
	if !withAction {
		return definition
	}
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "monitor", Version: version, Name: "Monitor Reddit",
		Entrypoints: map[string]string{"monitor": "search"},
		Triggers: map[string]runbook.Trigger{"every-five-minutes": {
			Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 */5 * * * *", Timezone: "UTC"},
			Entrypoint: "monitor", ObjectiveID: WorkforceObjectiveKey("agent", "researcher", "monitor"), MaximumConcurrent: 1,
		}},
		Steps: map[string]runbook.Step{
			"search": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reddit-search", SkillVersion: "2.0.0", Action: "search", Arguments: map[string]runbook.Value{"limit": literalActionValue(25)}, ResultPath: "/results/search", Next: "done"}},
			"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	return definition
}

func sourceScopeRequest(existing *WorkforceCandidate) GenerateRequest {
	return GenerateRequest{
		Mode: ModeAmend, Prompt: "Monitor Reddit every 5 minutes", Existing: existing, Catalog: sourceScopeCatalog(),
		Refinement: &RefinementContext{Answers: []RefinementResolvedAnswer{{QuestionID: CapabilitySourceScopeQuestionID("reddit-access"), Value: RefinementProviderAnswerValue{Items: []string{"openclaw"}}, Source: RefinementAnswerSourceUser}}},
	}
}

func decodedActionArgument(t *testing.T, definition *agent.AgentDefinition, stepID, name string) interface{} {
	t.Helper()
	var value interface{}
	if err := json.Unmarshal(definition.Runbook.Steps[stepID].Action.Arguments[name].Literal, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCompilerMaterializesAnsweredSourceScopeAndPreservesDroppedRunbook(t *testing.T) {
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", true)}}
	generated := GenerationResponse{Candidate: WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.1.0", false)}}}
	payload, _ := json.Marshal(generated)
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
	if got := decodedActionArgument(t, first.Candidate.Agents[0], "search", "subreddit"); got != "openclaw" {
		t.Fatalf("materialized subreddit = %#v", got)
	}
	if got := decodedActionArgument(t, first.Candidate.Agents[0], "search", "limit"); got != float64(25) {
		t.Fatalf("preserved limit = %#v", got)
	}
}

func TestMaterializesSourceScopeThroughScheduledDelegateRunbook(t *testing.T) {
	catalog := sourceScopeCatalog()
	catalog.CapabilityNeeds[0].SourceScope.Targets = []string{"r/openclaw", "r/selfhosted"}
	definition := sourceScopeAgent("1.0.0", false)
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "review", Version: "1.0.0", Name: "Review",
		Entrypoints: map[string]string{"review": "review"},
		Interfaces:  map[string]runbook.Interface{"review": {Description: "Review configured communities.", InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}}}},
		Triggers:    map[string]runbook.Trigger{"daily": {Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}, Entrypoint: "review", ObjectiveID: WorkforceObjectiveKey("agent", "researcher", "monitor")}},
		Steps: map[string]runbook.Step{
			"review": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{AgentID: literalActionValue("researcher"), Goal: literalActionValue("Review bounded sources."), ResultPath: "/results/review", Next: "done"}},
			"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}}
	if issues := materializeAnsweredCapabilitySourceScopes(&candidate, GenerateRequest{Mode: ModeCreate, Catalog: catalog}); len(issues) != 0 {
		t.Fatalf("Runbook source materialization issues = %#v", issues)
	}
	trigger := definition.Runbook.Triggers["daily"]
	var communities []string
	if json.Unmarshal(trigger.Input["subreddits"].Literal, &communities) != nil || !reflect.DeepEqual(communities, []string{"r/openclaw", "r/selfhosted"}) {
		t.Fatalf("trigger source input = %#v", trigger.Input)
	}
	if reference := definition.Runbook.Steps["review"].Delegate.Context["subreddits"].Ref; reference != "/input/subreddits" {
		t.Fatalf("delegate source reference = %q", reference)
	}
}

func TestCompilerMaterializesAnsweredSourceScopeIntoEventRunbook(t *testing.T) {
	definition := sourceScopeAgent("1.0.0", true)
	definition.Runbook.Triggers = map[string]runbook.Trigger{"wake": {Kind: runbook.TriggerEvent, EventType: "source.poll.received", Entrypoint: "monitor", ObjectiveID: WorkforceObjectiveKey("agent", "researcher", "monitor")}}
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: existing})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	request := sourceScopeRequest(&existing)
	request.Prompt = "Handle permitted Reddit source events"
	result, err := compiler.Compile(context.Background(), request)
	if err != nil || !result.Valid || decodedActionArgument(t, result.Candidate.Agents[0], "search", "subreddit") != "openclaw" {
		t.Fatalf("event materialization result=%#v err=%v", result, err)
	}
}

func TestCompilerBlocksAmbiguousSourceScopeMaterialization(t *testing.T) {
	first, second := sourceScopeAgent("1.0.0", true), sourceScopeAgent("1.0.0", true)
	second.ID, second.DisplayName = "secondary", "Secondary"
	second.ObjectiveTemplates[0].ID = "secondary"
	trigger := second.Runbook.Triggers["every-five-minutes"]
	trigger.ObjectiveID = WorkforceObjectiveKey("agent", "secondary", "secondary")
	second.Runbook.Triggers["every-five-minutes"] = trigger
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{first, second}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Monitor Reddit every 5 minutes", Catalog: sourceScopeCatalog(), Refinement: sourceScopeRequest(nil).Refinement})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "source_scope_action_ambiguous") {
		t.Fatalf("ambiguous actions result=%#v err=%v", result, err)
	}
}

func TestCompilerBindsSourceScopeToSingleRunbookWhenSeveralSkillActionsParticipate(t *testing.T) {
	definition := sourceScopeAgent("1.0.0", true)
	search := definition.Runbook.Steps["search"]
	search.Action.Next = "snapshot"
	definition.Runbook.Steps["search"] = search
	definition.Runbook.Steps["snapshot"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{
		SkillID: "reddit-search", SkillVersion: "2.0.0", Action: "search",
		Arguments: map[string]runbook.Value{"query": literalActionValue("recent")}, ResultPath: "/results/snapshot", Next: "done",
	}}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Monitor Reddit every 5 minutes", Catalog: sourceScopeCatalog(), Refinement: sourceScopeRequest(nil).Refinement,
	})
	if err != nil || hasValidationCode(result.Validation, "source_scope_action_ambiguous") {
		t.Fatalf("single Runbook source boundary result=%#v err=%v", result, err)
	}
	trigger := result.Candidate.Agents[0].Runbook.Triggers["every-five-minutes"]
	if got := trigger.Input["subreddit"]; len(got.Literal) == 0 {
		t.Fatalf("Runbook trigger did not receive audited source scope: %#v", trigger.Input)
	}
}

func TestCompilerBlocksSourceScopeWithoutMatchingRunbook(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{sourceScopeAgent("1.0.0", false)}}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Monitor Reddit every 5 minutes", Catalog: sourceScopeCatalog(), Refinement: sourceScopeRequest(nil).Refinement})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "source_scope_action_not_found") {
		t.Fatalf("missing Runbook result=%#v err=%v", result, err)
	}
}
