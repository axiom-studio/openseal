package routine_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

func TestCreateTaskEventTriggerResolves(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	args := f.WithEvent(f.SlackEvent("C1"))
	preview, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, args)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := preview["schedule"]; ok {
		t.Fatal("event routine preview has a schedule")
	}
	event, _ := preview["event"].(map[string]interface{})
	if event["kind"] != "slack.message" || event["sourceLabel"] != "Acme Slack" || event["maxPerHour"] != models.RoutineEventDefaultMaxPerHour ||
		event["when"] != "When someone posts in #support (Acme Slack)" || event["whoCanTrigger"] != "anyone who posts in #support" || event["webhook"] != false {
		t.Fatalf("event preview=%#v", event)
	}
	result, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, args, "create-slack-routine")
	if err != nil {
		t.Fatal(err)
	}
	activation := result["activation"].(*models.RunbookActivation)
	if activation.ID != routine.RunbookActivationIDForKey(f.Run.Scope, "create-slack-routine") || activation.TriggerID != "event" ||
		activation.Trigger.Kind != runbook.TriggerEvent || activation.Trigger.Schedule != nil || activation.NextRunAt != nil || activation.MaximumConcurrent != 1 {
		t.Fatalf("activation=%#v", activation)
	}
	trigger := activation.Task.Event
	if trigger == nil || trigger.SourceRef != f.SlackRef || trigger.SourceLabel != "Acme Slack" || trigger.MaxPerHour != models.RoutineEventDefaultMaxPerHour ||
		trigger.Filter[0].Label != "#support" || activation.Task.CreatedBy != "user" {
		t.Fatalf("trigger=%#v", trigger)
	}
	// The approved brief card shows exactly the routine that was made.
	if stored := routine.RoutineEventPreview(trigger); !reflect.DeepEqual(stored, event) {
		t.Fatalf("preview %#v differs from the stored routine %#v", event, stored)
	}
	// Replaying the approved call converges on the same routine.
	again, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, args, "create-slack-routine")
	if err != nil || again["activation"].(*models.RunbookActivation).ID != activation.ID {
		t.Fatalf("replay=%#v, %v", again, err)
	}

	// replace_schedule never applies to an event routine.
	if _, _, _, _, err := routine.ResolveRunbookScheduleReplacement(t.Context(), f.Store, f.Run, map[string]interface{}{"activationId": activation.ID, "cron": "0 0 * * * *", "reason": "hourly"}); err == nil || !strings.Contains(err.Error(), "starts on an event") {
		t.Fatalf("replace_schedule on an event routine: %v", err)
	}

	gmail := f.WithEvent(map[string]interface{}{"kind": "gmail.message", "source": models.RoutineGoogleSourceRef("user", "g-user"), "maximumOccurrences": float64(1), "maxPerHour": float64(5),
		"filter": []interface{}{map[string]interface{}{"field": "from", "op": "glob", "value": "*@acme.com"}}})
	preview, err = f.Validate(t, f.Run, models.RunbookActionCreateTask, gmail)
	if err != nil {
		t.Fatal(err)
	}
	if event := preview["event"].(map[string]interface{}); event["maximumOccurrences"] != int64(1) || event["maxPerHour"] != 5 || event["sourceLabel"] != "your Gmail (user@acme.com)" {
		t.Fatalf("gmail preview=%#v", event)
	}

	both := maps.Clone(args)
	both["trigger"] = map[string]interface{}{"event": f.SlackEvent("C1"), "schedule": map[string]interface{}{"cron": "0 0 * * * *", "timezone": "UTC"}}
	if err := f.Catalog.ValidateInput(t.Context(), f.Bound, both); err == nil {
		t.Fatal("contract accepted a schedule and an event")
	}
	if _, err := f.Validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: f.Run, Bound: f.Bound, Arguments: both}); err == nil {
		t.Fatal("a routine with two triggers was accepted")
	}
}

