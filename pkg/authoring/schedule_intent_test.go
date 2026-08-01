package authoring

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func scheduledAuthoringCandidate(schedule *runbook.Schedule) WorkforceCandidate {
	definition := &agent.AgentDefinition{
		ID: "operator", Version: "1.0.0", DisplayName: "Operator", Purpose: "Complete requested work", SystemPrompt: "Complete only authorized work.",
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "operate", Title: "Operate", Goal: "Complete the requested work", Priority: 1}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "operate", Version: "1.0.0", Name: "Operate",
			Entrypoints: map[string]string{"operate": "done"},
			Steps:       map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
			Triggers:    map[string]runbook.Trigger{},
		},
	}
	if schedule != nil {
		definition.Runbook.Triggers["recurring"] = runbook.Trigger{
			Kind: runbook.TriggerSchedule, Schedule: schedule, Entrypoint: "operate",
			ObjectiveID: WorkforceObjectiveKey("agent", "operator", "operate"), MaximumConcurrent: 1,
		}
	}
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{definition}}
}

func dailySchedule(timeOfDay, timezone string) *runbook.Schedule {
	hour, minute, err := scheduleClockParts(timeOfDay)
	if err != nil {
		panic(err)
	}
	return &runbook.Schedule{Cron: "0 " + strconv.Itoa(minute) + " " + strconv.Itoa(hour) + " * * *", Timezone: timezone}
}

func weekdaySchedule(timeOfDay, timezone string) *runbook.Schedule {
	hour, minute, err := scheduleClockParts(timeOfDay)
	if err != nil {
		panic(err)
	}
	return &runbook.Schedule{Cron: "0 " + strconv.Itoa(minute) + " " + strconv.Itoa(hour) + " * * 1-5", Timezone: timezone}
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

func candidateSchedule(result *CompileResult) *runbook.Schedule {
	if result == nil || len(result.Candidate.Agents) == 0 || result.Candidate.Agents[0].Runbook == nil {
		return nil
	}
	trigger, ok := result.Candidate.Agents[0].Runbook.Triggers["recurring"]
	if !ok {
		return nil
	}
	return trigger.Schedule
}

func TestScheduleIntentManualPromptCannotGainRunbookTrigger(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent with one manual objective", scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), nil, nil)
	if !result.Valid || candidateSchedule(result) != nil {
		t.Fatalf("manual candidate = %#v", result)
	}
	if !containsString(result.Assumptions, "Recurring schedules were removed because the request requires manual execution.") {
		t.Fatalf("manual assumptions = %#v", result.Assumptions)
	}
}

func TestScheduleIntentExplicitlyRejectsRecurringRunbooks(t *testing.T) {
	for _, prompt := range []string{"Create one inactive Agent that answers general questions. Give it no recurring objectives.", "Create one Agent without recurring work.", "Create one Agent for non-recurring work."} {
		t.Run(prompt, func(t *testing.T) {
			result := compileScheduledCandidate(t, prompt, scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), nil, nil)
			if !result.Valid || hasScheduleIntentQuestion(result.UnresolvedQuestions) || candidateSchedule(result) != nil {
				t.Fatalf("explicit non-recurring candidate = %#v", result)
			}
		})
	}
}

func TestScheduleIntentExactDailyTimezoneIsPreserved(t *testing.T) {
	schedule := dailySchedule("09:00", "UTC")
	result := compileScheduledCandidate(t, "Create one Agent that runs daily at 09:00 UTC", scheduledAuthoringCandidate(schedule), nil, nil)
	if !result.Valid || !reflect.DeepEqual(candidateSchedule(result), schedule) {
		t.Fatalf("exact daily candidate = %#v", result)
	}
}

