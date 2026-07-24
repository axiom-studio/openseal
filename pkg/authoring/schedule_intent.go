package authoring

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/workforce"
)

const scheduleIntentQuestionID = "schedule-intent"

type scheduleIntentKind int

const (
	scheduleIntentAbsent scheduleIntentKind = iota
	scheduleIntentManual
	scheduleIntentAmbiguous
	scheduleIntentExact
)

type scheduleIntent struct {
	kind            scheduleIntentKind
	cadenceType     string
	intervalSeconds int64
	timeOfDay       string
	dayOfWeek       string
	cronExpression  string
	timezone        string
}

var (
	scheduleClockPattern    = regexp.MustCompile(`(?i)\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	scheduleTimezonePattern = regexp.MustCompile(`\b(?:UTC|GMT|[A-Za-z][A-Za-z0-9_+.-]*/[A-Za-z][A-Za-z0-9_+./-]*)\b`)
	scheduleIntervalPattern = regexp.MustCompile(`(?i)\bevery\s+(\d{1,7})\s+(second|seconds|minute|minutes|hour|hours|day|days)\b`)
)

// enforceScheduleIntentAuthority is the deterministic authority boundary for
// recurring work. A provider-created cadence is never evidence that the user
// requested recurring execution. Only an exact prompt clause or the audited
// answer to OpenSeal's reserved schedule question may authorize a new cadence.
func enforceScheduleIntentAuthority(generated *GenerationResponse, request GenerateRequest) []ValidationIssue {
	if generated == nil {
		return nil
	}
	intent := parseScheduleIntent(scheduleIntentAuthorityText(request))
	switch intent.kind {
	case scheduleIntentManual:
		if removeAllCandidateCadences(&generated.Candidate) {
			generated.Assumptions = append(generated.Assumptions, "Recurring schedules were removed because the request requires manual execution.")
		}
		removeScheduleIntentQuestion(generated)
		return nil
	case scheduleIntentAbsent:
		if restoreExistingAndRemoveNewCadences(&generated.Candidate, request.Existing) {
			generated.Assumptions = append(generated.Assumptions, "Provider-created recurring schedules were removed because recurring execution was not requested.")
		}
		if answeredSourceScopeNeedsExecutionChoice(&generated.Candidate, request) {
			upsertScheduleIntentQuestion(generated)
			return nil
		}
		removeScheduleIntentQuestion(generated)
		return nil
	case scheduleIntentAmbiguous:
		restoreExistingAndRemoveNewCadences(&generated.Candidate, request.Existing)
		upsertScheduleIntentQuestion(generated)
		return nil
	case scheduleIntentExact:
		removeScheduleIntentQuestion(generated)
		return validateAuthorizedCandidateCadences(&generated.Candidate, request.Existing, intent)
	default:
		return nil
	}
}

func scheduleIntentAuthorityText(request GenerateRequest) string {
	text := strings.TrimSpace(request.Prompt)
	if request.Refinement == nil {
		return text
	}
	for _, answer := range request.Refinement.Answers {
		if answer.QuestionID != scheduleIntentQuestionID {
			continue
		}
		if value := strings.TrimSpace(answer.Value.Text); value != "" {
			text += ". " + value
		}
	}
	return text
}

func parseScheduleIntent(value string) scheduleIntent {
	lower := strings.ToLower(value)
	if containsAnySchedulePhrase(lower,
		"manual objective", "manual execution", "run manually", "only manually", "on demand", "when asked",
		"do not schedule", "don't schedule", "no schedule", "without a schedule", "not recurring",
	) {
		return scheduleIntent{kind: scheduleIntentManual}
	}

	if match := scheduleIntervalPattern.FindStringSubmatch(value); len(match) == 3 {
		count, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && count > 0 {
			multipliers := map[string]int64{"second": 1, "seconds": 1, "minute": 60, "minutes": 60, "hour": 3600, "hours": 3600, "day": 86400, "days": 86400}
			return scheduleIntent{kind: scheduleIntentExact, cadenceType: "interval", intervalSeconds: count * multipliers[strings.ToLower(match[2])]}
		}
	}
	if containsWord(lower, "hourly") || containsSchedulePhrase(lower, "every hour") {
		return scheduleIntent{kind: scheduleIntentExact, cadenceType: "interval", intervalSeconds: 3600}
	}

	if containsWord(lower, "weekday") || containsWord(lower, "weekdays") {
		clock := scheduleClockPattern.FindStringSubmatch(value)
		zone := scheduleTimezonePattern.FindString(value)
		if len(clock) == 3 && zone != "" {
			if _, err := time.LoadLocation(zone); err == nil {
				hour, _ := strconv.Atoi(clock[1])
				minute, _ := strconv.Atoi(clock[2])
				return scheduleIntent{
					kind:           scheduleIntentExact,
					cadenceType:    "cron",
					cronExpression: fmt.Sprintf("0 %d %d * * 1-5", minute, hour),
					timezone:       zone,
				}
			}
		}
		return scheduleIntent{kind: scheduleIntentAmbiguous}
	}

	frequency := ""
	if containsWord(lower, "daily") || containsSchedulePhrase(lower, "every day") {
		frequency = "daily"
	} else if containsWord(lower, "weekly") || containsSchedulePhrase(lower, "every week") {
		frequency = "weekly"
	}
	if frequency != "" {
		clock := scheduleClockPattern.FindStringSubmatch(value)
		zone := scheduleTimezonePattern.FindString(value)
		if len(clock) == 3 && zone != "" {
			if _, err := time.LoadLocation(zone); err == nil {
				hour, _ := strconv.Atoi(clock[1])
				intent := scheduleIntent{kind: scheduleIntentExact, cadenceType: frequency, timeOfDay: fmt.Sprintf("%02d:%s", hour, clock[2]), timezone: zone}
				if frequency == "daily" {
					return intent
				}
				for _, day := range []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"} {
					if containsWord(lower, day) {
						intent.dayOfWeek = day
						return intent
					}
				}
			}
		}
		return scheduleIntent{kind: scheduleIntentAmbiguous}
	}

	if containsAnyScheduleWord(lower, "recurring", "recurrently", "regularly", "periodically", "scheduled", "schedule", "cadence", "continuously", "continuous") {
		return scheduleIntent{kind: scheduleIntentAmbiguous}
	}
	return scheduleIntent{kind: scheduleIntentAbsent}
}

func containsAnySchedulePhrase(value string, phrases ...string) bool {
	for _, phrase := range phrases {
		if containsSchedulePhrase(value, phrase) {
			return true
		}
	}
	return false
}

func containsSchedulePhrase(value, phrase string) bool {
	return strings.Contains(" "+strings.Join(commitmentTokens(value), " ")+" ", " "+strings.Join(commitmentTokens(phrase), " ")+" ")
}

func containsAnyScheduleWord(value string, words ...string) bool {
	for _, word := range words {
		if containsWord(value, word) {
			return true
		}
	}
	return false
}

func containsWord(value, word string) bool {
	for _, token := range commitmentTokens(value) {
		if token == word {
			return true
		}
	}
	return false
}

func upsertScheduleIntentQuestion(generated *GenerationResponse) {
	removeScheduleIntentQuestion(generated)
	generated.UnresolvedQuestions = append(generated.UnresolvedQuestions, RefinementQuestion{
		ID:        scheduleIntentQuestionID,
		Category:  RefinementCategoryPolicy,
		Prompt:    "When should this work run? Choose “on demand”, or specify an exact schedule such as “daily at 09:00 UTC”.",
		WhyNeeded: "This source is ready, but OpenSeal needs to know whether the Agent should wait for you or run automatically.",
		Blocking:  []RefinementBlockingScope{RefinementBlocksCandidate, RefinementBlocksApply},
		Answer: RefinementAnswerSchema{
			Kind: RefinementAnswerText, Minimum: 1, Maximum: 256,
		},
		Provenance: []RefinementQuestionProvenance{{
			Kind: RefinementProvenancePrompt, Evidence: "The request implies recurring work but does not specify a complete portable cadence.",
		}},
		Priority: 950,
	})
}

// answeredSourceScopeNeedsExecutionChoice prevents a guided source setup from
// ending in the impossible state where every visible question is answered but
// no execution mode was authorized. Existing durable actions remain valid;
// otherwise the operator chooses on-demand work or an exact schedule.
func answeredSourceScopeNeedsExecutionChoice(candidate *WorkforceCandidate, request GenerateRequest) bool {
	if request.Refinement == nil {
		return false
	}
	answered := make(map[string]RefinementProviderAnswerValue, len(request.Refinement.Answers))
	for _, answer := range request.Refinement.Answers {
		answered[strings.TrimSpace(answer.QuestionID)] = answer.Value
	}
	for _, need := range request.Catalog.CapabilityNeeds {
		if need.SourceScope == nil {
			continue
		}
		answer, exists := answered[CapabilitySourceScopeQuestionID(need.ID)]
		if !exists || len(nonEmptyUnique(answer.Items)) == 0 {
			continue
		}
		selected := selectedCapabilityNeed(need, answered)
		if len(matchingSourceScopeInvocations(candidate, selected, request.Catalog)) == 0 {
			return true
		}
	}
	return false
}

func sourceCapabilityRequiresDurableAction(request GenerateRequest) bool {
	return parseScheduleIntent(scheduleIntentAuthorityText(request)).kind != scheduleIntentManual
}

func removeScheduleIntentQuestion(generated *GenerationResponse) {
	questions := generated.UnresolvedQuestions[:0]
	for _, question := range generated.UnresolvedQuestions {
		if question.ID != scheduleIntentQuestionID {
			questions = append(questions, question)
		}
	}
	generated.UnresolvedQuestions = questions
}

func hasScheduleIntentQuestion(questions []RefinementQuestion) bool {
	for _, question := range questions {
		if question.ID == scheduleIntentQuestionID {
			return true
		}
	}
	return false
}

func deferScheduleBlockedMaterializationIssues(issues []ValidationIssue) []ValidationIssue {
	result := issues[:0]
	for _, current := range issues {
		if current.Code != "source_scope_action_not_found" {
			result = append(result, current)
		}
	}
	return result
}

func removeAllCandidateCadences(candidate *WorkforceCandidate) bool {
	changed := false
	visitCandidateObjectives(candidate, func(_ string, templates []workforce.ObjectiveTemplate, _ []workforce.ObjectiveTemplate) {
		for index := range templates {
			if len(templates[index].Cadence) > 0 {
				templates[index].Cadence = nil
				changed = true
			}
		}
	})
	return changed
}

func restoreExistingAndRemoveNewCadences(candidate *WorkforceCandidate, existing *WorkforceCandidate) bool {
	changed := false
	visitCandidateObjectivesWithExisting(candidate, existing, func(_ string, templates []workforce.ObjectiveTemplate, previous []workforce.ObjectiveTemplate) {
		byID := objectiveTemplatesByID(previous)
		for index := range templates {
			old := byID[templates[index].ID]
			if old != nil && len(old.Cadence) > 0 {
				preserved := preserveExistingCadenceAuthority(old.Cadence, templates[index].Cadence)
				if !reflect.DeepEqual(templates[index].Cadence, preserved) {
					templates[index].Cadence = preserved
					changed = true
				}
				continue
			}
			if len(templates[index].Cadence) > 0 {
				templates[index].Cadence = nil
				changed = true
			}
		}
	})
	return changed
}

// preserveExistingCadenceAuthority keeps every schedule, concurrency, budget,
// and assignment field from the reviewed Objective while allowing an amendment
// to update the runTemplate payload. This is what lets a later source-scope
// answer materialize deterministic action inputs without authorizing the model
// to change when or how often that Objective runs.
func preserveExistingCadenceAuthority(existing, candidate map[string]interface{}) map[string]interface{} {
	preserved := cloneAuthoringMap(existing)
	if candidate != nil {
		if runTemplate, ok := candidate["runTemplate"]; ok {
			preserved["runTemplate"] = cloneAuthoringMap(map[string]interface{}{"value": runTemplate})["value"]
		}
	}
	return preserved
}

func validateAuthorizedCandidateCadences(candidate, existing *WorkforceCandidate, intent scheduleIntent) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	matched := false
	visitCandidateObjectivesWithExisting(candidate, existing, func(path string, templates []workforce.ObjectiveTemplate, previous []workforce.ObjectiveTemplate) {
		byID := objectiveTemplatesByID(previous)
		for index := range templates {
			cadence := templates[index].Cadence
			if len(cadence) == 0 {
				continue
			}
			if old := byID[templates[index].ID]; old != nil && reflect.DeepEqual(cadence, old.Cadence) {
				matched = matched || cadenceMatchesScheduleIntent(cadence, intent)
				continue
			}
			if cadenceMatchesScheduleIntent(cadence, intent) {
				matched = true
				continue
			}
			issues = append(issues, issue(fmt.Sprintf("%s[%d].cadence", path, index), "schedule_intent_mismatch", "Generated cadence does not match the exact cadence authorized by the request"))
		}
	})
	if !matched {
		issues = append(issues, issue("candidate", "requested_schedule_missing", "The request authorizes an exact recurring cadence, but no new Objective preserves that schedule"))
	}
	return issues
}

func cadenceMatchesScheduleIntent(cadence map[string]interface{}, intent scheduleIntent) bool {
	if strings.TrimSpace(fmt.Sprint(cadence["type"])) != intent.cadenceType {
		return false
	}
	if intent.cadenceType == "interval" {
		value, ok := numericInt64(cadence["intervalSeconds"])
		return ok && value == intent.intervalSeconds
	}
	if intent.cadenceType == "cron" {
		return normalizeWhitespace(fmt.Sprint(cadence["cronExpression"])) == intent.cronExpression &&
			strings.TrimSpace(fmt.Sprint(cadence["timezone"])) == intent.timezone
	}
	if strings.TrimSpace(fmt.Sprint(cadence["timeOfDay"])) != intent.timeOfDay || strings.TrimSpace(fmt.Sprint(cadence["timezone"])) != intent.timezone {
		return false
	}
	return intent.cadenceType != "weekly" || strings.EqualFold(strings.TrimSpace(fmt.Sprint(cadence["dayOfWeek"])), intent.dayOfWeek)
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func numericInt64(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		integer := int64(number)
		return integer, float64(integer) == number
	default:
		return 0, false
	}
}

func objectiveTemplatesByID(templates []workforce.ObjectiveTemplate) map[string]*workforce.ObjectiveTemplate {
	result := make(map[string]*workforce.ObjectiveTemplate, len(templates))
	for index := range templates {
		result[templates[index].ID] = &templates[index]
	}
	return result
}

func visitCandidateObjectives(candidate *WorkforceCandidate, visit func(string, []workforce.ObjectiveTemplate, []workforce.ObjectiveTemplate)) {
	visitCandidateObjectivesWithExisting(candidate, nil, visit)
}

func visitCandidateObjectivesWithExisting(candidate, existing *WorkforceCandidate, visit func(string, []workforce.ObjectiveTemplate, []workforce.ObjectiveTemplate)) {
	if candidate == nil {
		return
	}
	existingAgents := map[string][]workforce.ObjectiveTemplate{}
	if existing != nil {
		for _, definition := range existing.Agents {
			if definition != nil {
				existingAgents[definition.ID] = definition.ObjectiveTemplates
			}
		}
	}
	for index, definition := range candidate.Agents {
		if definition != nil {
			visit(fmt.Sprintf("agents[%d].objectiveTemplates", index), definition.ObjectiveTemplates, existingAgents[definition.ID])
		}
	}
	if candidate.Team != nil {
		var previous []workforce.ObjectiveTemplate
		if existing != nil && existing.Team != nil && existing.Team.ID == candidate.Team.ID {
			previous = existing.Team.ObjectiveTemplates
		}
		visit("team.objectiveTemplates", candidate.Team.ObjectiveTemplates, previous)
	}
}
