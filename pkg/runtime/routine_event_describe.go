package runtime

import (
	"fmt"
	"strings"
)

// routineFieldWording says how one filter field reads in a sentence. A field with a preposition reads
// "from X" / "not from X"; one without reads "where the <noun> is X".
type routineFieldWording struct {
	noun        string
	preposition string
}

var routineFieldWordings = map[RoutineEventKind]map[string]routineFieldWording{
	RoutineEventSlackMessage: {
		"channel":    {noun: "channel", preposition: "in"},
		"sender":     {noun: "sender", preposition: "from"},
		"senderName": {noun: "sender", preposition: "from"},
		"text":       {noun: "message"},
	},
	RoutineEventTelegramMessage: {
		"chat":           {noun: "chat", preposition: "in"},
		"chatTitle":      {noun: "chat title"},
		"sender":         {noun: "sender", preposition: "from"},
		"senderName":     {noun: "sender", preposition: "from"},
		"senderUsername": {noun: "sender", preposition: "from"},
		"text":           {noun: "message"},
	},
	RoutineEventGitHub: {
		"event":      {noun: "event"},
		"repository": {noun: "repository", preposition: "in"},
		"action":     {noun: "action"},
		"sender":     {noun: "author", preposition: "by"},
		"title":      {noun: "title"},
		"labels":     {noun: "label", preposition: "labeled"},
		"branch":     {noun: "branch", preposition: "on branch"},
		"conclusion": {noun: "result"},
	},
	RoutineEventGmailMessage: {
		"from":           {noun: "sender", preposition: "from"},
		"fromUnverified": {noun: "unverified sender", preposition: "from"},
		"to":             {noun: "recipient", preposition: "sent to"},
		"cc":             {noun: "copied address", preposition: "copying"},
		"subject":        {noun: "subject"},
		"label":          {noun: "label", preposition: "labeled"},
		"snippet":        {noun: "preview"},
	},
	RoutineEventCalendar: {
		"title":     {noun: "title"},
		"organizer": {noun: "organizer", preposition: "organized by"},
		"attendee":  {noun: "guest", preposition: "with"},
		"location":  {noun: "location"},
		"calendar":  {noun: "calendar"},
	},
	RoutineEventWebhook: {
		"contentType": {noun: "content type"},
		"githubEvent": {noun: "GitHub event"},
	},
}

// routineGitHubEventPhrases words the GitHub event types the callback adapter produces.
var routineGitHubEventPhrases = map[string]string{
	"issues.opened":                 "an issue is opened",
	"issues.reopened":               "an issue is reopened",
	"issues.edited":                 "an issue is edited",
	"issues.closed":                 "an issue is closed",
	"issue_comment.created":         "someone comments on an issue or pull request",
	"issue_comment.edited":          "a comment on an issue or pull request is edited",
	"pull_request.opened":           "a pull request is opened",
	"pull_request.reopened":         "a pull request is reopened",
	"pull_request.synchronize":      "a pull request gets new commits",
	"pull_request.ready_for_review": "a pull request is ready for review",
	"pull_request.closed":           "a pull request is closed",
	"workflow_run.requested":        "a workflow run is requested",
	"workflow_run.in_progress":      "a workflow run starts",
	"workflow_run.completed":        "a workflow run finishes",
}

// DescribeRoutineEventTrigger is the brief card's "When" sentence: plain English built from the trigger,
// labels instead of raw IDs, no event type strings and no limit (that has its own row).
func DescribeRoutineEventTrigger(t *RoutineEventTrigger) string {
	if t == nil {
		return ""
	}
	label := strings.TrimSpace(t.SourceLabel)
	used := map[int]bool{}
	var head string
	switch t.Kind {
	case RoutineEventGmailMessage:
		head = "When a new email arrives in " + label
	case RoutineEventCalendar:
		head = describeRoutineCalendarTiming(t.Calendar, label)
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		head = "When someone posts in " + label
		if index := t.placeConditionIndex(); index >= 0 {
			used[index] = true
			head = "When someone posts in " + routineConditionDisplay(&t.Filter[index], false) + " (" + label + ")"
		}
	case RoutineEventGitHub:
		activity := "there is new activity"
		if index, phrase := t.githubEventPhrase(); index >= 0 {
			used[index] = true
			activity = phrase
		}
		where := label
		if index := t.positiveConditionIndex("repository"); index >= 0 {
			used[index] = true
			where = routineConditionDisplay(&t.Filter[index], false) + " (" + label + ")"
		}
		head = "When " + activity + " in " + where
	case RoutineEventWebhook:
		head = "When a request arrives at this routine's address"
	default:
		head = "When an event arrives from " + label
	}
	var clauses []string
	whereUsed := false
	for index := range t.Filter {
		if used[index] {
			continue
		}
		clause := describeRoutineCondition(t.Kind, &t.Filter[index])
		if strings.HasPrefix(clause, "where ") {
			if whereUsed {
				clause = "and " + strings.TrimPrefix(clause, "where ")
			}
			whereUsed = true
		}
		clauses = append(clauses, clause)
	}
	if len(clauses) == 0 {
		return head
	}
	return head + " " + strings.Join(clauses, " ")
}

func describeRoutineCalendarTiming(timing *RoutineCalendarTiming, label string) string {
	if timing == nil {
		return "When an event changes in " + label
	}
	switch timing.On {
	case RoutineCalendarStartsIn:
		return routineMinutesText(timing.Minutes) + " before each event in " + label
	case RoutineCalendarCreated:
		return "When an event is added to " + label
	case RoutineCalendarUpdated:
		return "When an event changes in " + label
	case RoutineCalendarEnded:
		return "When an event ends in " + label
	}
	return "When an event changes in " + label
}

