package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validRoutineTestEvent() *RoutineEvent {
	return &RoutineEvent{
		Scope:      Scope{Kind: "tenant", ID: "t1"},
		ID:         "gmail:18c2f",
		Kind:       RoutineEventGmailMessage,
		SourceRef:  RoutineGoogleSourceRef("user-1", "1093"),
		OccurredAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
		Subject:    "thread-1",
		Summary:    "New email from Ana Ruiz: Invoice #4411",
		Fields:     map[string][]string{"from": {"ana@acme.com", "Ana Ruiz <ana@acme.com>"}, "subject": {"Invoice #4411"}},
		Payload:    json.RawMessage(`{"messageId":"18c2f"}`),
	}
}

func TestRoutineEventValidate(t *testing.T) {
	if err := validRoutineTestEvent().Validate(); err != nil {
		t.Fatalf("valid event: %v", err)
	}
	cases := map[string]func(e *RoutineEvent){
		"no scope":          func(e *RoutineEvent) { e.Scope = Scope{} },
		"empty id":          func(e *RoutineEvent) { e.ID = "" },
		"id with slash":     func(e *RoutineEvent) { e.ID = "a/b" },
		"long id":           func(e *RoutineEvent) { e.ID = strings.Repeat("a", 257) },
		"unknown kind":      func(e *RoutineEvent) { e.Kind = "jira.issue" },
		"wrong source":      func(e *RoutineEvent) { e.SourceRef = RoutineCallbackSourceRef("reg-1") },
		"google no subject": func(e *RoutineEvent) { e.SourceRef = "google:user-1:" },
		"no time":           func(e *RoutineEvent) { e.OccurredAt = time.Time{} },
		"long subject":      func(e *RoutineEvent) { e.Subject = strings.Repeat("s", 257) },
		"no summary":        func(e *RoutineEvent) { e.Summary = " " },
		"two line summary":  func(e *RoutineEvent) { e.Summary = "a\nb" },
		"long summary":      func(e *RoutineEvent) { e.Summary = strings.Repeat("s", 281) },
		"unknown field":     func(e *RoutineEvent) { e.Fields["channel"] = []string{"C1"} },
		"bot on gmail":      func(e *RoutineEvent) { e.Fields["bot"] = []string{"true"} },
		"too many values":   func(e *RoutineEvent) { e.Fields["to"] = make([]string, 17) },
		"long value":        func(e *RoutineEvent) { e.Fields["subject"] = []string{strings.Repeat("v", 1025)} },
		"array payload":     func(e *RoutineEvent) { e.Payload = json.RawMessage(`[1]`) },
		"broken payload":    func(e *RoutineEvent) { e.Payload = json.RawMessage(`{"a":`) },
		"big payload":       func(e *RoutineEvent) { e.Payload = json.RawMessage(`{"a":"` + strings.Repeat("x", 256<<10) + `"}`) },
		"test without by":   func(e *RoutineEvent) { e.Test = true },
		"testBy on real":    func(e *RoutineEvent) { e.TestBy = "user-1" },
		"too many field keys": func(e *RoutineEvent) {
			e.Kind, e.SourceRef = RoutineEventWebhook, HookSourceRef(HookID(e.Scope, "act-1"))
			e.Fields = map[string][]string{}
			for i := 0; i < 33; i++ {
				e.Fields[strings.Repeat("k", i+1)] = nil
			}
		},
	}
	for name, mutate := range cases {
		event := validRoutineTestEvent()
		mutate(event)
		if err := event.Validate(); !errors.Is(err, ErrInvalidRoutineEvent) {
			t.Errorf("%s: want ErrInvalidRoutineEvent, got %v", name, err)
		}
	}

	slack := &RoutineEvent{
		Scope: Scope{Kind: "tenant", ID: "t1"}, ID: "slack:message:T1:C1:1700000000.000100", Kind: RoutineEventSlackMessage,
		SourceRef: RoutineConversationSourceRef("dep", "bind", "slack", "T1"), OccurredAt: time.Now(), Summary: "Ana in #support: help",
		Fields: map[string][]string{"channel": {"C1"}, "bot": {"false"}, "text": {strings.Repeat("t", 4<<10)}},
	}
	if err := slack.Validate(); err != nil {
		t.Fatalf("slack event with 4 KiB text: %v", err)
	}
	slack.Fields["bot"] = []string{"maybe"}
	if err := slack.Validate(); err == nil {
		t.Fatal("bot must be true or false")
	}

	calendar := &RoutineEvent{
		Scope: Scope{Kind: "tenant", ID: "t1"}, ID: "calendar:ev1:1700000000:starts_in:15", Kind: RoutineEventCalendar,
		SourceRef: RoutineGoogleSourceRef("user-1", "1093"), OccurredAt: time.Now(), Summary: "Standup starts in 15 minutes",
		Fields: map[string][]string{"on": {"starts_in:15"}, "title": {"Standup"}},
	}
	if err := calendar.Validate(); err != nil {
		t.Fatalf("calendar event: %v", err)
	}
	for _, on := range []string{"starts_in:0", "starts_in:1441", "starts_in:015", "soon", ""} {
		calendar.Fields["on"] = []string{on}
		if err := calendar.Validate(); err == nil {
			t.Errorf("calendar on %q accepted", on)
		}
	}

	test := validRoutineTestEvent()
	test.ID, test.Test, test.TestBy, test.Summary = "test:9f1c", true, "user-1", "Test event"
	if err := test.Validate(); err != nil {
		t.Fatalf("test event: %v", err)
	}
}

