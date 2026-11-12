package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var routineFilterTestScope = Scope{Kind: "tenant", ID: "t1"}

func routineSlackTestRef() string {
	return RoutineConversationSourceRef("dep-1", "bind-1", "slack", "T01")
}

func routineGoogleTestRef() string { return RoutineGoogleSourceRef("user-1", "1093") }

func routineWebhookTestTrigger() RoutineEventTrigger {
	hookID := HookID(routineFilterTestScope, "act-1")
	return RoutineEventTrigger{
		Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "Signup form",
		MaxPerHour: 20, Webhook: &RoutineWebhook{HookID: hookID},
	}
}

func TestRoutineEventTriggerValidate(t *testing.T) {
	slack := func() RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventSlackMessage, SourceRef: routineSlackTestRef(), SourceLabel: "Acme Slack", MaxPerHour: 20,
			Filter: []RoutineEventCondition{{Field: "channel", Op: RoutineFilterEquals, Value: "C1", Label: "#support"}},
		}
	}
	gmail := func() RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventGmailMessage, SourceRef: routineGoogleTestRef(), SourceLabel: "your Gmail (ana@acme.com)", MaxPerHour: 20,
			Filter: []RoutineEventCondition{{Field: "fromUnverified", Op: RoutineFilterGlob, Value: "*@acme.com"}},
		}
	}
	calendar := func() RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventCalendar, SourceRef: routineGoogleTestRef(), SourceLabel: "your Google Calendar", MaxPerHour: 20,
			Calendar: &RoutineCalendarTiming{On: RoutineCalendarStartsIn, Minutes: 15},
		}
	}
	telegram := func() RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventTelegramMessage, SourceRef: routineSlackTestRef(), SourceLabel: "Ops bot", MaxPerHour: 5, IncludeBots: true,
			Filter: []RoutineEventCondition{{Field: "chat", Op: RoutineFilterOneOf, Values: []string{"-100", "-200"}}},
		}
	}
	github := func() RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventGitHub, SourceRef: RoutineCallbackSourceRef("reg-1"), SourceLabel: "GitHub", MaxPerHour: 60, MaximumOccurrences: 3,
			Filter: []RoutineEventCondition{{Field: "event", Op: RoutineFilterEquals, Value: "issues.opened"}},
		}
	}
	webhook := func() RoutineEventTrigger {
		trigger := routineWebhookTestTrigger()
		trigger.Filter = []RoutineEventCondition{{Field: "payload.order.status", Op: RoutineFilterEquals, Value: "paid"}}
		return trigger
	}
	for name, build := range map[string]func() RoutineEventTrigger{"slack": slack, "gmail": gmail, "calendar": calendar, "telegram": telegram, "github": github, "webhook": webhook} {
		trigger := build()
		if err := trigger.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	invalid := map[string]RoutineEventTrigger{}
	add := func(name string, base func() RoutineEventTrigger, mutate func(*RoutineEventTrigger)) {
		trigger := base()
		mutate(&trigger)
		invalid[name] = trigger
	}
	add("unknown kind", slack, func(t *RoutineEventTrigger) { t.Kind = "jira" })
	add("wrong source for kind", slack, func(t *RoutineEventTrigger) { t.SourceRef = routineGoogleTestRef() })
	add("no label", slack, func(t *RoutineEventTrigger) { t.SourceLabel = " " })
	add("long label", slack, func(t *RoutineEventTrigger) { t.SourceLabel = strings.Repeat("l", 161) })
	add("slack without channel", slack, func(t *RoutineEventTrigger) { t.Filter = nil })
	add("slack channel contains", slack, func(t *RoutineEventTrigger) { t.Filter[0].Op = RoutineFilterContains })
	add("slack channel not", slack, func(t *RoutineEventTrigger) { t.Filter[0].Not = true })
	add("telegram without chat", telegram, func(t *RoutineEventTrigger) { t.Filter[0].Field = "chatTitle" })
	add("bot filter", slack, func(t *RoutineEventTrigger) {
		t.Filter = append(t.Filter, RoutineEventCondition{Field: "bot", Op: RoutineFilterEquals, Value: "true"})
	})
	add("slack direct field", slack, func(t *RoutineEventTrigger) {
		t.Filter = append(t.Filter, RoutineEventCondition{Field: "direct", Op: RoutineFilterEquals, Value: "true"})
	})
	add("gmail field from other kind", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Field = "channel" })
	add("calendar on filter", calendar, func(t *RoutineEventTrigger) {
		t.Filter = []RoutineEventCondition{{Field: "on", Op: RoutineFilterEquals, Value: "created"}}
	})
	add("payload path outside webhook", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Field = "payload.a" })
	add("empty payload path", webhook, func(t *RoutineEventTrigger) { t.Filter[0].Field = "payload.a..b" })
	add("too many conditions", gmail, func(t *RoutineEventTrigger) {
		for len(t.Filter) <= 10 {
			t.Filter = append(t.Filter, RoutineEventCondition{Field: "subject", Op: RoutineFilterExists})
		}
	})
	add("unknown op", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Op = "regex" })
	add("equals without value", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Value = "" })
	add("equals with values", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Values = []string{"a"} })
	add("long value", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Value = strings.Repeat("v", 513) })
	add("one_of without values", telegram, func(t *RoutineEventTrigger) { t.Filter[0].Values = nil })
	add("one_of too many values", telegram, func(t *RoutineEventTrigger) { t.Filter[0].Values = make([]string, 21) })
	add("one_of empty value", telegram, func(t *RoutineEventTrigger) { t.Filter[0].Values = []string{"-100", " "} })
	add("exists with value", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Op = RoutineFilterExists })
	add("long condition label", gmail, func(t *RoutineEventTrigger) { t.Filter[0].Label = strings.Repeat("l", 121) })
	add("calendar without timing", calendar, func(t *RoutineEventTrigger) { t.Calendar = nil })
	add("calendar unknown timing", calendar, func(t *RoutineEventTrigger) { t.Calendar.On = "soon" })
	add("calendar zero minutes", calendar, func(t *RoutineEventTrigger) { t.Calendar.Minutes = 0 })
	add("calendar too many minutes", calendar, func(t *RoutineEventTrigger) { t.Calendar.Minutes = 1441 })
	add("calendar minutes on created", calendar, func(t *RoutineEventTrigger) { t.Calendar.On = RoutineCalendarCreated })
	add("timing on gmail", gmail, func(t *RoutineEventTrigger) { t.Calendar = &RoutineCalendarTiming{On: RoutineCalendarCreated} })
	add("includeBots on gmail", gmail, func(t *RoutineEventTrigger) { t.IncludeBots = true })
	add("maxPerHour zero", gmail, func(t *RoutineEventTrigger) { t.MaxPerHour = 0 })
	add("maxPerHour 61", gmail, func(t *RoutineEventTrigger) { t.MaxPerHour = 61 })
	add("negative occurrences", gmail, func(t *RoutineEventTrigger) { t.MaximumOccurrences = -1 })
	add("webhook without hook", webhook, func(t *RoutineEventTrigger) { t.Webhook = nil })
	add("webhook other hook", webhook, func(t *RoutineEventTrigger) { t.Webhook.HookID = HookID(routineFilterTestScope, "act-2") })
	add("hook on gmail", gmail, func(t *RoutineEventTrigger) {
		t.Webhook = &RoutineWebhook{HookID: HookID(routineFilterTestScope, "act-1")}
	})
	for name, trigger := range invalid {
		if err := trigger.Validate(); !errors.Is(err, ErrInvalidRoutineEventTrigger) {
			t.Errorf("%s: want ErrInvalidRoutineEventTrigger, got %v", name, err)
		}
	}
	var missing *RoutineEventTrigger
	if err := missing.Validate(); err == nil {
		t.Error("nil trigger accepted")
	}
}

