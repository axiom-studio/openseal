package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Internal fields: producers set them, routines cannot filter on them.
const (
	routineEventBotField        = "bot"
	routineEventCalendarOnField = "on"
	routineEventPayloadPrefix   = "payload."
)

// Limits of a RoutineEventTrigger.
const (
	RoutineEventMaxConditions        = 10
	RoutineEventConditionValueMax    = 512
	RoutineEventConditionMaxValues   = 20
	RoutineEventConditionLabelMax    = 120
	RoutineEventSourceLabelMax       = 160
	RoutineEventPayloadPathMaxBytes  = 256
	RoutineEventPayloadPathMaxLevels = 16
)

// routineEventFilterFields is the filterable vocabulary per kind (SPEC §3.2). Webhook routines may also
// filter on any "payload.<path>".
var routineEventFilterFields = map[RoutineEventKind][]string{
	RoutineEventSlackMessage:    {"channel", "sender", "senderName", "text", "inThread"},
	RoutineEventTelegramMessage: {"chat", "chatTitle", "sender", "senderName", "senderUsername", "text"},
	RoutineEventGitHub:          {"event", "repository", "action", "sender", "title", "labels", "branch", "conclusion"},
	RoutineEventGmailMessage:    {"from", "fromUnverified", "to", "cc", "subject", "label", "snippet"},
	RoutineEventCalendar:        {"title", "organizer", "attendee", "location", "calendar"},
	RoutineEventWebhook:         {"contentType", "githubEvent"},
}

// RoutineEventFilterFields returns the fields a routine of this kind can filter on, in a fixed order.
func RoutineEventFilterFields(kind RoutineEventKind) []string {
	return append([]string(nil), routineEventFilterFields[kind]...)
}

func routineEventFilterableField(kind RoutineEventKind, field string) bool {
	if kind == RoutineEventWebhook && strings.HasPrefix(field, routineEventPayloadPrefix) {
		return validRoutinePayloadPath(strings.TrimPrefix(field, routineEventPayloadPrefix))
	}
	for _, name := range routineEventFilterFields[kind] {
		if name == field {
			return true
		}
	}
	return false
}

// routineEventProducedField reports whether a producer may set this field on an event of the kind.
func routineEventProducedField(kind RoutineEventKind, field string) bool {
	switch {
	case field == routineEventBotField:
		return kind == RoutineEventSlackMessage || kind == RoutineEventTelegramMessage
	case field == routineEventCalendarOnField:
		return kind == RoutineEventCalendar
	case strings.HasPrefix(field, routineEventPayloadPrefix):
		return false
	}
	return routineEventFilterableField(kind, field)
}

func validRoutinePayloadPath(path string) bool {
	if path == "" || len(path) > RoutineEventPayloadPathMaxBytes || !utf8.ValidString(path) {
		return false
	}
	segments := strings.Split(path, ".")
	if len(segments) > RoutineEventPayloadPathMaxLevels {
		return false
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			return false
		}
	}
	return true
}

func parseRoutineCalendarOn(value string) (RoutineCalendarTiming, error) {
	switch value {
	case RoutineCalendarCreated, RoutineCalendarUpdated, RoutineCalendarEnded:
		return RoutineCalendarTiming{On: value}, nil
	}
	if rest, ok := strings.CutPrefix(value, RoutineCalendarStartsIn+":"); ok {
		minutes, err := strconv.Atoi(rest)
		if err == nil && minutes >= 1 && minutes <= RoutineCalendarMaxMinutesBefore && strconv.Itoa(minutes) == rest {
			return RoutineCalendarTiming{On: RoutineCalendarStartsIn, Minutes: minutes}, nil
		}
	}
	return RoutineCalendarTiming{}, fmt.Errorf("calendar on value %q is not created, updated, ended or starts_in:<minutes>", value)
}

func routineTriggerError(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidRoutineEventTrigger, fmt.Sprintf(format, args...))
}

