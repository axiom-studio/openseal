package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// RoutineEventKind names the source family of an event that can start a routine.
type RoutineEventKind string

const (
	RoutineEventSlackMessage    RoutineEventKind = "slack.message"
	RoutineEventTelegramMessage RoutineEventKind = "telegram.message"
	RoutineEventGitHub          RoutineEventKind = "github.event"
	RoutineEventGmailMessage    RoutineEventKind = "gmail.message"
	RoutineEventCalendar        RoutineEventKind = "calendar.event"
	RoutineEventWebhook         RoutineEventKind = "webhook"
)

// Valid reports whether the kind is one of the known kinds.
func (k RoutineEventKind) Valid() bool {
	switch k {
	case RoutineEventSlackMessage, RoutineEventTelegramMessage, RoutineEventGitHub,
		RoutineEventGmailMessage, RoutineEventCalendar, RoutineEventWebhook:
		return true
	}
	return false
}

var (
	ErrInvalidRoutineEvent        = errors.New("invalid routine event")
	ErrInvalidRoutineEventTrigger = errors.New("invalid routine event trigger")
)

// Limits of the RoutineEvent wire contract.
const (
	RoutineEventIDMaxBytes          = 256
	RoutineEventSourceRefMaxBytes   = 512
	RoutineEventSubjectMaxBytes     = 256
	RoutineEventSummaryMaxBytes     = 280
	RoutineEventMaxFieldKeys        = 32
	RoutineEventMaxFieldValues      = 16
	RoutineEventFieldValueMaxBytes  = 1 << 10
	RoutineEventTextFieldMaxBytes   = 4 << 10
	RoutineEventPayloadMaxBytes     = 256 << 10
	RoutineEventDefaultMaxPerHour   = 20
	RoutineEventMaxPerHourLimit     = 60
	RoutineCalendarMaxMinutesBefore = 1440
)

// RoutineEvent is the one wire contract every source hands to the matcher.
type RoutineEvent struct {
	Scope      Scope               `json:"scope"`
	ID         string              `json:"id"`
	Kind       RoutineEventKind    `json:"kind"`
	SourceRef  string              `json:"sourceRef"`
	OccurredAt time.Time           `json:"occurredAt"`
	Subject    string              `json:"subject,omitempty"`
	Summary    string              `json:"summary"`
	Fields     map[string][]string `json:"fields,omitempty"`
	Payload    json.RawMessage     `json:"payload,omitempty"`
	Test       bool                `json:"test,omitempty"`
	TestBy     string              `json:"testBy,omitempty"`
}

// ValidRoutineEventID reports whether id is a routine event ID as the kernel
// stores it: a plain identifier of at most RoutineEventIDMaxBytes bytes with
// no surrounding space. Hosts use it to keep a producer's ID unchanged.
func ValidRoutineEventID(id string) bool {
	return strings.TrimSpace(id) == id && IsValidOpaqueIdentifier(id, RoutineEventIDMaxBytes)
}

func routineEventError(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidRoutineEvent, fmt.Sprintf(format, args...))
}

// Validate checks sizes, kind, IDs, fields and payload of an event.
func (e *RoutineEvent) Validate() error {
	if e == nil {
		return routineEventError("event is required")
	}
	if err := e.Scope.Validate(); err != nil {
		return routineEventError("scope is required")
	}
	if !ValidRoutineEventID(e.ID) {
		return routineEventError("id must be a plain identifier of at most %d bytes", RoutineEventIDMaxBytes)
	}
	if !e.Kind.Valid() {
		return routineEventError("unknown kind %q", e.Kind)
	}
	if err := ValidateRoutineSourceRef(e.Kind, e.SourceRef); err != nil {
		return routineEventError("%s", err.Error())
	}
	if e.OccurredAt.IsZero() {
		return routineEventError("occurredAt is required")
	}
	if len(e.Subject) > RoutineEventSubjectMaxBytes || !utf8.ValidString(e.Subject) {
		return routineEventError("subject must be valid text of at most %d bytes", RoutineEventSubjectMaxBytes)
	}
	summary := strings.TrimSpace(e.Summary)
	if summary == "" || len(e.Summary) > RoutineEventSummaryMaxBytes || !utf8.ValidString(e.Summary) || strings.ContainsAny(e.Summary, "\r\n") {
		return routineEventError("summary must be one line of at most %d bytes", RoutineEventSummaryMaxBytes)
	}
	if err := validateRoutineEventFields(e.Kind, e.Fields); err != nil {
		return err
	}
	if len(e.Payload) > 0 {
		if len(e.Payload) > RoutineEventPayloadMaxBytes {
			return routineEventError("payload is larger than %d bytes", RoutineEventPayloadMaxBytes)
		}
		trimmed := bytes.TrimSpace(e.Payload)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return routineEventError("payload must be a JSON object")
		}
	}
	if e.Test {
		if strings.TrimSpace(e.TestBy) == "" {
			return routineEventError("a test event needs testBy")
		}
	} else if e.TestBy != "" {
		return routineEventError("testBy is only set on test events")
	}
	return nil
}