func routineMatchEvent(kind RoutineEventKind, sourceRef string, fields map[string][]string, payload string) *RoutineEvent {
	event := &RoutineEvent{
		Scope: routineFilterTestScope, ID: "ev-1", Kind: kind, SourceRef: sourceRef,
		OccurredAt: time.Now(), Summary: "event", Fields: fields,
	}
	if payload != "" {
		event.Payload = json.RawMessage(payload)
	}
	return event
}

func TestRoutineEventFilterMatches(t *testing.T) {
	gmail := RoutineEventTrigger{
		Kind: RoutineEventGmailMessage, SourceRef: routineGoogleTestRef(), SourceLabel: "your Gmail", MaxPerHour: 20,
		Filter: []RoutineEventCondition{
			{Field: "from", Op: RoutineFilterGlob, Value: "*@ACME.com"},
			{Field: "subject", Op: RoutineFilterContains, Value: "Invoice"},
			{Field: "label", Op: RoutineFilterEquals, Value: "spam", Not: true},
		},
	}
	gmailEvent := func(fields map[string][]string) *RoutineEvent {
		return routineMatchEvent(RoutineEventGmailMessage, routineGoogleTestRef(), fields, "")
	}
	cases := []struct {
		name   string
		fields map[string][]string
		want   bool
	}{
		{"all hold", map[string][]string{"from": {"Ana <ana@acme.com>", "ana@acme.com"}, "subject": {"Your invoice #44"}, "label": {"INBOX"}}, true},
		{"other domain", map[string][]string{"from": {"bob@other.com"}, "subject": {"invoice"}}, false},
		{"unverified sender never matches from", map[string][]string{"fromUnverified": {"ana@acme.com"}, "subject": {"invoice"}}, false},
		{"subject missing", map[string][]string{"from": {"ana@acme.com"}}, false},
		{"negated label present", map[string][]string{"from": {"ana@acme.com"}, "subject": {"invoice"}, "label": {"INBOX", " Spam "}}, false},
	}
	for _, tc := range cases {
		if got := gmail.Matches(gmailEvent(tc.fields)); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}

	other := gmailEvent(map[string][]string{"from": {"ana@acme.com"}, "subject": {"invoice"}})
	other.SourceRef = RoutineGoogleSourceRef("user-2", "1093")
	if gmail.Matches(other) {
		t.Error("an event of another source matched")
	}
	other.SourceRef, other.Kind = routineGoogleTestRef(), RoutineEventCalendar
	if gmail.Matches(other) {
		t.Error("an event of another kind matched")
	}

	ops := []struct {
		condition RoutineEventCondition
		values    []string
		want      bool
	}{
		{RoutineEventCondition{Op: RoutineFilterEquals, Value: " Support "}, []string{"support"}, true},
		{RoutineEventCondition{Op: RoutineFilterEquals, Value: "support"}, []string{"supports"}, false},
		{RoutineEventCondition{Op: RoutineFilterContains, Value: "URGENT"}, []string{"this is urgent!"}, true},
		{RoutineEventCondition{Op: RoutineFilterOneOf, Values: []string{"a", "B"}}, []string{"c", "b"}, true},
		{RoutineEventCondition{Op: RoutineFilterOneOf, Values: []string{"a", "B"}}, []string{"c"}, false},
		{RoutineEventCondition{Op: RoutineFilterGlob, Value: "release-*"}, []string{"Release-1.2"}, true},
		{RoutineEventCondition{Op: RoutineFilterGlob, Value: "release-*"}, []string{"pre-release-1"}, false},
		{RoutineEventCondition{Op: RoutineFilterGlob, Value: "*fix*bug*"}, []string{"a fix for a bug"}, true},
		{RoutineEventCondition{Op: RoutineFilterGlob, Value: "a?c"}, []string{"abc"}, false},
		{RoutineEventCondition{Op: RoutineFilterExists}, []string{" ", ""}, false},
		{RoutineEventCondition{Op: RoutineFilterExists}, []string{"x"}, true},
		{RoutineEventCondition{Op: RoutineFilterExists}, nil, false},
		{RoutineEventCondition{Op: RoutineFilterExists, Not: true}, nil, true},
		{RoutineEventCondition{Op: RoutineFilterEquals, Value: "x", Not: true}, nil, true},
		{RoutineEventCondition{Op: RoutineFilterEquals, Value: "x", Not: true}, []string{"y", "X"}, false},
	}
	for index, tc := range ops {
		if got := tc.condition.holds(tc.values); got != tc.want {
			t.Errorf("op case %d (%s %v): holds = %v, want %v", index, tc.condition.Op, tc.values, got, tc.want)
		}
	}

	slack := RoutineEventTrigger{
		Kind: RoutineEventSlackMessage, SourceRef: routineSlackTestRef(), SourceLabel: "Acme Slack", MaxPerHour: 20,
		Filter: []RoutineEventCondition{{Field: "channel", Op: RoutineFilterOneOf, Values: []string{"C1", "C2"}}},
	}
	human := routineMatchEvent(RoutineEventSlackMessage, routineSlackTestRef(), map[string][]string{"channel": {"C2"}, "bot": {"false"}}, "")
	bot := routineMatchEvent(RoutineEventSlackMessage, routineSlackTestRef(), map[string][]string{"channel": {"C2"}, "bot": {"true"}}, "")
	if !slack.Matches(human) {
		t.Error("human message in a named channel did not match")
	}
	if slack.Matches(bot) {
		t.Error("bot message matched without includeBots")
	}
	slack.IncludeBots = true
	if !slack.Matches(bot) {
		t.Error("bot message did not match with includeBots")
	}
	if slack.Matches(routineMatchEvent(RoutineEventSlackMessage, routineSlackTestRef(), map[string][]string{"channel": {"C3"}}, "")) {
		t.Error("message in another channel matched")
	}

	webhook := routineWebhookTestTrigger()
	webhook.Filter = []RoutineEventCondition{
		{Field: "payload.order.status", Op: RoutineFilterEquals, Value: "PAID"},
		{Field: "payload.order.items.1.sku", Op: RoutineFilterGlob, Value: "sku-*"},
		{Field: "payload.tags", Op: RoutineFilterOneOf, Values: []string{"vip"}},
		{Field: "payload.total", Op: RoutineFilterEquals, Value: "12.50"},
		{Field: "payload.test", Op: RoutineFilterEquals, Value: "false"},
		{Field: "payload.note", Op: RoutineFilterExists, Not: true},
		{Field: "contentType", Op: RoutineFilterContains, Value: "json"},
	}
	webhookEvent := func(payload string) *RoutineEvent {
		return routineMatchEvent(RoutineEventWebhook, webhook.SourceRef, map[string][]string{"contentType": {"application/json"}}, payload)
	}
	matching := `{"order":{"status":"paid","items":[{"sku":"a"},{"sku":"SKU-9"}]},"tags":["new","vip"],"total":12.50,"test":false,"note":null}`
	if !webhook.Matches(webhookEvent(matching)) {
		t.Error("webhook payload conditions did not match")
	}
	for name, payload := range map[string]string{
		"status differs":  `{"order":{"status":"open","items":[{"sku":"a"},{"sku":"SKU-9"}]},"tags":["vip"],"total":12.50,"test":false}`,
		"index missing":   `{"order":{"status":"paid","items":[{"sku":"SKU-9"}]},"tags":["vip"],"total":12.50,"test":false}`,
		"note present":    `{"order":{"status":"paid","items":[{"sku":"a"},{"sku":"SKU-9"}]},"tags":["vip"],"total":12.50,"test":false,"note":"x"}`,
		"tags not listed": `{"order":{"status":"paid","items":[{"sku":"a"},{"sku":"SKU-9"}]},"tags":"vip2","total":12.50,"test":false}`,
		"no payload":      ``,
	} {
		if webhook.Matches(webhookEvent(payload)) {
			t.Errorf("webhook %s matched", name)
		}
	}
}