// Validate checks a trigger at create time: vocabulary per kind, timing, webhook, limits and the
// Slack/Telegram rule that the routine names its channels or chats (E14).
func (t *RoutineEventTrigger) Validate() error {
	if t == nil {
		return routineTriggerError("trigger is required")
	}
	if !t.Kind.Valid() {
		return routineTriggerError("unknown kind %q", t.Kind)
	}
	if err := ValidateRoutineSourceRef(t.Kind, t.SourceRef); err != nil {
		return routineTriggerError("%s", err.Error())
	}
	label := strings.TrimSpace(t.SourceLabel)
	if label == "" || len(t.SourceLabel) > RoutineEventSourceLabelMax || !utf8.ValidString(t.SourceLabel) || strings.ContainsAny(t.SourceLabel, "\r\n") {
		return routineTriggerError("sourceLabel must be one line of at most %d bytes", RoutineEventSourceLabelMax)
	}
	if len(t.Filter) > RoutineEventMaxConditions {
		return routineTriggerError("at most %d filter conditions", RoutineEventMaxConditions)
	}
	for index := range t.Filter {
		if err := t.Filter[index].validate(t.Kind); err != nil {
			return routineTriggerError("filter %d: %s", index+1, err.Error())
		}
	}
	if t.Kind == RoutineEventCalendar {
		if t.Calendar == nil {
			return routineTriggerError("a calendar routine needs calendar timing")
		}
		switch t.Calendar.On {
		case RoutineCalendarStartsIn:
			if t.Calendar.Minutes < 1 || t.Calendar.Minutes > RoutineCalendarMaxMinutesBefore {
				return routineTriggerError("calendar minutes must be between 1 and %d", RoutineCalendarMaxMinutesBefore)
			}
		case RoutineCalendarCreated, RoutineCalendarUpdated, RoutineCalendarEnded:
			if t.Calendar.Minutes != 0 {
				return routineTriggerError("calendar minutes only go with starts_in")
			}
		default:
			return routineTriggerError("calendar on must be starts_in, created, updated or ended")
		}
	} else if t.Calendar != nil {
		return routineTriggerError("calendar timing only goes with calendar routines")
	}
	if t.IncludeBots && t.Kind != RoutineEventSlackMessage && t.Kind != RoutineEventTelegramMessage {
		return routineTriggerError("includeBots only goes with Slack and Telegram routines")
	}
	if t.MaxPerHour < 1 || t.MaxPerHour > RoutineEventMaxPerHourLimit {
		return routineTriggerError("maxPerHour must be between 1 and %d", RoutineEventMaxPerHourLimit)
	}
	if t.MaximumOccurrences < 0 {
		return routineTriggerError("maximumOccurrences cannot be negative")
	}
	if t.Kind == RoutineEventWebhook {
		if t.Webhook == nil || !ValidHookID(t.Webhook.HookID) {
			return routineTriggerError("a webhook routine needs a hook ID")
		}
		if t.SourceRef != HookSourceRef(t.Webhook.HookID) {
			return routineTriggerError("sourceRef must be the routine's hook")
		}
	} else if t.Webhook != nil {
		return routineTriggerError("webhook only goes with webhook routines")
	}
	if field := RoutineEventPlaceField(t.Kind); field != "" && t.placeConditionIndex() < 0 {
		return routineTriggerError("name the %ss this routine listens to with a %q condition (equals or one_of)", field, field)
	}
	return nil
}

func (c *RoutineEventCondition) validate(kind RoutineEventKind) error {
	if c.Field == routineEventBotField || c.Field == routineEventCalendarOnField {
		return fmt.Errorf("field %q cannot be filtered on", c.Field)
	}
	if !routineEventFilterableField(kind, c.Field) {
		return fmt.Errorf("field %q is not part of %s events", c.Field, kind)
	}
	if len(c.Label) > RoutineEventConditionLabelMax || !utf8.ValidString(c.Label) || strings.ContainsAny(c.Label, "\r\n") {
		return fmt.Errorf("label must be one line of at most %d bytes", RoutineEventConditionLabelMax)
	}
	validValue := func(value string) bool {
		return strings.TrimSpace(value) != "" && len(value) <= RoutineEventConditionValueMax && utf8.ValidString(value)
	}
	switch c.Op {
	case RoutineFilterEquals, RoutineFilterContains, RoutineFilterGlob:
		if !validValue(c.Value) || len(c.Values) > 0 {
			return fmt.Errorf("%s needs one value of at most %d bytes", c.Op, RoutineEventConditionValueMax)
		}
	case RoutineFilterOneOf:
		if c.Value != "" || len(c.Values) == 0 || len(c.Values) > RoutineEventConditionMaxValues {
			return fmt.Errorf("one_of needs between 1 and %d values", RoutineEventConditionMaxValues)
		}
		for _, value := range c.Values {
			if !validValue(value) {
				return fmt.Errorf("one_of values must be set and at most %d bytes", RoutineEventConditionValueMax)
			}
		}
	case RoutineFilterExists:
		if c.Value != "" || len(c.Values) > 0 {
			return fmt.Errorf("exists takes no value")
		}
	default:
		return fmt.Errorf("unknown op %q", c.Op)
	}
	return nil
}