func TestScheduleIntentPreservesExactBoundedHourlyOccurrences(t *testing.T) {
	schedule := &runbook.Schedule{Cron: "0 0 */1 * * *", Timezone: "UTC", MaximumOccurrences: 5}
	result := compileScheduledCandidate(t, "Create one Agent that runs hourly for 5 hours", scheduledAuthoringCandidate(schedule), nil, nil)
	if !result.Valid || !reflect.DeepEqual(candidateSchedule(result), schedule) {
		t.Fatalf("bounded hourly candidate = %#v", result)
	}

	unbounded := &runbook.Schedule{Cron: "0 0 */1 * * *", Timezone: "UTC"}
	mismatch := compileScheduledCandidate(t, "Create one Agent that runs hourly for 5 hours", scheduledAuthoringCandidate(unbounded), nil, nil)
	if mismatch.Valid || !hasValidationCode(mismatch.Validation, "schedule_intent_mismatch") {
		t.Fatalf("unbounded mismatch = %#v", mismatch)
	}
}

func TestScheduleIntentExactWeekdaysUsesPortableCronTrigger(t *testing.T) {
	schedule := weekdaySchedule("09:00", "UTC")
	result := compileScheduledCandidate(t, "Create one Agent that runs every weekday at 09:00 UTC", scheduledAuthoringCandidate(schedule), nil, nil)
	if !result.Valid || len(result.UnresolvedQuestions) != 0 || !reflect.DeepEqual(candidateSchedule(result), schedule) {
		t.Fatalf("exact weekday candidate = %#v", result)
	}
}

func TestScheduleIntentAmbiguousRecurrenceProducesTypedGuidance(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), nil, nil)
	if result.Valid || candidateSchedule(result) != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("ambiguous recurring candidate = %#v", result)
	}
	question := result.UnresolvedQuestions[0]
	if question.ID != scheduleIntentQuestionID || question.Category != RefinementCategoryPolicy || question.Answer.Kind != RefinementAnswerText || question.Answer.Minimum != 1 || question.Answer.Maximum != 256 || len(question.Blocking) != 2 || len(question.Provenance) != 1 {
		t.Fatalf("schedule guidance = %#v", question)
	}
}

func TestScheduleIntentNaturalOnceDailyLanguageRequestsMissingClockAndTimezone(t *testing.T) {
	for _, prompt := range []string{"Create one Agent that posts once a day", "Create one Agent that posts once per day", "Create one Agent that runs every single day", "Create one Agent that posts every single day, once a day"} {
		t.Run(prompt, func(t *testing.T) {
			result := compileScheduledCandidate(t, prompt, scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), nil, nil)
			if result.Valid || candidateSchedule(result) != nil || len(result.UnresolvedQuestions) != 1 || result.UnresolvedQuestions[0].ID != scheduleIntentQuestionID {
				t.Fatalf("natural once-daily candidate = %#v", result)
			}
		})
	}
}

func TestScheduleIntentDailyVariedTimeUsesPortableJitterWindow(t *testing.T) {
	schedule := dailySchedule("00:00", "UTC")
	schedule.JitterSeconds = 86399
	prompt := "Create one Agent that scours r/greenwoodworking and r/woodcarving once per day at a varied time UTC"
	result := compileScheduledCandidate(t, prompt, scheduledAuthoringCandidate(schedule), nil, nil)
	if !result.Valid || hasScheduleIntentQuestion(result.UnresolvedQuestions) || len(result.Validation) != 0 {
		t.Fatalf("varied daily schedule = %#v", result)
	}
	missingJitter := compileScheduledCandidate(t, prompt, scheduledAuthoringCandidate(dailySchedule("00:00", "UTC")), nil, nil)
	if missingJitter.Valid || !hasValidationCode(missingJitter.Validation, "schedule_intent_mismatch") {
		t.Fatalf("fixed schedule satisfied varied-time intent: %#v", missingJitter)
	}
}

func TestScheduleIntentAuditedAnswerAuthorizesExactTrigger(t *testing.T) {
	refinement := &RefinementContext{Answers: []RefinementResolvedAnswer{{QuestionID: scheduleIntentQuestionID, Source: RefinementAnswerSourceUser, Value: RefinementProviderAnswerValue{Text: "daily at 09:00 UTC"}}}}
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), refinement, nil)
	if !result.Valid || len(result.UnresolvedQuestions) != 0 || candidateSchedule(result) == nil {
		t.Fatalf("answered recurring candidate = %#v", result)
	}
}