func TestRoutineEventCalendarTimingMatches(t *testing.T) {
	trigger := func(on string, minutes int) RoutineEventTrigger {
		return RoutineEventTrigger{
			Kind: RoutineEventCalendar, SourceRef: routineGoogleTestRef(), SourceLabel: "your Google Calendar", MaxPerHour: 20,
			Calendar: &RoutineCalendarTiming{On: on, Minutes: minutes},
		}
	}
	event := func(on string) *RoutineEvent {
		fields := map[string][]string{"title": {"Standup"}}
		if on != "" {
			fields["on"] = []string{on}
		}
		return routineMatchEvent(RoutineEventCalendar, routineGoogleTestRef(), fields, "")
	}
	created := trigger(RoutineCalendarCreated, 0)
	if created.Matches(event("starts_in:15")) {
		t.Fatal("a created routine matched a starts_in:15 event")
	}
	if !created.Matches(event("created")) {
		t.Fatal("a created routine did not match a created event")
	}
	if created.Matches(event("updated")) || created.Matches(event("")) {
		t.Fatal("a created routine matched an updated event or one without timing")
	}
	reminder := trigger(RoutineCalendarStartsIn, 15)
	if !reminder.Matches(event("starts_in:15")) {
		t.Fatal("a 15 minute routine did not match its reminder")
	}
	if reminder.Matches(event("starts_in:30")) || reminder.Matches(event("created")) {
		t.Fatal("a 15 minute routine matched another timing")
	}
	ended := trigger(RoutineCalendarEnded, 0)
	if !ended.Matches(event("ended")) || ended.Matches(event("starts_in:15")) {
		t.Fatal("ended routine timing")
	}
	reminder.Filter = []RoutineEventCondition{{Field: "title", Op: RoutineFilterContains, Value: "review"}}
	if reminder.Matches(event("starts_in:15")) {
		t.Fatal("title condition ignored")
	}
}

