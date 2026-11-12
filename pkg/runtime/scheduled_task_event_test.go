package runtime

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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

	both := maps.Clone(args)
	both["trigger"] = map[string]interface{}{"event": f.slackEvent("C1"), "schedule": map[string]interface{}{"cron": "0 0 * * * *", "timezone": "UTC"}}
	if err := f.catalog.ValidateInput(t.Context(), f.bound, both); err == nil {
		t.Fatal("contract accepted a schedule and an event")
	}
	if _, err := f.validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: f.run, Bound: f.bound, Arguments: both}); err == nil {
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
	if _, err := noHooks.validate(t, noHooks.run, RunbookActionCreateTask, noHooks.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"})); !errors.Is(err, ErrHooksUnavailable) {
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
	// Sends whose target the kernel cannot read from a known argument fail
	// closed: a chat ID key must be pinned, and a raw message body never can.
	freeChat := map[string]interface{}{"action": "mail.chat", "label": "Post the summary"}
	if _, err := f.resolve(t, f.withGrants(freeChat)); !errors.Is(err, ErrRoutineEventSendGrantNotPinned) &&
		(err == nil || !strings.Contains(err.Error(), "toDestinations")) {
		t.Fatalf("free chat send in an event routine: %v", err)
	}
	pinnedChat := map[string]interface{}{"action": "mail.chat", "label": "Post the summary", "toDestinations": []interface{}{"12345"}}
	if _, err := f.resolve(t, f.withGrants(pinnedChat)); err != nil {
		t.Fatalf("pinned chat send in an event routine: %v", err)
	}
	raw := map[string]interface{}{"action": "mail.raw", "label": "Send the draft"}
	if _, err := f.resolve(t, f.withGrants(raw)); !errors.Is(err, ErrRoutineEventSendGrantNotPinned) {
		t.Fatalf("raw-body send in an event routine: %v", err)
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
	if result["activationId"] != activation.ID || result["eventId"] != "test:fixed" || result["offered"] != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
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

// setRoutineStatus runs set_status the way a chat does.
func (f *eventRoutineFixture) setRoutineStatus(t *testing.T, activationID string, status RunbookActivationStatus, callID string) (*RunbookActivation, error) {
	t.Helper()
	current, err := f.store.GetRunbookActivation(t.Context(), f.run.Scope, activationID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.dispatch(t, f.run, RunbookActionSetStatus, map[string]interface{}{
		"activationId": activationID, "expectedRevision": float64(current.Revision), "status": string(status), "reason": "test",
	}, callID)
	if err != nil {
		return nil, err
	}
	return result["activation"].(*RunbookActivation), nil
}

func (f *eventRoutineFixture) setEndpointStatus(t *testing.T, id string, status ExternalConversationEndpointStatus) {
	t.Helper()
	endpoint, err := f.store.GetExternalConversationEndpoint(t.Context(), f.run.Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	next := *endpoint
	next.Status, next.Revision = status, endpoint.Revision+1
	if err := f.store.UpdateExternalConversationEndpoint(t.Context(), &next, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
}

func resumeRefusedWith(t *testing.T, err error, reason RoutinePausedReason) {
	t.Helper()
	var refused *RoutineSourceUnavailableError
	if !errors.Is(err, ErrRoutineSourceUnavailable) || !errors.As(err, &refused) || refused.Reason != reason ||
		!strings.Contains(err.Error(), string(reason)) {
		t.Fatalf("resume error = %v, want %s", err, reason)
	}
}

func TestResumeEventRoutineRechecksSource(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	created, err := f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(f.slackEvent("C1")), "create-slack")
	if err != nil {
		t.Fatal(err)
	}
	slack := created["activation"].(*RunbookActivation)
	if _, err := f.setRoutineStatus(t, slack.ID, RunbookActivationPaused, "pause-slack"); err != nil {
		t.Fatal(err)
	}

	// The named channel's endpoint is gone: the connection alone is not enough.
	f.setEndpointStatus(t, "ep-support", ExternalConversationEndpointPaused)
	_, err = f.setRoutineStatus(t, slack.ID, RunbookActivationActive, "resume-slack-gone")
	resumeRefusedWith(t, err, RoutinePausedSourceRemoved)
	// Every caller of the service gets the same check.
	current, _ := f.store.GetRunbookActivation(t.Context(), f.run.Scope, slack.ID)
	_, err = NewRunbookActivationService(f.store).Update(t.Context(), f.run.Scope, slack.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationActive})
	resumeRefusedWith(t, err, RoutinePausedSourceRemoved)
	if stored, _ := f.store.GetRunbookActivation(t.Context(), f.run.Scope, slack.ID); stored.Status != RunbookActivationPaused || stored.Revision != current.Revision {
		t.Fatalf("refused resume changed the routine: %#v", stored)
	}
	f.setEndpointStatus(t, "ep-support", ExternalConversationEndpointActive)
	if resumed, err := f.setRoutineStatus(t, slack.ID, RunbookActivationActive, "resume-slack"); err != nil || resumed.Status != RunbookActivationActive {
		t.Fatalf("resume with the channel back = %#v, %v", resumed, err)
	}

	created, err = f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "gmail.message", "source": RoutineGoogleSourceRef("user", "g-user")}), "create-gmail")
	if err != nil {
		t.Fatal(err)
	}
	gmail := created["activation"].(*RunbookActivation)
	if _, err := f.setRoutineStatus(t, gmail.ID, RunbookActivationPaused, "pause-gmail"); err != nil {
		t.Fatal(err)
	}
	// The creator's Google sign-in is gone from the host's catalog.
	f.dispatcher.SetRoutineEvents(f.offerer, routineGoogleSourcesForTest{
		{Kind: RoutineEventGmailMessage, Source: RoutineGoogleSourceRef("user", "g-user"), Label: "your Gmail", Available: false, Reason: RoutineEventSourceNeedsSignIn, SkillID: "google"},
	}, true)
	_, err = f.setRoutineStatus(t, gmail.ID, RunbookActivationActive, "resume-gmail-signed-out")
	resumeRefusedWith(t, err, RoutinePausedNeedsSignIn)
	// A host without a Google catalog cannot vouch for the sign-in either.
	current, _ = f.store.GetRunbookActivation(t.Context(), f.run.Scope, gmail.ID)
	_, err = NewRunbookActivationService(f.store).Update(t.Context(), f.run.Scope, gmail.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationActive})
	resumeRefusedWith(t, err, RoutinePausedNeedsSignIn)
	f.dispatcher.SetRoutineEvents(f.offerer, f.google, true)
	if resumed, err := f.setRoutineStatus(t, gmail.ID, RunbookActivationActive, "resume-gmail"); err != nil || resumed.Status != RunbookActivationActive {
		t.Fatalf("resume with the sign-in back = %#v, %v", resumed, err)
	}

	// A webhook routine has no outside source to check.
	created, err = f.dispatch(t, f.run, RunbookActionCreateTask, f.withEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	hook := created["activation"].(*RunbookActivation)
	if _, err := f.setRoutineStatus(t, hook.ID, RunbookActivationPaused, "pause-hook"); err != nil {
		t.Fatal(err)
	}
	if resumed, err := f.setRoutineStatus(t, hook.ID, RunbookActivationActive, "resume-hook"); err != nil || resumed.Status != RunbookActivationActive {
		t.Fatalf("webhook resume = %#v, %v", resumed, err)
	}
}