func validateRoutineEventFields(kind RoutineEventKind, fields map[string][]string) error {
	if len(fields) > RoutineEventMaxFieldKeys {
		return routineEventError("at most %d fields", RoutineEventMaxFieldKeys)
	}
	for key, values := range fields {
		if !routineEventProducedField(kind, key) {
			return routineEventError("field %q is not part of %s events", key, kind)
		}
		if len(values) > RoutineEventMaxFieldValues {
			return routineEventError("field %q has more than %d values", key, RoutineEventMaxFieldValues)
		}
		limit := RoutineEventFieldValueMaxBytes
		if key == "text" {
			limit = RoutineEventTextFieldMaxBytes
		}
		for _, value := range values {
			if len(value) > limit || !utf8.ValidString(value) {
				return routineEventError("a value of field %q is longer than %d bytes", key, limit)
			}
		}
	}
	switch kind {
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		if values, ok := fields[routineEventBotField]; ok {
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				return routineEventError("field bot must be true or false")
			}
		}
	case RoutineEventCalendar:
		if values, ok := fields[routineEventCalendarOnField]; ok {
			if len(values) != 1 {
				return routineEventError("field on must have one value")
			}
			if _, err := parseRoutineCalendarOn(values[0]); err != nil {
				return routineEventError("%s", err.Error())
			}
		}
	}
	return nil
}

// RoutineEventTrigger is ScheduledAgentTask.Event: what starts an event routine.
type RoutineEventTrigger struct {
	Kind               RoutineEventKind        `json:"kind"`
	SourceRef          string                  `json:"sourceRef"`
	SourceLabel        string                  `json:"sourceLabel"`
	Filter             []RoutineEventCondition `json:"filter,omitempty"`
	Calendar           *RoutineCalendarTiming  `json:"calendar,omitempty"`
	IncludeBots        bool                    `json:"includeBots,omitempty"`
	MaxPerHour         int                     `json:"maxPerHour"`
	MaximumOccurrences int64                   `json:"maximumOccurrences,omitempty"`
	Webhook            *RoutineWebhook         `json:"webhook,omitempty"`
}

type RoutineFilterOp string

const (
	RoutineFilterEquals   RoutineFilterOp = "equals"   // case-insensitive, trimmed
	RoutineFilterContains RoutineFilterOp = "contains" // case-insensitive substring
	RoutineFilterOneOf    RoutineFilterOp = "one_of"   // equals any of Values (at most 20)
	RoutineFilterGlob     RoutineFilterOp = "glob"     // '*' wildcard only, case-insensitive, whole value
	RoutineFilterExists   RoutineFilterOp = "exists"   // field has a non-empty value
)

type RoutineEventCondition struct {
	Field  string          `json:"field"`
	Op     RoutineFilterOp `json:"op"`
	Value  string          `json:"value,omitempty"`
	Values []string        `json:"values,omitempty"`
	Not    bool            `json:"not,omitempty"`
	Label  string          `json:"label,omitempty"`
}

// Calendar timings of a calendar routine.
const (
	RoutineCalendarStartsIn = "starts_in"
	RoutineCalendarCreated  = "created"
	RoutineCalendarUpdated  = "updated"
	RoutineCalendarEnded    = "ended"
)

type RoutineCalendarTiming struct {
	On      string `json:"on"`
	Minutes int    `json:"minutes,omitempty"`
}

// EventOn is the value of a calendar event's internal `on` field that this timing matches.
func (t *RoutineCalendarTiming) EventOn() string {
	if t == nil {
		return ""
	}
	if t.On == RoutineCalendarStartsIn {
		return fmt.Sprintf("%s:%d", RoutineCalendarStartsIn, t.Minutes)
	}
	return t.On
}

type RoutineWebhook struct {
	HookID string `json:"hookId"`
}

type RoutinePausedReason string