func routineMinutesText(minutes int) string {
	plural := func(count int, unit string) string {
		if count == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", count, unit)
	}
	if minutes >= 60 && minutes%60 == 0 {
		return plural(minutes/60, "hour")
	}
	return plural(minutes, "minute")
}

func (t *RoutineEventTrigger) placeConditionIndex() int {
	field := routineEventPlaceField(t.Kind)
	if field == "" {
		return -1
	}
	return t.positiveConditionIndex(field)
}

// positiveConditionIndex finds the first equals/one_of condition on field without Not.
func (t *RoutineEventTrigger) positiveConditionIndex(field string) int {
	for index := range t.Filter {
		condition := &t.Filter[index]
		if condition.Field == field && !condition.Not && (condition.Op == RoutineFilterEquals || condition.Op == RoutineFilterOneOf) {
			return index
		}
	}
	return -1
}

// githubEventPhrase words a positive event condition when every value is a known GitHub event type.
func (t *RoutineEventTrigger) githubEventPhrase() (int, string) {
	index := t.positiveConditionIndex("event")
	if index < 0 {
		return -1, ""
	}
	condition := &t.Filter[index]
	values := condition.Values
	if condition.Op == RoutineFilterEquals {
		values = []string{condition.Value}
	}
	phrases := make([]string, 0, len(values))
	for _, value := range values {
		phrase, ok := routineGitHubEventPhrases[strings.ToLower(strings.TrimSpace(value))]
		if !ok {
			return -1, ""
		}
		phrases = append(phrases, phrase)
	}
	return index, routineJoinOr(phrases)
}

func describeRoutineCondition(kind RoutineEventKind, condition *RoutineEventCondition) string {
	wording, ok := routineFieldWordings[kind][condition.Field]
	if !ok {
		path := strings.TrimPrefix(condition.Field, routineEventPayloadPrefix)
		wording = routineFieldWording{noun: "field " + path}
	}
	if kind == RoutineEventSlackMessage && condition.Field == "inThread" && condition.Op == RoutineFilterEquals {
		inThread := routineFilterEqual(condition.Value, "true")
		if condition.Not {
			inThread = !inThread
		}
		if inThread {
			return "as a reply in a thread"
		}
		return "and it isn't a reply in a thread"
	}
	suffix := ""
	if kind == RoutineEventGmailMessage && condition.Field == "fromUnverified" {
		suffix = " (sender not verified)"
	}
	switch condition.Op {
	case RoutineFilterContains:
		if condition.Not {
			return "without " + routineConditionDisplay(condition, true) + " in the " + wording.noun
		}
		return "with " + routineConditionDisplay(condition, true) + " in the " + wording.noun
	case RoutineFilterExists:
		article := "a "
		if strings.ContainsAny(wording.noun[:1], "aeiou") {
			article = "an "
		}
		if condition.Not {
			return "without " + article + wording.noun
		}
		return "with " + article + wording.noun
	}
	display := routineConditionDisplay(condition, false)
	if wording.preposition != "" {
		if condition.Not {
			return "not " + wording.preposition + " " + display + suffix
		}
		return wording.preposition + " " + display + suffix
	}
	verb := "is"
	if condition.Op == RoutineFilterGlob {
		verb = "matches"
	}
	if condition.Not {
		verb += " not"
	}
	return "where the " + wording.noun + " " + verb + " " + display + suffix
}

// routineConditionDisplay is the condition's label, or its value(s) when it has none.
func routineConditionDisplay(condition *RoutineEventCondition, quote bool) string {
	if label := strings.TrimSpace(condition.Label); label != "" {
		if quote {
			return "“" + label + "”"
		}
		return label
	}
	values := condition.Values
	if condition.Op != RoutineFilterOneOf {
		values = []string{condition.Value}
	}
	shown := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if quote {
			value = "“" + value + "”"
		}
		shown = append(shown, value)
	}
	return routineJoinOr(shown)
}

func routineJoinOr(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// DescribeRoutineEventTriggerAudience is the brief card's plain "who can start it" line (whoCanTrigger).
func DescribeRoutineEventTriggerAudience(t *RoutineEventTrigger) string {
	if t == nil {
		return ""
	}
	label := strings.TrimSpace(t.SourceLabel)
	switch t.Kind {
	case RoutineEventGmailMessage:
		return "anyone who emails you (only verified senders match a sender condition)"
	case RoutineEventCalendar:
		return "anyone who sends you an invitation"
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		if index := t.placeConditionIndex(); index >= 0 {
			return "anyone who posts in " + routineConditionDisplay(&t.Filter[index], false)
		}
		return "anyone who posts in " + label
	case RoutineEventGitHub:
		if index := t.positiveConditionIndex("repository"); index >= 0 {
			return "activity in " + routineConditionDisplay(&t.Filter[index], false)
		}
		return "activity in " + label
	case RoutineEventWebhook:
		return "anyone with this routine's secret or private address"
	}
	return ""
}

// DescribeRoutineEventTriggerLimit is the "At most" row and the list status line.
func DescribeRoutineEventTriggerLimit(t *RoutineEventTrigger) string {
	if t == nil {
		return ""
	}
	times := fmt.Sprintf("%d times", t.MaxPerHour)
	if t.MaxPerHour == 1 {
		times = "once"
	}
	return fmt.Sprintf("At most %s an hour. More %s than that wait and go into the next run together.", times, routineEventNoun(t.Kind, true))
}

// routineEventNoun is "email", "message" or "event" for a kind.
func routineEventNoun(kind RoutineEventKind, plural bool) string {
	noun := "event"
	switch kind {
	case RoutineEventGmailMessage:
		noun = "email"
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		noun = "message"
	}
	if plural {
		return noun + "s"
	}
	return noun
}