// RoutineEventPlaceField is the field a Slack or Telegram routine must name (E14).
func RoutineEventPlaceField(kind RoutineEventKind) string {
	switch kind {
	case RoutineEventSlackMessage:
		return "channel"
	case RoutineEventTelegramMessage:
		return "chat"
	}
	return ""
}

// Matches reports whether the event starts this routine (SPEC §3.2). It does not check scope or owner;
// the matcher does that. Test events skip the filter and are not passed here.
func (t *RoutineEventTrigger) Matches(e *RoutineEvent) bool {
	if t == nil || e == nil || e.Kind != t.Kind || e.SourceRef != t.SourceRef {
		return false
	}
	switch t.Kind {
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		if !t.IncludeBots {
			for _, value := range e.Fields[routineEventBotField] {
				if strings.EqualFold(strings.TrimSpace(value), "true") {
					return false
				}
			}
		}
	case RoutineEventCalendar:
		on := e.Fields[routineEventCalendarOnField]
		if t.Calendar == nil || len(on) != 1 || on[0] != t.Calendar.EventOn() {
			return false
		}
	}
	var payload routinePayloadValues
	for index := range t.Filter {
		condition := &t.Filter[index]
		var values []string
		if path, ok := strings.CutPrefix(condition.Field, routineEventPayloadPrefix); ok && t.Kind == RoutineEventWebhook {
			values = payload.lookup(e.Payload, path)
		} else {
			values = e.Fields[condition.Field]
		}
		if !condition.holds(values) {
			return false
		}
	}
	return true
}

func (c *RoutineEventCondition) holds(values []string) bool {
	found := false
	for _, value := range values {
		if c.satisfiedBy(value) {
			found = true
			break
		}
	}
	if c.Not {
		return !found
	}
	return found
}

func (c *RoutineEventCondition) satisfiedBy(value string) bool {
	switch c.Op {
	case RoutineFilterEquals:
		return routineFilterEqual(value, c.Value)
	case RoutineFilterContains:
		return strings.Contains(strings.ToLower(value), strings.ToLower(c.Value))
	case RoutineFilterOneOf:
		for _, candidate := range c.Values {
			if routineFilterEqual(value, candidate) {
				return true
			}
		}
		return false
	case RoutineFilterGlob:
		return routineGlobMatch(strings.ToLower(strings.TrimSpace(c.Value)), strings.ToLower(strings.TrimSpace(value)))
	case RoutineFilterExists:
		return strings.TrimSpace(value) != ""
	}
	return false
}

func routineFilterEqual(value, want string) bool {
	return strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want))
}

// routineGlobMatch matches the whole value against a pattern where '*' matches any run of bytes.
func routineGlobMatch(pattern, value string) bool {
	p, v := 0, 0
	star, mark := -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case p < len(pattern) && pattern[p] == value[v]:
			p++
			v++
		case star >= 0:
			p = star + 1
			mark++
			v = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// routinePayloadValues decodes a webhook payload once per Matches call.
type routinePayloadValues struct {
	decoded bool
	root    interface{}
}

// lookup walks a dot path (array indexes are path segments) and returns the values found there:
// a scalar gives one value, an array of scalars gives one value per item, an object or nested array
// gives its compact JSON, null or a missing path gives none.
func (p *routinePayloadValues) lookup(raw json.RawMessage, path string) []string {
	if !p.decoded {
		p.decoded = true
		if len(raw) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var root interface{}
			if decoder.Decode(&root) == nil {
				p.root = root
			}
		}
	}
	node := p.root
	for _, segment := range strings.Split(path, ".") {
		switch current := node.(type) {
		case map[string]interface{}:
			next, ok := current[segment]
			if !ok {
				return nil
			}
			node = next
		case []interface{}:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(current) {
				return nil
			}
			node = current[index]
		default:
			return nil
		}
	}
	if items, ok := node.([]interface{}); ok {
		values := make([]string, 0, len(items))
		for _, item := range items {
			if value, ok := routinePayloadScalar(item); ok {
				values = append(values, value)
			}
		}
		return values
	}
	if value, ok := routinePayloadScalar(node); ok {
		return []string{value}
	}
	return nil
}

func routinePayloadScalar(node interface{}) (string, bool) {
	switch value := node.(type) {
	case nil:
		return "", false
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case bool:
		return strconv.FormatBool(value), true
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
}
