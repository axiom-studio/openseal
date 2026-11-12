package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// routineTestOfferer records test events instead of matching them.
type routineTestOfferer struct {
	tests []routineTestOffer
}

type routineTestOffer struct {
	scope        Scope
	activationID string
	testBy       string
	payload      json.RawMessage
}

func (o *routineTestOfferer) Listening(Scope, RoutineEventKind, string) bool { return false }

func (o *routineTestOfferer) OfferRoutineEvent(context.Context, *RoutineEvent) (RoutineEventOffer, error) {
	return RoutineEventOffer{}, nil
}

func (o *routineTestOfferer) OfferRoutineTestEvent(_ context.Context, scope Scope, activationID, testBy string, payload json.RawMessage) (string, RoutineEventOffer, error) {
	o.tests = append(o.tests, routineTestOffer{scope: scope, activationID: activationID, testBy: testBy, payload: payload})
	return "test:fixed", RoutineEventOffer{Matched: 1, Inserted: 1}, nil
}

// routineGoogleSourcesForTest returns its options whoever asks, so the
// kernel's own person check is what is tested.
type routineGoogleSourcesForTest []RoutineEventSourceOption

func (c routineGoogleSourcesForTest) ListRoutineEventSources(context.Context, Scope, ObjectiveOwner, string) ([]RoutineEventSourceOption, error) {
	return append([]RoutineEventSourceOption(nil), c...), nil
}

type eventRoutineFixture struct {
	store      *sqlite.Store
	run        *AgentRun
	catalog    *skill.Catalog
	bound      *skill.BoundAction
	args       map[string]interface{}
	offerer    *routineTestOfferer
	google     routineGoogleSourcesForTest
	validator  *RunbookActionValidator
	dispatcher *RunbookActionDispatcher
	slackRef   string
	githubRef  string
}

var routineSlackAdapterForTest = ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "events"}