func TestCreateTaskRejectsUnknownSource(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	for name, event := range map[string]map[string]interface{}{
		"made up slack":        {"kind": "slack.message", "source": "conversation:" + strings.Repeat("a", 40), "filter": []interface{}{map[string]interface{}{"field": "channel", "op": "equals", "value": "C1"}}},
		"slack ref as github":  {"kind": "github.event", "source": f.SlackRef},
		"no routines consumer": {"kind": "github.event", "source": models.RoutineCallbackSourceRef("reg-approvals")},
		"another deployment":   {"kind": "github.event", "source": models.RoutineCallbackSourceRef("reg-other")},
		"webhook source":       {"kind": "webhook", "source": "hook:hk_" + strings.Repeat("a", 26)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(event)); err == nil {
				t.Fatal("unknown source accepted")
			}
		})
	}
	if _, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "github.event", "source": f.GitHubRef})); err != nil {
		t.Fatalf("listed GitHub source refused: %v", err)
	}
}

func TestCreateTaskGoogleNeedsOwnSignIn(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	for name, event := range map[string]map[string]interface{}{
		"another person's gmail": {"kind": "gmail.message", "source": models.RoutineGoogleSourceRef("other", "g-other")},
		"calendar needs sign-in": {"kind": "calendar.event", "source": models.RoutineGoogleSourceRef("user", "g-user"), "calendar": map[string]interface{}{"on": "created"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(event)); err == nil {
				t.Fatal("Google source accepted")
			}
		})
	}
	other := models.CloneAgentRun(f.Run)
	other.ActingUser = "other"
	if _, err := f.Validate(t, other, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "gmail.message", "source": models.RoutineGoogleSourceRef("user", "g-user")})); err == nil {
		t.Fatal("another person used this person's Gmail")
	}
	result, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "gmail.message", "source": models.RoutineGoogleSourceRef("user", "g-user")}), "create-gmail")
	if err != nil {
		t.Fatal(err)
	}
	if trigger := result["activation"].(*models.RunbookActivation).Task.Event; trigger.SourceRef != models.RoutineGoogleSourceRef("user", "g-user") || trigger.SourceLabel != "your Gmail (user@acme.com)" {
		t.Fatalf("gmail trigger=%#v", trigger)
	}
}

func TestCreateTaskWebhookCreatesHook(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	args := f.WithEvent(map[string]interface{}{"kind": "webhook", "source": "new"})
	preview, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, args)
	if err != nil {
		t.Fatal(err)
	}
	if event := preview["event"].(map[string]interface{}); event["webhook"] != true || event["when"] != "When a request arrives at this routine's address" {
		t.Fatalf("webhook preview=%#v", event)
	}
	result, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, args, "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	activation := result["activation"].(*models.RunbookActivation)
	hookID := models.HookID(f.Run.Scope, activation.ID)
	if trigger := activation.Task.Event; trigger.Webhook == nil || trigger.Webhook.HookID != hookID || trigger.SourceRef != models.HookSourceRef(hookID) {
		t.Fatalf("webhook trigger=%#v", trigger)
	}
	hook, err := f.Store.GetHook(t.Context(), hookID)
	if err != nil || hook == nil || hook.ActivationID != activation.ID || hook.Scope != f.Run.Scope || hook.PathTokenEnabled {
		t.Fatalf("hook=%#v, %v", hook, err)
	}
	if _, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, args, "create-webhook"); err != nil {
		t.Fatalf("replay: %v", err)
	}

	noHooks := routinetest.NewEventRoutine(t, false)
	if _, err := noHooks.Validate(t, noHooks.Run, models.RunbookActionCreateTask, noHooks.WithEvent(map[string]interface{}{"kind": "webhook", "source": "new"})); !errors.Is(err, routine.ErrHooksUnavailable) {
		t.Fatalf("webhook without hooks: %v", err)
	}
}

func TestCreateTaskEventNeedsActingUser(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	run := models.CloneAgentRun(f.Run)
	run.ActingUser = ""
	for _, event := range []map[string]interface{}{f.SlackEvent("C1"), {"kind": "webhook", "source": "new"}} {
		if _, err := f.Validate(t, run, models.RunbookActionCreateTask, f.WithEvent(event)); err == nil || !strings.Contains(err.Error(), "signed-in person") {
			t.Fatalf("event routine without a person: %v", err)
		}
	}
	if _, err := f.Validate(t, run, models.RunbookActionCreateTask, f.Args); err != nil {
		t.Fatalf("schedule routine without a person: %v", err)
	}
}