func TestScheduleIntentAmbiguousAnswerProducesANewBlockingClarification(t *testing.T) {
	refinement := &RefinementContext{Answers: []RefinementResolvedAnswer{{
		QuestionID: scheduleIntentQuestionID, Source: RefinementAnswerSourceUser,
		Value: RefinementProviderAnswerValue{Text: "daily at a varied time, 10 times between 08:00 and 20:00"},
	}}}
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(dailySchedule("09:00", "UTC")), refinement, nil)
	if result.Valid || candidateSchedule(result) != nil || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("ambiguous answered schedule = %#v", result)
	}
	question := result.UnresolvedQuestions[0]
	if question.ID != scheduleIntentClarificationQuestionIDPrefix+"1" || !hasScheduleIntentQuestion(result.UnresolvedQuestions) {
		t.Fatalf("schedule clarification = %#v", question)
	}
	if unanswered := unansweredRefinementQuestions(result.UnresolvedQuestions, ChangeSetRefinement{Answers: []RefinementAnswerEvent{{
		QuestionID: scheduleIntentQuestionID, Value: RefinementAnswerValue{Text: "daily at a varied time, 10 times between 08:00 and 20:00"},
	}}}); len(unanswered) != 1 || unanswered[0].ID != question.ID {
		t.Fatalf("new clarification was incorrectly treated as answered: %#v", unanswered)
	}
}

func TestScheduleIntentExactFollowupAuthorizesTrigger(t *testing.T) {
	refinement := &RefinementContext{Answers: []RefinementResolvedAnswer{
		{QuestionID: scheduleIntentQuestionID, Source: RefinementAnswerSourceUser, Value: RefinementProviderAnswerValue{Text: "daily at a varied time, 10 times between 08:00 and 20:00"}},
		{QuestionID: scheduleIntentClarificationQuestionIDPrefix + "1", Source: RefinementAnswerSourceUser, Value: RefinementProviderAnswerValue{Text: `cron "0 0 9 * * *" timezone UTC jitter 300 seconds`}},
	}}
	schedule := dailySchedule("09:00", "UTC")
	schedule.JitterSeconds = 300
	result := compileScheduledCandidate(t, "Create one Agent that runs regularly", scheduledAuthoringCandidate(schedule), refinement, nil)
	if !result.Valid || hasScheduleIntentQuestion(result.UnresolvedQuestions) || !reflect.DeepEqual(candidateSchedule(result), schedule) {
		t.Fatalf("exact schedule clarification = %#v", result)
	}
}

func TestScheduleIntentMismatchedTriggerFailsClosed(t *testing.T) {
	result := compileScheduledCandidate(t, "Create one Agent that runs daily at 09:00 UTC", scheduledAuthoringCandidate(dailySchedule("10:00", "UTC")), nil, nil)
	if result.Valid || candidateSchedule(result) == nil || !hasValidationCode(result.Validation, "schedule_intent_mismatch") || !hasValidationCode(result.Validation, "requested_schedule_missing") {
		t.Fatalf("mismatched recurring candidate = %#v", result)
	}
}

func TestScheduleIntentAmendmentPreservesExistingAndRemovesWidening(t *testing.T) {
	existing := scheduledAuthoringCandidate(dailySchedule("08:00", "UTC"))
	candidate := scheduledAuthoringCandidate(dailySchedule("09:00", "UTC"))
	candidate.Agents[0].Version = "1.1.0"
	candidate.Agents[0].Runbook.Triggers["invented"] = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: dailySchedule("10:00", "UTC"), Entrypoint: "operate", ObjectiveID: WorkforceObjectiveKey("agent", "operator", "operate")}
	result := compileScheduledCandidate(t, "Improve the Agent's instructions", candidate, nil, &existing)
	if !result.Valid || !reflect.DeepEqual(candidateSchedule(result), existing.Agents[0].Runbook.Triggers["recurring"].Schedule) || len(result.Candidate.Agents[0].Runbook.Triggers) != 1 {
		t.Fatalf("non-schedule amendment = %#v", result)
	}
}