func newEventRoutineFixture(t *testing.T, hooksAvailable bool) *eventRoutineFixture {
	t.Helper()
	ctx := t.Context()
	store := newTestKernelStore(t)
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	now := time.Now().UTC()
	owner := run.Owner
	gateway := &ExternalConversationGatewayRegistration{
		ID: "gw-slack", IngressRoute: "route-gw-slack", Name: "Acme", Status: ExternalConversationGatewayActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Gateway:   ExternalConversationIngressGateway{Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Adapter: routineSlackAdapterForTest, Provider: "slack", InstallationID: "T1"},
		Lifecycle: []ExternalConversationGatewayLifecycleEntry{{Revision: 1, Action: ExternalConversationGatewayCreated, Actor: ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
	}
	if err := store.CreateExternalConversationGateway(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	endpoint := func(id, name, address string, mode capability.ConversationEndpointMode) *ExternalConversationEndpoint {
		return &ExternalConversationEndpoint{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Name: name,
			Adapter: routineSlackAdapterForTest, Provider: "slack", Mode: mode, InstallationID: "T1", Address: address,
			Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: owner.ID},
			Policy:  ExternalConversationPolicy{MessageSelection: ExternalConversationSelectAllMessages, ReplyMode: ExternalConversationReplyProviderDefault},
			Status:  ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	for _, value := range []*ExternalConversationEndpoint{
		endpoint("ep-support", "#support", "C1", capability.ConversationEndpointChannel),
		endpoint("ep-dm", "Direct messages", "D1", capability.ConversationEndpointDirect),
	} {
		if err := store.CreateExternalConversationEndpoint(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	registration := func(id, name, deploymentID string, subscriptions ...CallbackSubscription) *CallbackRegistration {
		registrationOwner := owner
		registrationOwner.ID = deploymentID
		return &CallbackRegistration{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: registrationOwner, DeploymentID: deploymentID, Name: name, Provider: "github",
			Adapter:       CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github-binding", BindingRevision: 1, AdapterID: "callbacks"},
			Subscriptions: subscriptions, Status: CallbackRegistrationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
			Lifecycle: []CallbackRegistrationLifecycleEntry{{Revision: 1, Action: CallbackRegistrationCreated, Actor: ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
		}
	}
	for _, value := range []*CallbackRegistration{
		registration("reg-api", "acme/api", owner.ID, CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}, CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}, CallbackSubscription{EventType: "github.pull_request.opened", Consumer: "routines"}),
		registration("reg-approvals", "acme/web", owner.ID, CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}),
		registration("reg-other", "other/repo", "someone-else", CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}),
	} {
		if err := store.CreateCallbackRegistration(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	offerer := &routineTestOfferer{}
	google := routineGoogleSourcesForTest{
		{Kind: RoutineEventGmailMessage, Source: RoutineGoogleSourceRef("user", "g-user"), Label: "your Gmail (user@acme.com)", Available: true, SkillID: "google"},
		{Kind: RoutineEventGmailMessage, Source: RoutineGoogleSourceRef("other", "g-other"), Label: "Gmail (other@acme.com)", Available: true, SkillID: "google"},
		{Kind: RoutineEventCalendar, Source: RoutineGoogleSourceRef("user", "g-user"), Label: "your Google Calendar (user@acme.com)", Available: false, Reason: RoutineEventSourceNeedsSignIn, SkillID: "google"},
	}
	validator, err := NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	validator.SetRoutineEvents(offerer, google, hooksAvailable)
	dispatcher, err := NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetRoutineEvents(offerer, google, hooksAvailable)
	return &eventRoutineFixture{
		store: store, run: run, catalog: catalog, bound: bound, args: args, offerer: offerer, google: google,
		validator: validator, dispatcher: dispatcher,
		slackRef:  RoutineConversationSourceRef(owner.ID, routineSlackAdapterForTest.BindingID, routineSlackAdapterForTest.AdapterID, "T1"),
		githubRef: RoutineCallbackSourceRef("reg-api"),
	}
}

func (f *eventRoutineFixture) boundAction(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(t.Context(), f.bound.Binding.Scope, f.run.AssignedAgentID, f.bound.Definition.ID, f.bound.Definition.Version, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// withEvent returns create_task arguments whose trigger is this event.
func (f *eventRoutineFixture) withEvent(event map[string]interface{}) map[string]interface{} {
	args := cloneMap(f.args)
	args["trigger"] = map[string]interface{}{"event": event}
	return args
}

func (f *eventRoutineFixture) validate(t *testing.T, run *AgentRun, action string, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	bound := f.boundAction(t, action)
	if err := f.catalog.ValidateInput(t.Context(), bound, args); err != nil {
		t.Fatalf("arguments break the %s contract: %v", action, err)
	}
	return f.validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args})
}

func (f *eventRoutineFixture) dispatch(t *testing.T, run *AgentRun, action string, args map[string]interface{}, callID string) (map[string]interface{}, error) {
	t.Helper()
	bound := f.boundAction(t, action)
	result, err := f.dispatcher.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: bound, Call: &ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: args})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.ValidateOutput(t.Context(), bound, decoded); err != nil {
		t.Fatalf("%s result contract: %v", action, err)
	}
	return result, nil
}

func TestEventSourcesListsConnections(t *testing.T) {
	f := newEventRoutineFixture(t, true)
	result, err := f.dispatch(t, f.run, RunbookActionEventSources, map[string]interface{}{}, "sources")
	if err != nil {
		t.Fatal(err)
	}
	sources := result["sources"].([]*RoutineEventSourceView)
	bySource := map[string]*RoutineEventSourceView{}
	for _, source := range sources {
		if source.Kind != RoutineEventCalendar {
			bySource[source.Source] = source
		}
	}
	slack := bySource[f.slackRef]
	if slack == nil || slack.Kind != RoutineEventSlackMessage || slack.Label != "Acme Slack" || !slack.Available ||
		!reflect.DeepEqual(slack.Channels, []RoutineEventSourceChannel{{ID: "C1", Name: "#support"}}) {
		t.Fatalf("slack source=%#v", slack)
	}
	github := bySource[f.githubRef]
	if github == nil || github.Kind != RoutineEventGitHub || github.Label != "acme/api (GitHub)" || !reflect.DeepEqual(github.EventTypes, []string{"issues.opened", "pull_request.opened"}) {
		t.Fatalf("github source=%#v", github)
	}
	for _, missing := range []string{RoutineCallbackSourceRef("reg-approvals"), RoutineCallbackSourceRef("reg-other"), RoutineGoogleSourceRef("other", "g-other")} {
		if bySource[missing] != nil {
			t.Fatalf("listed %s", missing)
		}
	}
	gmail := bySource[RoutineGoogleSourceRef("user", "g-user")]
	if gmail == nil || gmail.Kind != RoutineEventGmailMessage || !gmail.Available || !reflect.DeepEqual(gmail.Fields, RoutineEventFilterFields(RoutineEventGmailMessage)) {
		t.Fatalf("gmail source=%#v", gmail)
	}
	if webhook := bySource[RoutineEventSourceNew]; webhook == nil || webhook.Kind != RoutineEventWebhook || !webhook.Available {
		t.Fatalf("webhook source=%#v", webhook)
	}
	calendar := sources[slices.IndexFunc(sources, func(source *RoutineEventSourceView) bool { return source.Kind == RoutineEventCalendar })]
	if calendar.Available || calendar.Reason != RoutineEventSourceNeedsSignIn || calendar.SkillID != "google" {
		t.Fatalf("calendar source=%#v", calendar)
	}
	if len(sources) != 5 {
		t.Fatalf("sources=%d %#v", len(sources), sources)
	}

	filtered, err := f.dispatch(t, f.run, RunbookActionEventSources, map[string]interface{}{"kind": "github.event"}, "github-sources")
	if err != nil {
		t.Fatal(err)
	}
	if only := filtered["sources"].([]*RoutineEventSourceView); len(only) != 1 || only[0].Source != f.githubRef {
		t.Fatalf("filtered=%#v", only)
	}

	noHooks := newEventRoutineFixture(t, false)
	result, err = noHooks.dispatch(t, noHooks.run, RunbookActionEventSources, map[string]interface{}{"kind": "webhook"}, "webhooks")
	if err != nil {
		t.Fatal(err)
	}
	if listed := result["sources"].([]*RoutineEventSourceView); len(listed) != 0 {
		t.Fatalf("webhook offered without hooks: %#v", listed)
	}

	checker := NewRoutineEventSourceChecker(f.store)
	for _, check := range []struct {
		kind      RoutineEventKind
		sourceRef string
		owner     ObjectiveOwner
		channels  []string
		want      bool
	}{
		{RoutineEventSlackMessage, f.slackRef, f.run.Owner, nil, true},
		{RoutineEventSlackMessage, f.slackRef, f.run.Owner, []string{"c1"}, true},
		{RoutineEventSlackMessage, f.slackRef, f.run.Owner, []string{"C1", "C2"}, false},
		{RoutineEventTelegramMessage, f.slackRef, f.run.Owner, nil, false},
		{RoutineEventSlackMessage, f.slackRef, ObjectiveOwner{Type: OwnerTypeAgent, ID: "someone-else"}, nil, false},
		{RoutineEventGitHub, f.githubRef, f.run.Owner, nil, true},
		{RoutineEventGitHub, RoutineCallbackSourceRef("reg-approvals"), f.run.Owner, nil, false},
		{RoutineEventGitHub, RoutineCallbackSourceRef("missing"), f.run.Owner, nil, false},
	} {
		subscription := RoutineEventSubscription{Scope: f.run.Scope, Owner: check.owner, Kind: check.kind, SourceRef: check.sourceRef, Channels: check.channels}
		got, err := checker.SourceExists(t.Context(), subscription)
		if err != nil || got != check.want {
			t.Fatalf("SourceExists(%s, %s, %s, %v)=%v, %v", check.kind, check.sourceRef, check.owner.ID, check.channels, got, err)
		}
	}
}
