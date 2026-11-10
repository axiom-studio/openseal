package runtime

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func (f *eventRoutineFixture) slackEvent(channels ...string) map[string]interface{} {
	condition := map[string]interface{}{"field": "channel", "op": "equals", "value": channels[0]}
	if len(channels) > 1 {
		values := make([]interface{}, 0, len(channels))
		for _, channel := range channels {
			values = append(values, channel)
		}
		condition = map[string]interface{}{"field": "channel", "op": "one_of", "values": values}
	}
	return map[string]interface{}{"kind": "slack.message", "source": f.slackRef, "filter": []interface{}{condition}}
}

func TestCreateTaskEventTriggerResolves(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	args := f.withEvent(f.slackEvent("C1"))
	preview, err := f.validate(t, f.run, RunbookActionCreateTask, args)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := preview["schedule"]; ok {
		t.Fatal("event routine preview has a schedule")
	}
	event, _ := preview["event"].(map[string]interface{})
	if event["kind"] != "slack.message" || event["sourceLabel"] != "Acme Slack" || event["maxPerHour"] != RoutineEventDefaultMaxPerHour ||
		event["when"] != "When someone posts in #support (Acme Slack)" || event["whoCanTrigger"] != "anyone who posts in #support" || event["webhook"] != false {
		t.Fatalf("event preview=%#v", event)
	}
	result, err := f.dispatch(t, f.run, RunbookActionCreateTask, args, "create-slack-routine")
	if err != nil {
		t.Fatal(err)
	}
	activation := result["activation"].(*RunbookActivation)
	if activation.ID != RunbookActivationIDForKey(f.run.Scope, "create-slack-routine") || activation.TriggerID != "event" ||
		activation.Trigger.Kind != runbook.TriggerEvent || activation.Trigger.Schedule != nil || activation.NextRunAt != nil || activation.MaximumConcurrent != 1 {
		t.Fatalf("activation=%#v", activation)
	}
	trigger := activation.Task.Event
	if trigger == nil || trigger.SourceRef != f.slackRef || trigger.SourceLabel != "Acme Slack" || trigger.MaxPerHour != RoutineEventDefaultMaxPerHour ||
		trigger.Filter[0].Label != "#support" || activation.Task.CreatedBy != "user" {
		t.Fatalf("trigger=%#v", trigger)
	}
	// The approved brief card shows exactly the routine that was made.
	if stored := routineEventPreview(trigger); !reflect.DeepEqual(stored, event) {
		t.Fatalf("preview %#v differs from the stored routine %#v", event, stored)
	}
	// Replaying the approved call converges on the same routine.
	again, err := f.dispatch(t, f.run, RunbookActionCreateTask, args, "create-slack-routine")
	if err != nil || again["activation"].(*RunbookActivation).ID != activation.ID {
		t.Fatalf("replay=%#v, %v", again, err)
	}

	// replace_schedule never applies to an event routine.
	if _, _, _, _, err := resolveRunbookScheduleReplacement(t.Context(), f.store, f.run, map[string]interface{}{"activationId": activation.ID, "cron": "0 0 * * * *", "reason": "hourly"}); err == nil || !strings.Contains(err.Error(), "starts on an event") {
		t.Fatalf("replace_schedule on an event routine: %v", err)
	}

	gmail := f.withEvent(map[string]interface{}{"kind": "gmail.message", "source": RoutineGoogleSourceRef("user", "g-user"), "maximumOccurrences": float64(1), "maxPerHour": float64(5),
		"filter": []interface{}{map[string]interface{}{"field": "from", "op": "glob", "value": "*@acme.com"}}})
	preview, err = f.validate(t, f.run, RunbookActionCreateTask, gmail)
	if err != nil {
		t.Fatal(err)
	}
	if event := preview["event"].(map[string]interface{}); event["maximumOccurrences"] != int64(1) || event["maxPerHour"] != 5 || event["sourceLabel"] != "your Gmail (user@acme.com)" {
		t.Fatalf("gmail preview=%#v", event)
	}

	both := cloneMap(args)
	both["trigger"] = map[string]interface{}{"event": f.slackEvent("C1"), "schedule": map[string]interface{}{"cron": "0 0 * * * *", "timezone": "UTC"}}
	if err := f.catalog.ValidateInput(t.Context(), f.bound, both); err == nil {
		t.Fatal("contract accepted a schedule and an event")
	}
	if _, err := f.validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: f.run, Bound: f.bound, Arguments: both}); err == nil {
		t.Fatal("a routine with two triggers was accepted")
	}
}