func TestCreateTaskSlackNeedsEndpointChannel(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	for name, event := range map[string]map[string]interface{}{
		"unknown channel":    f.SlackEvent("C9"),
		"one unknown of two": f.SlackEvent("C1", "C9"),
		"direct endpoint":    f.SlackEvent("D1"),
		"no channel":         {"kind": "slack.message", "source": f.SlackRef},
		"negated channel":    {"kind": "slack.message", "source": f.SlackRef, "filter": []interface{}{map[string]interface{}{"field": "channel", "op": "equals", "value": "C1", "not": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(event)); err == nil {
				t.Fatal("channel outside the umbrella's endpoints accepted")
			}
		})
	}
	if _, err := f.Validate(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(f.SlackEvent("c1"))); err != nil {
		t.Fatalf("endpoint channel refused: %v", err)
	}
}

func TestSendTestEventOffers(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	created, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	activation := created["activation"].(*models.RunbookActivation)
	args := map[string]interface{}{"activationId": activation.ID, "payload": map[string]interface{}{"order": "A-1"}}
	preview, err := f.Validate(t, f.Run, routine.RunbookActionSendTestEvent, args)
	if err != nil || preview["activationId"] != activation.ID {
		t.Fatalf("preview=%#v, %v", preview, err)
	}
	result, err := f.Dispatch(t, f.Run, routine.RunbookActionSendTestEvent, args, "test-event")
	if err != nil {
		t.Fatal(err)
	}
	if result["activationId"] != activation.ID || result["eventId"] != "test:fixed" || result["offered"] != (externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("result=%#v", result)
	}
	if len(f.Offerer.Tests) != 1 || f.Offerer.Tests[0].ActivationID != activation.ID || f.Offerer.Tests[0].TestBy != "user" ||
		f.Offerer.Tests[0].Scope != f.Run.Scope || string(f.Offerer.Tests[0].Payload) != `{"order":"A-1"}` {
		t.Fatalf("offered=%#v", f.Offerer.Tests)
	}
	if _, err := f.Dispatch(t, f.Run, routine.RunbookActionSendTestEvent, map[string]interface{}{"activationId": activation.ID}, "test-event-default"); err != nil || f.Offerer.Tests[1].Payload != nil {
		t.Fatalf("default payload: %v %#v", err, f.Offerer.Tests)
	}
	large := map[string]interface{}{"activationId": activation.ID, "payload": map[string]interface{}{"text": strings.Repeat("x", routine.MaximumRoutineTestEventPayload)}}
	if _, err := f.Validate(t, f.Run, routine.RunbookActionSendTestEvent, large); err == nil {
		t.Fatal("oversized test payload accepted")
	}
	schedule, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.Args, "create-schedule")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Validate(t, f.Run, routine.RunbookActionSendTestEvent, map[string]interface{}{"activationId": schedule["activation"].(*models.RunbookActivation).ID}); err == nil {
		t.Fatal("test event sent to a schedule routine")
	}
}

func TestSendTestEventRejectsNonCreator(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	created, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"activationId": created["activation"].(*models.RunbookActivation).ID}
	for _, actingUser := range []string{"someone-else", ""} {
		run := models.CloneAgentRun(f.Run)
		run.ActingUser = actingUser
		if _, err := f.Validate(t, run, routine.RunbookActionSendTestEvent, args); !errors.Is(err, routine.ErrRoutineNotCreator) {
			t.Fatalf("acting user %q: %v", actingUser, err)
		}
	}
	if len(f.Offerer.Tests) != 0 {
		t.Fatalf("offered=%#v", f.Offerer.Tests)
	}
	f.Validator.SetRoutineEvents(nil, nil, true)
	if _, err := f.Validate(t, f.Run, routine.RunbookActionSendTestEvent, args); !errors.Is(err, routine.ErrRoutineEventsUnavailable) {
		t.Fatalf("without an offerer: %v", err)
	}
}

func resumeRefusedWith(t *testing.T, err error, reason models.RoutinePausedReason) {
	t.Helper()
	var refused *routine.SourceUnavailableError
	if !errors.Is(err, routine.ErrRoutineSourceUnavailable) || !errors.As(err, &refused) || refused.Reason != reason ||
		!strings.Contains(err.Error(), string(reason)) {
		t.Fatalf("resume error = %v, want %s", err, reason)
	}
}