const (
	RoutinePausedNeedsSignIn   RoutinePausedReason = "needs_sign_in"
	RoutinePausedSourceRemoved RoutinePausedReason = "source_removed"
	RoutinePausedUnverified    RoutinePausedReason = "unverified"
)

// Run.Context keys set on an occurrence by the dispatcher.
const (
	RoutineEventBatchContextKey  = "routineEventBatchId"
	RoutineEventCountContextKey  = "routineEventCount"
	RoutineEventKindContextKey   = "routineEventKind"
	RoutineEventHeaderContextKey = "routineEventHeader"
	RoutineEventTestContextKey   = "routineEventTest"
)

// SourceRef prefixes (SPEC §1.3).
const (
	routineConversationSourcePrefix = "conversation:"
	RoutineCallbackSourcePrefix     = "callback:"
	RoutineGoogleSourcePrefix       = "google:"
	routineHookSourcePrefix         = "hook:"
	routineHookIDPrefix             = "hk_"
	routineHookIDHashLength         = 26
	routineConversationHashLength   = 40
)

var routineHookIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// RoutineConversationSourceRef identifies one Slack or Telegram connection of a deployment.
func RoutineConversationSourceRef(deploymentID, bindingID, adapterID, installationID string) string {
	sum := sha256.Sum256([]byte(deploymentID + "\x00" + bindingID + "\x00" + adapterID + "\x00" + installationID))
	return routineConversationSourcePrefix + hex.EncodeToString(sum[:])[:routineConversationHashLength]
}

// RoutineCallbackSourceRef identifies one callback registration (GitHub).
func RoutineCallbackSourceRef(registrationID string) string {
	return RoutineCallbackSourcePrefix + registrationID
}

// RoutineGoogleSourceRef identifies one person's Google account.
func RoutineGoogleSourceRef(userID, googleSubject string) string {
	return RoutineGoogleSourcePrefix + userID + ":" + googleSubject
}

// HookSourceRef is the SourceRef of a webhook routine.
func HookSourceRef(hookID string) string {
	return routineHookSourcePrefix + hookID
}

// HookID is the stable hook ID of an activation: "hk_" and 26 lower base32 characters of
// sha256(scope kind, scope ID, activation ID), so retries create the same hook.
func HookID(scope Scope, activationID string) string {
	sum := sha256.Sum256([]byte(scope.Kind + "\x00" + scope.ID + "\x00" + activationID))
	return routineHookIDPrefix + routineHookIDEncoding.EncodeToString(sum[:])[:routineHookIDHashLength]
}

// ValidHookID reports whether id has the shape HookID produces.
func ValidHookID(id string) bool {
	if len(id) != len(routineHookIDPrefix)+routineHookIDHashLength || !strings.HasPrefix(id, routineHookIDPrefix) {
		return false
	}
	for _, r := range id[len(routineHookIDPrefix):] {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			return false
		}
	}
	return true
}

func ValidateRoutineSourceRef(kind RoutineEventKind, sourceRef string) error {
	if sourceRef == "" || len(sourceRef) > RoutineEventSourceRefMaxBytes || strings.TrimSpace(sourceRef) != sourceRef || strings.ContainsAny(sourceRef, "\r\n\x00") {
		return fmt.Errorf("sourceRef must be set and at most %d bytes", RoutineEventSourceRefMaxBytes)
	}
	switch kind {
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		hash, ok := strings.CutPrefix(sourceRef, routineConversationSourcePrefix)
		if !ok || len(hash) != routineConversationHashLength || !isLowerHex(hash) {
			return fmt.Errorf("sourceRef of %s must be a conversation source", kind)
		}
	case RoutineEventGitHub:
		id, ok := strings.CutPrefix(sourceRef, RoutineCallbackSourcePrefix)
		if !ok || !IsValidOpaqueIdentifier(id, 256) || strings.TrimSpace(id) != id {
			return fmt.Errorf("sourceRef of %s must be a callback source", kind)
		}
	case RoutineEventGmailMessage, RoutineEventCalendar:
		rest, ok := strings.CutPrefix(sourceRef, RoutineGoogleSourcePrefix)
		user, subject, found := strings.Cut(rest, ":")
		if !ok || !found || strings.TrimSpace(user) == "" || strings.TrimSpace(subject) == "" {
			return fmt.Errorf("sourceRef of %s must be a Google source", kind)
		}
	case RoutineEventWebhook:
		id, ok := strings.CutPrefix(sourceRef, routineHookSourcePrefix)
		if !ok || !ValidHookID(id) {
			return fmt.Errorf("sourceRef of %s must be a hook source", kind)
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
