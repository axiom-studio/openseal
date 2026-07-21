package authoring

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func scheduledAuthoringCandidate(cadence map[string]interface{}) WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "operator", Version: "1.0.0", DisplayName: "Operator", Purpose: "Complete requested work", SystemPrompt: "Complete only authorized work.",
		Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{
			ID: "operate", Title: "Operate", Goal: "Complete the requested work", Priority: 1, Cadence: cadence,
		}},
	}}}
}

func dailyCadence(timeOfDay, timezone string) map[string]interface{} {
	return map[string]interface{}{"type": "daily", "timeOfDay": timeOfDay, "timezone": timezone, "assignedAgentId": "operator"}
}

func compileScheduledCandidate(t *testing.T, prompt string, candidate WorkforceCandidate, refinement *RefinementContext, existing *WorkforceCandidate) *CompileResult {
	t.Helper()
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	mode := ModeCreate
	if existing != nil {
		mode = ModeAmend
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: mode, Prompt: prompt, Existing: existing, Refinement: refinement})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScheduleIntentManualPromptCannotGainCadence(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent with one manual objective", scheduledAuthoringCandidate(dailyCadence("09:00", "UTC")), nil, nil)
	if !result.Valid || len(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence) != 0 {
		t.Fatalf("manual candidate = %#v", result)
	}
	if !containsString(result.Assumptions, "Recurring schedules were removed because the request requires manual execution.") {
		t.Fatalf("manual assumptions = %#v", result.Assumptions)
	}
}

func TestScheduleIntentExactDailyTimezoneIsPreserved(t *testing.T) {
	cadence := dailyCadence("09:00", "UTC")
	result := compileScheduledCandidate(t, "Create one Agent that runs daily at 09:00 UTC", scheduledAuthoringCandidate(cadence), nil, nil)
	if !result.Valid || !reflect.DeepEqual(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence, cadence) {
		t.Fatalf("exact daily candidate = %#v", result)
	}
}

func TestScheduleIntentAmbiguousRecurrenceProducesTypedGuidance(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(dailyCadence("09:00", "UTC")), nil, nil)
	if result.Valid || len(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence) != 0 || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("ambiguous recurring candidate = %#v", result)
	}
	question := result.UnresolvedQuestions[0]
	if question.ID != scheduleIntentQuestionID || question.Category != RefinementCategoryPolicy || question.Answer.Kind != RefinementAnswerText ||
		question.Answer.Minimum != 1 || question.Answer.Maximum != 256 || len(question.Blocking) != 2 || len(question.Provenance) != 1 {
		t.Fatalf("schedule guidance = %#v", question)
	}
}

func TestScheduleIntentQuestionDefersScheduleBlockedSourceMaterialization(t *testing.T) {
	candidate := scheduledAuthoringCandidate(map[string]interface{}{
		"type": "interval", "intervalSeconds": float64(300),
		"runTemplate": map[string]interface{}{"capability": map[string]interface{}{
			"skillId": "reddit-search", "skillVersion": "2.0.0", "action": "search", "inputs": map[string]interface{}{},
		}},
	})
	candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "reddit-search", VersionConstraint: "2.0.0", RequiredActions: []string{"search"}}}
	candidate.Agents[0].Authority.AllowedSkillIDs = []string{"reddit-search"}
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create one Agent that researches Reddit regularly",
		Catalog: CapabilityCatalog{
			Skills: map[string]SkillCapability{"reddit-search": {ID: "reddit-search", Version: "2.0.0", Actions: []string{"search"}, Readiness: SkillReadinessReady}},
			CapabilityNeeds: []CapabilityNeed{{
				ID: "reddit-access", Prompt: "How should Reddit be accessed?", WhyNeeded: "A verified source action is required.", SkillIDs: []string{"reddit-search"}, Priority: 900,
				SourceScope: &CapabilitySourceScopeRequirement{Prompt: "Which subreddits?", WhyNeeded: "Source scope must be explicit.", Minimum: 1, Maximum: 20, Priority: 950, MaterializationInputKeys: []string{"subreddit"}},
			}},
		},
		Refinement: &RefinementContext{Answers: []RefinementResolvedAnswer{{
			QuestionID: CapabilitySourceScopeQuestionID("reddit-access"), Source: RefinementAnswerSourceUser,
			Value: RefinementProviderAnswerValue{Items: []string{"openseal"}},
		}}},
	})
	if err != nil || result.Valid || len(result.Validation) != 0 || len(result.UnresolvedQuestions) != 1 ||
		result.UnresolvedQuestions[0].ID != scheduleIntentQuestionID || len(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence) != 0 {
		t.Fatalf("schedule-blocked source materialization = %#v, err = %v", result, err)
	}
}

func TestScheduleIntentAuditedAnswerAuthorizesExactCadence(t *testing.T) {
	refinement := &RefinementContext{Answers: []RefinementResolvedAnswer{{
		QuestionID: scheduleIntentQuestionID, Source: RefinementAnswerSourceUser,
		Value: RefinementProviderAnswerValue{Text: "daily at 09:00 UTC"},
	}}}
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(dailyCadence("09:00", "UTC")), refinement, nil)
	if !result.Valid || len(result.UnresolvedQuestions) != 0 || len(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence) == 0 {
		t.Fatalf("answered recurring candidate = %#v", result)
	}
}

func TestScheduleIntentMismatchedCadenceFailsClosed(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent that runs daily at 09:00 UTC", scheduledAuthoringCandidate(dailyCadence("10:00", "UTC")), nil, nil)
	if result.Valid || len(result.Candidate.Agents[0].ObjectiveTemplates[0].Cadence) == 0 ||
		!hasValidationCode(result.Validation, "schedule_intent_mismatch") || !hasValidationCode(result.Validation, "requested_schedule_missing") {
		t.Fatalf("mismatched recurring candidate = %#v", result)
	}
}

func TestScheduleIntentAmendmentPreservesExistingAndRemovesWidening(t *testing.T) {
	existing := scheduledAuthoringCandidate(dailyCadence("08:00", "UTC"))
	candidate := scheduledAuthoringCandidate(dailyCadence("09:00", "UTC"))
	candidate.Agents[0].Version = "1.1.0"
	candidate.Agents[0].ObjectiveTemplates = append(candidate.Agents[0].ObjectiveTemplates, workforce.ObjectiveTemplate{
		ID: "invented", Title: "Invented", Goal: "Unrequested recurring work", Priority: 2, Cadence: dailyCadence("10:00", "UTC"),
	})
	result := compileScheduledCandidate(t, "Improve the Agent's instructions", candidate, nil, &existing)
	objectives := result.Candidate.Agents[0].ObjectiveTemplates
	if !result.Valid || !reflect.DeepEqual(objectives[0].Cadence, existing.Agents[0].ObjectiveTemplates[0].Cadence) || len(objectives[1].Cadence) != 0 {
		t.Fatalf("non-schedule amendment = %#v", result)
	}
}
