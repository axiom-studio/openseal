package authoring

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

const (
	scheduleIntentQuestionID                    = "schedule-intent"
	scheduleIntentClarificationQuestionIDPrefix = scheduleIntentQuestionID + "-clarification-"
)

type scheduleIntentKind int

const (
	scheduleIntentAbsent scheduleIntentKind = iota
	scheduleIntentManual
	scheduleIntentAmbiguous
	scheduleIntentExact
)

type scheduleIntent struct {
	kind               scheduleIntentKind
	cadenceType        string
	intervalSeconds    int64
	timeOfDay          string
	dayOfWeek          string
	cronExpression     string
	timezone           string
	jitterSeconds      int64
	maximumOccurrences int64
}

var (
	scheduleClockPattern      = regexp.MustCompile(`(?i)\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	scheduleTimezonePattern   = regexp.MustCompile(`\b(?:UTC|GMT|[A-Za-z][A-Za-z0-9_+.-]*/[A-Za-z][A-Za-z0-9_+./-]*)\b`)
	scheduleIntervalPattern   = regexp.MustCompile(`(?i)\bevery\s+(\d{1,7})\s+(second|seconds|minute|minutes|hour|hours|day|days)\b`)
	scheduleWindowPattern     = regexp.MustCompile(`(?i)\b(?:between|from)\s+([01]?\d|2[0-3]):([0-5]\d)\s+(?:and|to)\s+([01]?\d|2[0-3]):([0-5]\d)\b`)
	scheduleDurationPattern   = regexp.MustCompile(`(?i)\bfor\s+(\d{1,7})\s*(second|seconds|minute|minutes|hour|hours|day|days)\b`)
	scheduleOccurrencePattern = regexp.MustCompile(`(?i)\bfor\s+(\d{1,7})\s+(run|runs|occurrence|occurrences|time|times)\b`)
	scheduleCronPattern       = regexp.MustCompile(`(?i)\bcron\s+"([^"]+)"\s+timezone\s+([A-Za-z][A-Za-z0-9_+.-]*/?[A-Za-z0-9_+./-]*)(?:\s+jitter\s+(\d{1,8})\s+seconds)?\b`)
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
		if removeAllCandidateSchedules(&generated.Candidate) {
			generated.Assumptions = append(generated.Assumptions, "Recurring schedules were removed because the request requires manual execution.")
		}
		removeScheduleIntentQuestion(generated)
		return nil
	case scheduleIntentAbsent:
		if restoreExistingAndRemoveNewSchedules(&generated.Candidate, request.Existing) {
			generated.Assumptions = append(generated.Assumptions, "Provider-created recurring schedules were removed because recurring execution was not requested.")
		}
		removeScheduleIntentQuestion(generated)
		return nil
	case scheduleIntentAmbiguous:
		restoreExistingAndRemoveNewSchedules(&generated.Candidate, request.Existing)
		upsertScheduleIntentQuestion(generated, request)
		return nil
	case scheduleIntentExact:
		removeScheduleIntentQuestion(generated)
		return validateAuthorizedCandidateSchedules(&generated.Candidate, request.Existing, intent)
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
		if !isScheduleIntentQuestionID(answer.QuestionID) {
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
	// Explicit schedule negation owns the whole request. Phrases such as "on
	// demand" do not: a workforce may legitimately expose one manual operation
	// and a separate recurring operation, so those softer modality phrases are
	// considered only after every exact recurring form below.
	if containsAnySchedulePhrase(lower,
		"do not schedule", "don't schedule", "no schedule", "without a schedule", "not recurring",
		"no recurring", "without recurring", "non recurring",
	) {
		return scheduleIntent{kind: scheduleIntentManual}
	}
	if match := scheduleCronPattern.FindStringSubmatch(value); len(match) >= 3 {
		jitter := int64(0)
		if len(match) == 4 && match[3] != "" {
			jitter, _ = strconv.ParseInt(match[3], 10, 64)
		}
		intent := scheduleIntent{kind: scheduleIntentExact, cadenceType: "cron", cronExpression: match[1], timezone: match[2], jitterSeconds: jitter}
		if schedule, err := scheduleForIntent(intent); err == nil && schedule != nil {
			return intent
		}
		return scheduleIntent{kind: scheduleIntentAmbiguous}
	}

	if match := scheduleIntervalPattern.FindStringSubmatch(value); len(match) == 3 {
		count, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && count > 0 {
			multipliers := map[string]int64{"second": 1, "seconds": 1, "minute": 60, "minutes": 60, "hour": 3600, "hours": 3600, "day": 86400, "days": 86400}
			interval := count * multipliers[strings.ToLower(match[2])]
			if window := scheduleWindowPattern.FindStringSubmatch(value); len(window) == 5 {
				zone := validScheduleTimezone(value)
				startHour, _ := strconv.Atoi(window[1])
				endHour, _ := strconv.Atoi(window[3])
				if zone == "" || startHour > endHour || window[2] != window[4] || interval%3600 != 0 || interval/3600 > 23 {
					return scheduleIntent{kind: scheduleIntentAmbiguous}
				}
				return scheduleIntent{
					kind: scheduleIntentExact, cadenceType: "cron",
					cronExpression: fmt.Sprintf("0 %s %d-%d/%d * * *", window[2], startHour, endHour, interval/3600),
					timezone:       zone,
				}
			}
			return scheduleIntent{kind: scheduleIntentExact, cadenceType: "interval", intervalSeconds: interval, maximumOccurrences: boundedScheduleOccurrences(value, interval)}
		}
	}
	if containsWord(lower, "hourly") || containsSchedulePhrase(lower, "every hour") {
		return scheduleIntent{kind: scheduleIntentExact, cadenceType: "interval", intervalSeconds: 3600, maximumOccurrences: boundedScheduleOccurrences(value, 3600)}
	}

	if containsWord(lower, "weekday") || containsWord(lower, "weekdays") {
		clock := scheduleClockPattern.FindStringSubmatch(value)
		zone := validScheduleTimezone(value)
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
	if containsWord(lower, "daily") ||
		containsSchedulePhrase(lower, "every day") ||
		containsSchedulePhrase(lower, "every single day") ||
		containsSchedulePhrase(lower, "once a day") ||
		containsSchedulePhrase(lower, "once per day") {
		frequency = "daily"
	} else if containsWord(lower, "weekly") || containsSchedulePhrase(lower, "every week") {
		frequency = "weekly"
	}
	if frequency != "" {
		clock := scheduleClockPattern.FindStringSubmatch(value)
		zone := validScheduleTimezone(value)
		varied := containsAnySchedulePhrase(lower, "varied time", "varying time", "random time", "different time each day")
		if frequency == "daily" && varied && zone != "" {
			if _, err := time.LoadLocation(zone); err == nil {
				return scheduleIntent{kind: scheduleIntentExact, cadenceType: "daily", timeOfDay: "00:00", timezone: zone, jitterSeconds: 24*60*60 - 1}
			}
		}
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

	if containsAnySchedulePhrase(lower,
		"manual objective", "manual execution", "run manually", "only manually", "on demand", "when asked",
	) {
		return scheduleIntent{kind: scheduleIntentManual}
	}

	if containsAnyScheduleWord(lower, "recurring", "recurrently", "regularly", "periodically", "scheduled", "schedule", "cadence", "continuously", "continuous") {
		return scheduleIntent{kind: scheduleIntentAmbiguous}
	}
	return scheduleIntent{kind: scheduleIntentAbsent}
}

func boundedScheduleOccurrences(value string, intervalSeconds int64) int64 {
	if match := scheduleOccurrencePattern.FindStringSubmatch(value); len(match) == 3 {
		count, _ := strconv.ParseInt(match[1], 10, 64)
		if count > 0 && count <= 1_000_000 {
			return count
		}
	}
	if intervalSeconds <= 0 {
		return 0
	}
	match := scheduleDurationPattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return 0
	}
	count, _ := strconv.ParseInt(match[1], 10, 64)
	multipliers := map[string]int64{"second": 1, "seconds": 1, "minute": 60, "minutes": 60, "hour": 3600, "hours": 3600, "day": 86400, "days": 86400}
	duration := count * multipliers[strings.ToLower(match[2])]
	if count <= 0 || duration <= 0 || duration%intervalSeconds != 0 || duration/intervalSeconds > 1_000_000 {
		return 0
	}
	return duration / intervalSeconds
}

func validScheduleTimezone(value string) string {
	for _, candidate := range scheduleTimezonePattern.FindAllString(value, -1) {
		if _, err := time.LoadLocation(candidate); err == nil {
			return candidate
		}
	}
	return ""
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

func upsertScheduleIntentQuestion(generated *GenerationResponse, request GenerateRequest) {
	removeScheduleIntentQuestion(generated)
	questionID := nextScheduleIntentQuestionID(request.Refinement)
	prompt := "When should this work run? Choose “on demand”, specify an exact time such as “daily at 09:00 UTC”, or a bounded varied window such as “daily at a varied time UTC”."
	whyNeeded := "This source is ready, but OpenSeal needs to know whether the Agent should wait for you or run automatically."
	if questionID != scheduleIntentQuestionID {
		prompt = "The previous timing answer cannot be compiled exactly. Specify an exact portable cadence such as “every hour”, “daily at 09:00 UTC”, or `cron \"0 0 9 * * *\" timezone UTC jitter 300 seconds`; otherwise choose “on demand”."
		whyNeeded = "Recurring work is not activated from prose that the scheduler cannot represent exactly. A precise cadence is required before OpenSeal can create the Objective-owned Runbook trigger."
	}
	generated.UnresolvedQuestions = append(generated.UnresolvedQuestions, RefinementQuestion{
		ID:        questionID,
		Category:  RefinementCategoryPolicy,
		Prompt:    prompt,
		WhyNeeded: whyNeeded,
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

func sourceCapabilityRequiresDurableAction(request GenerateRequest) bool {
	switch parseScheduleIntent(scheduleIntentAuthorityText(request)).kind {
	case scheduleIntentAmbiguous, scheduleIntentExact:
		return true
	default:
		return false
	}
}

func removeScheduleIntentQuestion(generated *GenerationResponse) {
	questions := generated.UnresolvedQuestions[:0]
	for _, question := range generated.UnresolvedQuestions {
		if !isScheduleIntentQuestionID(question.ID) {
			questions = append(questions, question)
		}
	}
	generated.UnresolvedQuestions = questions
}

func hasScheduleIntentQuestion(questions []RefinementQuestion) bool {
	for _, question := range questions {
		if isScheduleIntentQuestionID(question.ID) {
			return true
		}
	}
	return false
}

func isScheduleIntentQuestionID(id string) bool {
	id = strings.TrimSpace(id)
	return id == scheduleIntentQuestionID || strings.HasPrefix(id, scheduleIntentClarificationQuestionIDPrefix)
}

func nextScheduleIntentQuestionID(refinement *RefinementContext) string {
	if refinement == nil {
		return scheduleIntentQuestionID
	}
	answered := map[string]bool{}
	for _, answer := range refinement.Answers {
		answered[strings.TrimSpace(answer.QuestionID)] = true
	}
	if !answered[scheduleIntentQuestionID] {
		return scheduleIntentQuestionID
	}
	for revision := 1; ; revision++ {
		id := fmt.Sprintf("%s%d", scheduleIntentClarificationQuestionIDPrefix, revision)
		if !answered[id] {
			return id
		}
	}
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

func removeAllCandidateSchedules(candidate *WorkforceCandidate) bool {
	changed := false
	for _, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		for id, trigger := range definition.Runbook.Triggers {
			if trigger.Kind == runbook.TriggerSchedule {
				delete(definition.Runbook.Triggers, id)
				changed = true
			}
		}
	}
	return changed
}

func restoreExistingAndRemoveNewSchedules(candidate *WorkforceCandidate, existing *WorkforceCandidate) bool {
	changed := false
	previous := map[string]*agent.AgentDefinition{}
	if existing != nil {
		previous = candidateAgentsByID(existing)
	}
	for _, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		oldSchedules := scheduleTriggers(previous[definition.ID])
		for id, trigger := range definition.Runbook.Triggers {
			if trigger.Kind == runbook.TriggerSchedule {
				delete(definition.Runbook.Triggers, id)
				changed = true
			}
		}
		for id, trigger := range oldSchedules {
			definition.Runbook.Triggers[id] = trigger
		}
	}
	return changed
}

func validateAuthorizedCandidateSchedules(candidate, existing *WorkforceCandidate, intent scheduleIntent) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	matched := false
	for agentIndex, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		for id, trigger := range definition.Runbook.Triggers {
			if trigger.Kind != runbook.TriggerSchedule {
				continue
			}
			if scheduleMatchesIntent(trigger.Schedule, intent) {
				matched = true
				continue
			}
			issues = append(issues, issue(fmt.Sprintf("agents[%d].runbook.triggers.%s.schedule", agentIndex, id), "schedule_intent_mismatch", "Generated Runbook trigger does not match the exact schedule authorized by the request"))
		}
	}
	if !matched {
		issues = append(issues, issue("candidate", "requested_schedule_missing", "The request authorizes an exact recurring schedule, but no Runbook trigger preserves it"))
	}
	return issues
}

func scheduleMatchesIntent(schedule *runbook.Schedule, intent scheduleIntent) bool {
	if schedule == nil {
		return false
	}
	expected, err := scheduleForIntent(intent)
	return err == nil && normalizeWhitespace(schedule.Cron) == expected.Cron &&
		strings.TrimSpace(schedule.Timezone) == expected.Timezone && schedule.JitterSeconds == expected.JitterSeconds &&
		schedule.MaximumOccurrences == expected.MaximumOccurrences
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func scheduleForIntent(intent scheduleIntent) (*runbook.Schedule, error) {
	timezone := strings.TrimSpace(intent.timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	cronExpression := strings.TrimSpace(intent.cronExpression)
	switch intent.cadenceType {
	case "interval":
		var err error
		cronExpression, err = authoringIntervalCron(intent.intervalSeconds)
		if err != nil {
			return nil, err
		}
	case "daily":
		hour, minute, err := scheduleClockParts(intent.timeOfDay)
		if err != nil {
			return nil, err
		}
		cronExpression = fmt.Sprintf("0 %d %d * * *", minute, hour)
	case "weekly":
		hour, minute, err := scheduleClockParts(intent.timeOfDay)
		if err != nil {
			return nil, err
		}
		weekday := map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}[strings.ToLower(intent.dayOfWeek)]
		cronExpression = fmt.Sprintf("0 %d %d * * %d", minute, hour, weekday)
	case "cron":
	default:
		return nil, fmt.Errorf("unsupported schedule intent %q", intent.cadenceType)
	}
	result := &runbook.Schedule{Cron: cronExpression, Timezone: timezone, JitterSeconds: intent.jitterSeconds, MaximumOccurrences: intent.maximumOccurrences}
	return result, result.Validate()
}

func scheduleClockParts(value string) (int, int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("schedule clock must be HH:MM")
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil {
		return 0, 0, fmt.Errorf("schedule clock must be HH:MM")
	}
	return hour, minute, nil
}

func authoringIntervalCron(seconds int64) (string, error) {
	switch {
	case seconds > 0 && seconds < 60 && 60%seconds == 0:
		return fmt.Sprintf("*/%d * * * * *", seconds), nil
	case seconds >= 60 && seconds < 3600 && seconds%60 == 0 && 60%(seconds/60) == 0:
		return fmt.Sprintf("0 */%d * * * *", seconds/60), nil
	case seconds >= 3600 && seconds < 86400 && seconds%3600 == 0 && 24%(seconds/3600) == 0:
		return fmt.Sprintf("0 0 */%d * * *", seconds/3600), nil
	case seconds == 86400:
		return "0 0 0 * * *", nil
	default:
		return "", fmt.Errorf("interval %d seconds is not representable by the portable cron trigger", seconds)
	}
}

func scheduleTriggers(definition *agent.AgentDefinition) map[string]runbook.Trigger {
	result := map[string]runbook.Trigger{}
	if definition != nil && definition.Runbook != nil {
		for id, trigger := range definition.Runbook.Triggers {
			if trigger.Kind == runbook.TriggerSchedule {
				result[id] = trigger
			}
		}
	}
	return result
}