func TestDescribeRoutineEventTrigger(t *testing.T) {
	gmail := &RoutineEventTrigger{
		Kind: RoutineEventGmailMessage, SourceRef: routineGoogleTestRef(), SourceLabel: "your Gmail (ana@acme.com)", MaxPerHour: 20,
		Filter: []RoutineEventCondition{
			{Field: "from", Op: RoutineFilterGlob, Value: "*@acme.com"},
			{Field: "subject", Op: RoutineFilterContains, Value: "invoice"},
		},
	}
	calendar := &RoutineEventTrigger{
		Kind: RoutineEventCalendar, SourceRef: routineGoogleTestRef(), SourceLabel: "your Google Calendar (ana@acme.com)", MaxPerHour: 20,
		Calendar: &RoutineCalendarTiming{On: RoutineCalendarStartsIn, Minutes: 15},
	}
	slack := &RoutineEventTrigger{
		Kind: RoutineEventSlackMessage, SourceRef: routineSlackTestRef(), SourceLabel: "Acme Slack", MaxPerHour: 20,
		Filter: []RoutineEventCondition{
			{Field: "channel", Op: RoutineFilterEquals, Value: "C0123", Label: "#support"},
			{Field: "inThread", Op: RoutineFilterEquals, Value: "false"},
		},
	}
	telegram := &RoutineEventTrigger{
		Kind: RoutineEventTelegramMessage, SourceRef: routineSlackTestRef(), SourceLabel: "Ops bot", MaxPerHour: 1,
		Filter: []RoutineEventCondition{{Field: "chat", Op: RoutineFilterOneOf, Values: []string{"-100", "-200"}, Label: "Ops and Alerts"}},
	}
	github := &RoutineEventTrigger{
		Kind: RoutineEventGitHub, SourceRef: RoutineCallbackSourceRef("reg-1"), SourceLabel: "GitHub", MaxPerHour: 20,
		Filter: []RoutineEventCondition{
			{Field: "event", Op: RoutineFilterEquals, Value: "issues.opened"},
			{Field: "repository", Op: RoutineFilterEquals, Value: "acme/api"},
			{Field: "labels", Op: RoutineFilterEquals, Value: "bug"},
		},
	}
	webhook := routineWebhookTestTrigger()
	webhook.Filter = []RoutineEventCondition{
		{Field: "payload.order.status", Op: RoutineFilterEquals, Value: "paid"},
		{Field: "payload.plan", Op: RoutineFilterEquals, Value: "free", Not: true},
	}
	cases := []struct {
		trigger  *RoutineEventTrigger
		when     string
		audience string
		limit    string
	}{
		{gmail,
			"When a new email arrives in your Gmail (ana@acme.com) from *@acme.com with “invoice” in the subject",
			"anyone who emails you (only verified senders match a sender condition)",
			"At most 20 times an hour. More emails than that wait and go into the next run together."},
		{calendar,
			"15 minutes before each event in your Google Calendar (ana@acme.com)",
			"anyone who sends you an invitation",
			"At most 20 times an hour. More events than that wait and go into the next run together."},
		{slack,
			"When someone posts in #support (Acme Slack) and it isn't a reply in a thread",
			"anyone who posts in #support",
			"At most 20 times an hour. More messages than that wait and go into the next run together."},
		{telegram,
			"When someone posts in Ops and Alerts (Ops bot)",
			"anyone who posts in Ops and Alerts",
			"At most once an hour. More messages than that wait and go into the next run together."},
		{github,
			"When an issue is opened in acme/api (GitHub) labeled bug",
			"activity in acme/api",
			"At most 20 times an hour. More events than that wait and go into the next run together."},
		{&webhook,
			"When a request arrives at this routine's address where the field order.status is paid and the field plan is not free",
			"anyone with this routine's secret or private address",
			"At most 20 times an hour. More events than that wait and go into the next run together."},
	}
	for _, tc := range cases {
		if err := tc.trigger.Validate(); err != nil {
			t.Fatalf("%s trigger invalid: %v", tc.trigger.Kind, err)
		}
		if got := DescribeRoutineEventTrigger(tc.trigger); got != tc.when {
			t.Errorf("%s when:\n got %q\nwant %q", tc.trigger.Kind, got, tc.when)
		}
		if got := DescribeRoutineEventTriggerAudience(tc.trigger); got != tc.audience {
			t.Errorf("%s audience:\n got %q\nwant %q", tc.trigger.Kind, got, tc.audience)
		}
		if got := DescribeRoutineEventTriggerLimit(tc.trigger); got != tc.limit {
			t.Errorf("%s limit:\n got %q\nwant %q", tc.trigger.Kind, got, tc.limit)
		}
	}

	timings := map[RoutineCalendarTiming]string{
		{On: RoutineCalendarStartsIn, Minutes: 1}:   "1 minute before each event in Cal",
		{On: RoutineCalendarStartsIn, Minutes: 120}: "2 hours before each event in Cal",
		{On: RoutineCalendarCreated}:                "When an event is added to Cal",
		{On: RoutineCalendarUpdated}:                "When an event changes in Cal",
		{On: RoutineCalendarEnded}:                  "When an event ends in Cal",
	}
	for timing, want := range timings {
		timing := timing
		trigger := &RoutineEventTrigger{Kind: RoutineEventCalendar, SourceLabel: "Cal", Calendar: &timing, MaxPerHour: 20}
		if got := DescribeRoutineEventTrigger(trigger); got != want {
			t.Errorf("calendar %+v: got %q, want %q", timing, got, want)
		}
	}

	unknownEvent := &RoutineEventTrigger{
		Kind: RoutineEventGitHub, SourceLabel: "acme/api on GitHub", MaxPerHour: 20,
		Filter: []RoutineEventCondition{{Field: "event", Op: RoutineFilterOneOf, Values: []string{"pull_request.opened", "pull_request.reopened"}}},
	}
	if got, want := DescribeRoutineEventTrigger(unknownEvent), "When a pull request is opened or a pull request is reopened in acme/api on GitHub"; got != want {
		t.Errorf("github one_of: got %q, want %q", got, want)
	}
	for _, sentence := range []string{DescribeRoutineEventTrigger(github), DescribeRoutineEventTrigger(slack)} {
		if strings.Contains(sentence, "issues.opened") || strings.Contains(sentence, "C0123") {
			t.Errorf("description shows a raw event type or ID: %q", sentence)
		}
	}
}