func TestCreateTaskRejectsUnknownSource(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	for name, event := range map[string]map[string]interface{}{
		"made up slack":        {"kind": "slack.message", "source": "conversation:" + strings.Repeat("a", 40), "filter": []interface{}{map[string]interface{}{"field": "channel", "op": "equals", "value": "C1"}}},
		"slack ref as github":  {"kind": "github.event", "source": f.slackRef},
		"no routines consumer": {"kind": "github.event", "source": RoutineCallbackSourceRef("reg-approvals")},
		"another deployment":   {"kind": "github.event", "source": RoutineCallbackSourceRef("reg-other")},
		"webhook source":       {"kind": "webhook", "source": "hook:hk_" + strings.Repeat("a", 26)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.validate(t, f.run, RunbookActionCreateTask, f.withEvent(event)); err == nil {
				t.Fatal("unknown source accepted")
			}
		})
	}
	if _, err := f.validate(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "github.event", "source": f.githubRef})); err != nil {
		t.Fatalf("listed GitHub source refused: %v", err)
	}
}

func TestCreateTaskGoogleNeedsOwnSignIn(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	for name, event := range map[string]map[string]interface{}{
		"another person's gmail": {"kind": "gmail.message", "source": RoutineGoogleSourceRef("other", "g-other")},
		"calendar needs sign-in": {"kind": "calendar.event", "source": RoutineGoogleSourceRef("user", "g-user"), "calendar": map[string]interface{}{"on": "created"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.validate(t, f.run, RunbookActionCreateTask, f.withEvent(event)); err == nil {
				t.Fatal("Google source accepted")
			}
		})
	}
	other := cloneAgentRun(f.run)
	other.ActingUser = "other"
	if _, err := f.validate(t, other, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "gmail.message", "source": RoutineGoogleSourceRef("user", "g-user")})); err == nil {
		t.Fatal("another person used this person's Gmail")
	}
	result, err := f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "gmail.message", "source": RoutineGoogleSourceRef("user", "g-user")}), "create-gmail")
	if err != nil {
		t.Fatal(err)
	}
	if trigger := result["activation"].(*RunbookActivation).Task.Event; trigger.SourceRef != RoutineGoogleSourceRef("user", "g-user") || trigger.SourceLabel != "your Gmail (user@acme.com)" {
		t.Fatalf("gmail trigger=%#v", trigger)
	}
}

