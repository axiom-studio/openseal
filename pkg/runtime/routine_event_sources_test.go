package runtime

import (
	"reflect"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

func TestEventSourcesListsConnections(t *testing.T) {
	f := routinetest.NewEventRoutine(t, true)
	result, err := f.Dispatch(t, f.Run, routine.RunbookActionEventSources, map[string]interface{}{}, "sources")
	if err != nil {
		t.Fatal(err)
	}
	sources := result["sources"].([]*routine.RoutineEventSourceView)
	bySource := map[string]*routine.RoutineEventSourceView{}
	for _, source := range sources {
		if source.Kind != models.RoutineEventCalendar {
			bySource[source.Source] = source
		}
	}
	slack := bySource[f.SlackRef]
	if slack == nil || slack.Kind != models.RoutineEventSlackMessage || slack.Label != "Acme Slack" || !slack.Available ||
		!reflect.DeepEqual(slack.Channels, []routine.RoutineEventSourceChannel{{ID: "C1", Name: "#support"}}) {
		t.Fatalf("slack source=%#v", slack)
	}
	github := bySource[f.GitHubRef]
	if github == nil || github.Kind != models.RoutineEventGitHub || github.Label != "acme/api (GitHub)" || !reflect.DeepEqual(github.EventTypes, []string{"issues.opened", "pull_request.opened"}) {
		t.Fatalf("github source=%#v", github)
	}
	for _, missing := range []string{models.RoutineCallbackSourceRef("reg-approvals"), models.RoutineCallbackSourceRef("reg-other"), models.RoutineGoogleSourceRef("other", "g-other")} {
		if bySource[missing] != nil {
			t.Fatalf("listed %s", missing)
		}
	}
	gmail := bySource[models.RoutineGoogleSourceRef("user", "g-user")]
	if gmail == nil || gmail.Kind != models.RoutineEventGmailMessage || !gmail.Available || !reflect.DeepEqual(gmail.Fields, models.RoutineEventFilterFields(models.RoutineEventGmailMessage)) {
		t.Fatalf("gmail source=%#v", gmail)
	}
	if webhook := bySource[routine.RoutineEventSourceNew]; webhook == nil || webhook.Kind != models.RoutineEventWebhook || !webhook.Available {
		t.Fatalf("webhook source=%#v", webhook)
	}
	calendar := sources[slices.IndexFunc(sources, func(source *routine.RoutineEventSourceView) bool { return source.Kind == models.RoutineEventCalendar })]
	if calendar.Available || calendar.Reason != routine.RoutineEventSourceNeedsSignIn || calendar.SkillID != "google" {
		t.Fatalf("calendar source=%#v", calendar)
	}
	if len(sources) != 5 {
		t.Fatalf("sources=%d %#v", len(sources), sources)
	}

	filtered, err := f.Dispatch(t, f.Run, routine.RunbookActionEventSources, map[string]interface{}{"kind": "github.event"}, "github-sources")
	if err != nil {
		t.Fatal(err)
	}
	if only := filtered["sources"].([]*routine.RoutineEventSourceView); len(only) != 1 || only[0].Source != f.GitHubRef {
		t.Fatalf("filtered=%#v", only)
	}

	noHooks := routinetest.NewEventRoutine(t, false)
	result, err = noHooks.Dispatch(t, noHooks.Run, routine.RunbookActionEventSources, map[string]interface{}{"kind": "webhook"}, "webhooks")
	if err != nil {
		t.Fatal(err)
	}
	if listed := result["sources"].([]*routine.RoutineEventSourceView); len(listed) != 0 {
		t.Fatalf("webhook offered without hooks: %#v", listed)
	}

	checker := routine.NewRoutineEventSourceChecker(f.Store)
	for _, check := range []struct {
		kind      models.RoutineEventKind
		sourceRef string
		owner     models.ObjectiveOwner
		channels  []string
		want      bool
	}{
		{models.RoutineEventSlackMessage, f.SlackRef, f.Run.Owner, nil, true},
		{models.RoutineEventSlackMessage, f.SlackRef, f.Run.Owner, []string{"c1"}, true},
		{models.RoutineEventSlackMessage, f.SlackRef, f.Run.Owner, []string{"C1", "C2"}, false},
		{models.RoutineEventTelegramMessage, f.SlackRef, f.Run.Owner, nil, false},
		{models.RoutineEventSlackMessage, f.SlackRef, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "someone-else"}, nil, false},
		{models.RoutineEventGitHub, f.GitHubRef, f.Run.Owner, nil, true},
		{models.RoutineEventGitHub, models.RoutineCallbackSourceRef("reg-approvals"), f.Run.Owner, nil, false},
		{models.RoutineEventGitHub, models.RoutineCallbackSourceRef("missing"), f.Run.Owner, nil, false},
	} {
		subscription := models.RoutineEventSubscription{Scope: f.Run.Scope, Owner: check.owner, Kind: check.kind, SourceRef: check.sourceRef, Channels: check.channels}
		got, err := checker.SourceExists(t.Context(), subscription)
		if err != nil || got != check.want {
			t.Fatalf("SourceExists(%s, %s, %s, %v)=%v, %v", check.kind, check.sourceRef, check.owner.ID, check.channels, got, err)
		}
	}
}