func TestResumeEventRoutineRechecksSource(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	created, err := f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(f.SlackEvent("C1")), "create-slack")
	if err != nil {
		t.Fatal(err)
	}
	slack := created["activation"].(*models.RunbookActivation)
	if _, err := f.SetRoutineStatus(t, slack.ID, models.RunbookActivationPaused, "pause-slack"); err != nil {
		t.Fatal(err)
	}

	// The named channel's endpoint is gone: the connection alone is not enough.
	f.SetEndpointStatus(t, "ep-support", models.ExternalConversationEndpointPaused)
	_, err = f.SetRoutineStatus(t, slack.ID, models.RunbookActivationActive, "resume-slack-gone")
	resumeRefusedWith(t, err, models.RoutinePausedSourceRemoved)
	// Every caller of the service gets the same check.
	current, _ := f.Store.GetRunbookActivation(t.Context(), f.Run.Scope, slack.ID)
	_, err = routine.NewRunbookActivationService(f.Store).Update(t.Context(), f.Run.Scope, slack.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: models.RunbookActivationActive})
	resumeRefusedWith(t, err, models.RoutinePausedSourceRemoved)
	if stored, _ := f.Store.GetRunbookActivation(t.Context(), f.Run.Scope, slack.ID); stored.Status != models.RunbookActivationPaused || stored.Revision != current.Revision {
		t.Fatalf("refused resume changed the routine: %#v", stored)
	}
	f.SetEndpointStatus(t, "ep-support", models.ExternalConversationEndpointActive)
	if resumed, err := f.SetRoutineStatus(t, slack.ID, models.RunbookActivationActive, "resume-slack"); err != nil || resumed.Status != models.RunbookActivationActive {
		t.Fatalf("resume with the channel back = %#v, %v", resumed, err)
	}

	created, err = f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "gmail.message", "source": models.RoutineGoogleSourceRef("user", "g-user")}), "create-gmail")
	if err != nil {
		t.Fatal(err)
	}
	gmail := created["activation"].(*models.RunbookActivation)
	if _, err := f.SetRoutineStatus(t, gmail.ID, models.RunbookActivationPaused, "pause-gmail"); err != nil {
		t.Fatal(err)
	}
	// The creator's Google sign-in is gone from the host's catalog.
	f.Dispatcher.SetRoutineEvents(f.Offerer, routinetest.GoogleSources{
		{Kind: models.RoutineEventGmailMessage, Source: models.RoutineGoogleSourceRef("user", "g-user"), Label: "your Gmail", Available: false, Reason: routine.EventSourceNeedsSignIn, SkillID: "google"},
	}, true)
	_, err = f.SetRoutineStatus(t, gmail.ID, models.RunbookActivationActive, "resume-gmail-signed-out")
	resumeRefusedWith(t, err, models.RoutinePausedNeedsSignIn)
	// A host without a Google catalog cannot vouch for the sign-in either.
	current, _ = f.Store.GetRunbookActivation(t.Context(), f.Run.Scope, gmail.ID)
	_, err = routine.NewRunbookActivationService(f.Store).Update(t.Context(), f.Run.Scope, gmail.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: models.RunbookActivationActive})
	resumeRefusedWith(t, err, models.RoutinePausedNeedsSignIn)
	f.Dispatcher.SetRoutineEvents(f.Offerer, f.Google, true)
	if resumed, err := f.SetRoutineStatus(t, gmail.ID, models.RunbookActivationActive, "resume-gmail"); err != nil || resumed.Status != models.RunbookActivationActive {
		t.Fatalf("resume with the sign-in back = %#v, %v", resumed, err)
	}

	// A webhook routine has no outside source to check.
	created, err = f.Dispatch(t, f.Run, models.RunbookActionCreateTask, f.WithEvent(map[string]interface{}{"kind": "webhook", "source": "new"}), "create-webhook")
	if err != nil {
		t.Fatal(err)
	}
	hook := created["activation"].(*models.RunbookActivation)
	if _, err := f.SetRoutineStatus(t, hook.ID, models.RunbookActivationPaused, "pause-hook"); err != nil {
		t.Fatal(err)
	}
	if resumed, err := f.SetRoutineStatus(t, hook.ID, models.RunbookActivationActive, "resume-hook"); err != nil || resumed.Status != models.RunbookActivationActive {
		t.Fatalf("webhook resume = %#v, %v", resumed, err)
	}
}