func TestCreateTaskWebhookCreatesHook(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	args := f.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"})
	preview, err := f.validate(t, f.run, RunbookActionCreateTask, args)
	if err != nil {
		t.Fatal(err)
	}
	if event := preview["event"].(map[string]interface{}); event["webhook"] != true || event["when"] != "When a request arrives at this routine's address" {
		t.Fatalf("webhook preview=%#v", event)
	}
	result, err := f.dispatch(t, f.run, RunbookActionCreateTask, args, "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	activation := result["activation"].(*RunbookActivation)
	hookID := HookID(f.run.Scope, activation.ID)
	if trigger := activation.Task.Event; trigger.Webhook == nil || trigger.Webhook.HookID != hookID || trigger.SourceRef != HookSourceRef(hookID) {
		t.Fatalf("webhook trigger=%#v", trigger)
	}
	hook, err := f.store.GetHook(t.Context(), hookID)
	if err != nil || hook == nil || hook.ActivationID != activation.ID || hook.Scope != f.run.Scope || hook.PathTokenEnabled {
		t.Fatalf("hook=%#v, %v", hook, err)
	}
	if _, err := f.dispatch(t, f.run, RunbookActionCreateTask, args, "create-webhook"); err != nil {
		t.Fatalf("replay: %v", err)
	}

	noHooks := newEventRoutineFixture(t, false)
	if _, err := noHooks.validate(t, noHooks.run, RunbookActionCreateTask, noHooks.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"})); !errors.Is(err, ErrRoutineHooksUnavailable) {
		t.Fatalf("webhook without hooks: %v", err)
	}
}

func TestCreateTaskEventNeedsActingUser(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	run := cloneAgentRun(f.run)
	run.ActingUser = ""
	for _, event := range []map[string]interface{}{f.slackEvent("C1"), {"kind": "webhook", "source": "new"}} {
		if _, err := f.validate(t, run, RunbookActionCreateTask, f.withEvent(event)); err == nil || !strings.Contains(err.Error(), "signed-in person") {
			t.Fatalf("event routine without a person: %v", err)
		}
	}
	if _, err := f.validate(t, run, RunbookActionCreateTask, f.args); err != nil {
		t.Fatalf("schedule routine without a person: %v", err)
	}
}

func TestCreateTaskEventSendGrantMustBePinned(t *testing.T) {
	f := routineGrantFixture(t)
	event := map[string]interface{}{"event": map[string]interface{}{"kind": "webhook", "source": "new"}}
	schedule := f.args["trigger"]
	free := map[string]interface{}{"action": "mail.send", "label": "Email the summary"}
	owned := map[string]interface{}{"action": "mail.send", "label": "Email you the summary", "toOwnerOnly": true}
	read := map[string]interface{}{"action": "mail.search", "label": "Search the web"}

	f.args["trigger"] = event
	if _, err := f.resolve(t, f.withGrants(read, free)); !errors.Is(err, ErrRoutineEventSendGrantNotPinned) {
		t.Fatalf("free-recipient send in an event routine: %v", err)
	}
	if _, err := f.resolve(t, f.withGrants(read, owned)); err != nil {
		t.Fatalf("owner-only send in an event routine: %v", err)
	}
	f.args["trigger"] = schedule
	if _, err := f.resolve(t, f.withGrants(read, free)); err != nil {
		t.Fatalf("schedule routines keep free sends: %v", err)
	}
}

func TestCreateTaskSlackNeedsEndpointChannel(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	for name, event := range map[string]map[string]interface{}{
		"unknown channel":    f.slackEvent("C9"),
		"one unknown of two": f.slackEvent("C1", "C9"),
		"direct endpoint":    f.slackEvent("D1"),
		"no channel":         {"kind": "slack.message", "source": f.slackRef},
		"negated channel":    {"kind": "slack.message", "source": f.slackRef, "filter": []interface{}{map[string]interface{}{"field": "channel", "op": "equals", "value": "C1", "not": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.validate(t, f.run, RunbookActionCreateTask, f.withEvent(event)); err == nil {
				t.Fatal("channel outside the umbrella's endpoints accepted")
			}
		})
	}
	if _, err := f.validate(t, f.run, RunbookActionCreateTask, f.withEvent(f.slackEvent("c1"))); err != nil {
		t.Fatalf("endpoint channel refused: %v", err)
	}
}

func TestSendTestEventOffers(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	created, err := f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	activation := created["activation"].(*RunbookActivation)
	args := map[string]interface{}{"activationId": activation.ID, "payload": map[string]interface{}{"order": "A-1"}}
	preview, err := f.validate(t, f.run, RunbookActionSendTestEvent, args)
	if err != nil || preview["activationId"] != activation.ID {
		t.Fatalf("preview=%#v, %v", preview, err)
	}
	result, err := f.dispatch(t, f.run, RunbookActionSendTestEvent, args, "test-event")
	if err != nil {
		t.Fatal(err)
	}
	if result["activationId"] != activation.ID || result["offered"] != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("result=%#v", result)
	}
	if len(f.offerer.tests) != 1 || f.offerer.tests[0].activationID != activation.ID || f.offerer.tests[0].testBy != "user" ||
		f.offerer.tests[0].scope != f.run.Scope || string(f.offerer.tests[0].payload) != `{"order":"A-1"}` {
		t.Fatalf("offered=%#v", f.offerer.tests)
	}
	if _, err := f.dispatch(t, f.run, RunbookActionSendTestEvent, map[string]interface{}{"activationId": activation.ID}, "test-event-default"); err != nil || f.offerer.tests[1].payload != nil {
		t.Fatalf("default payload: %v %#v", err, f.offerer.tests)
	}
	large := map[string]interface{}{"activationId": activation.ID, "payload": map[string]interface{}{"text": strings.Repeat("x", maximumRoutineTestEventPayload)}}
	if _, err := f.validate(t, f.run, RunbookActionSendTestEvent, large); err == nil {
		t.Fatal("oversized test payload accepted")
	}
	schedule, err := f.dispatch(t, f.run, RunbookActionCreateTask, f.args, "create-schedule")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.validate(t, f.run, RunbookActionSendTestEvent, map[string]interface{}{"activationId": schedule["activation"].(*RunbookActivation).ID}); err == nil {
		t.Fatal("test event sent to a schedule routine")
	}
}

func TestSendTestEventRejectsNonCreator(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	created, err := f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"activationId": created["activation"].(*RunbookActivation).ID}
	for _, actingUser := range []string{"someone-else", ""} {
		run := cloneAgentRun(f.run)
		run.ActingUser = actingUser
		if _, err := f.validate(t, run, RunbookActionSendTestEvent, args); !errors.Is(err, ErrRoutineNotCreator) {
			t.Fatalf("acting user %q: %v", actingUser, err)
		}
	}
	if len(f.offerer.tests) != 0 {
		t.Fatalf("offered=%#v", f.offerer.tests)
	}
	f.validator.SetRoutineEvents(nil, nil, true)
	if _, err := f.validate(t, f.run, RunbookActionSendTestEvent, args); !errors.Is(err, ErrRoutineEventsUnavailable) {
		t.Fatalf("without an offerer: %v", err)
	}
}