func TestRoutineSourceRefs(t *testing.T) {
	ref := RoutineConversationSourceRef("dep-1", "bind-1", "slack", "T01")
	if !strings.HasPrefix(ref, "conversation:") || len(ref) != len("conversation:")+40 {
		t.Fatalf("conversation source ref %q", ref)
	}
	if ref != RoutineConversationSourceRef("dep-1", "bind-1", "slack", "T01") {
		t.Fatal("conversation source ref is not stable")
	}
	if ref == RoutineConversationSourceRef("dep-2", "bind-1", "slack", "T01") {
		t.Fatal("deployment must be part of the conversation source ref")
	}
	if ref == RoutineConversationSourceRef("dep-1", "bind-1slack", "", "T01") {
		t.Fatal("parts must be separated")
	}
	if got := RoutineCallbackSourceRef("reg-1"); got != "callback:reg-1" {
		t.Fatalf("callback source ref %q", got)
	}
	if got := RoutineGoogleSourceRef("user-1", "1093"); got != "google:user-1:1093" {
		t.Fatalf("google source ref %q", got)
	}

	scope := Scope{Kind: "tenant", ID: "t1"}
	hookID := HookID(scope, "act-1")
	if !strings.HasPrefix(hookID, "hk_") || len(hookID) != 29 || !ValidHookID(hookID) {
		t.Fatalf("hook id %q", hookID)
	}
	if hookID != HookID(scope, "act-1") {
		t.Fatal("hook id is not stable")
	}
	if hookID == HookID(Scope{Kind: "tenant", ID: "t2"}, "act-1") || hookID == HookID(scope, "act-2") {
		t.Fatal("hook id must depend on scope and activation")
	}
	if got := HookSourceRef(hookID); got != "hook:"+hookID {
		t.Fatalf("hook source ref %q", got)
	}
	for _, bad := range []string{"", "hk_", "hk_ABCDEFGHIJKLMNOPQRSTUVWXYZ", "hk_abcdefghijklmnopqrstuvwxy1", "xx_abcdefghijklmnopqrstuvwxyz", hookID + "a"} {
		if ValidHookID(bad) {
			t.Errorf("ValidHookID(%q) = true", bad)
		}
	}

	valid := map[RoutineEventKind]string{
		RoutineEventSlackMessage:    ref,
		RoutineEventTelegramMessage: ref,
		RoutineEventGitHub:          RoutineCallbackSourceRef("reg-1"),
		RoutineEventGmailMessage:    RoutineGoogleSourceRef("user-1", "1093"),
		RoutineEventCalendar:        RoutineGoogleSourceRef("user-1", "1093"),
		RoutineEventWebhook:         HookSourceRef(hookID),
	}
	for kind, sourceRef := range valid {
		if err := ValidateRoutineSourceRef(kind, sourceRef); err != nil {
			t.Errorf("%s %q: %v", kind, sourceRef, err)
		}
		for otherKind, otherRef := range valid {
			if otherRef == sourceRef {
				continue
			}
			if err := ValidateRoutineSourceRef(kind, otherRef); err == nil {
				t.Errorf("%s accepted %s source %q", kind, otherKind, otherRef)
			}
		}
	}
	if err := ValidateRoutineSourceRef(RoutineEventWebhook, "hook:hk_short"); err == nil {
		t.Error("webhook accepted a malformed hook id")
	}
	if err := ValidateRoutineSourceRef(RoutineEventSlackMessage, "conversation:"+strings.Repeat("G", 40)); err == nil {
		t.Error("slack accepted a non-hex conversation hash")
	}
}

func TestValidRoutineEventID(t *testing.T) {
	for id, want := range map[string]bool{
		"hs:order-1": true, "github:abc": true, "": false, " hs:x": false, "hs:x ": false,
		"hs:a/b": false, "hs:a?b": false, "hs:http://x": false, strings.Repeat("x", RoutineEventIDMaxBytes): true,
		strings.Repeat("x", RoutineEventIDMaxBytes+1): false,
	} {
		if got := ValidRoutineEventID(id); got != want {
			t.Fatalf("ValidRoutineEventID(%q)=%v", id, got)
		}
	}
}
