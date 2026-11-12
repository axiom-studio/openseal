package routinetest

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// NewEventRoutine is ScheduledTask with a Slack gateway and endpoints,
// GitHub callback registrations, a recording test event offerer, Google
// sources and the routine action validator and dispatcher wired to them.
func NewEventRoutine(t *testing.T, hooksAvailable bool) *EventRoutine {
	t.Helper()
	ctx := t.Context()
	store := kerneltest.NewStore(t)
	run, catalog, bound, args := ScheduledTask(t, store)
	now := time.Now().UTC()
	owner := run.Owner
	gateway := &models.ExternalConversationGatewayRegistration{
		ID: "gw-slack", IngressRoute: "route-gw-slack", Name: "Acme", Status: models.ExternalConversationGatewayActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Gateway:   models.ExternalConversationIngressGateway{Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Adapter: SlackAdapter, Provider: "slack", InstallationID: "T1"},
		Lifecycle: []models.ExternalConversationGatewayLifecycleEntry{{Revision: 1, Action: models.ExternalConversationGatewayCreated, Actor: models.ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
	}
	if err := store.CreateExternalConversationGateway(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	endpoint := func(id, name, address string, mode capability.ConversationEndpointMode) *models.ExternalConversationEndpoint {
		return &models.ExternalConversationEndpoint{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Name: name,
			Adapter: SlackAdapter, Provider: "slack", Mode: mode, InstallationID: "T1", Address: address,
			Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: owner.ID},
			Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectAllMessages, ReplyMode: models.ExternalConversationReplyProviderDefault},
			Status:  models.ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	for _, value := range []*models.ExternalConversationEndpoint{
		endpoint("ep-support", "#support", "C1", capability.ConversationEndpointChannel),
		endpoint("ep-dm", "Direct messages", "D1", capability.ConversationEndpointDirect),
	} {
		if err := store.CreateExternalConversationEndpoint(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	registration := func(id, name, deploymentID string, subscriptions ...models.CallbackSubscription) *models.CallbackRegistration {
		registrationOwner := owner
		registrationOwner.ID = deploymentID
		return &models.CallbackRegistration{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: registrationOwner, DeploymentID: deploymentID, Name: name, Provider: "github",
			Adapter:       models.CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github-binding", BindingRevision: 1, AdapterID: "callbacks"},
			Subscriptions: subscriptions, Status: models.CallbackRegistrationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
			Lifecycle: []models.CallbackRegistrationLifecycleEntry{{Revision: 1, Action: models.CallbackRegistrationCreated, Actor: models.ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
		}
	}
	for _, value := range []*models.CallbackRegistration{
		registration("reg-api", "acme/api", owner.ID, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}, models.CallbackSubscription{EventType: "github.pull_request.opened", Consumer: "routines"}),
		registration("reg-approvals", "acme/web", owner.ID, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}),
		registration("reg-other", "other/repo", "someone-else", models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}),
	} {
		if err := store.CreateCallbackRegistration(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	offerer := &TestOfferer{}
	google := GoogleSources{
		{Kind: models.RoutineEventGmailMessage, Source: models.RoutineGoogleSourceRef("user", "g-user"), Label: "your Gmail (user@acme.com)", Available: true, SkillID: "google"},
		{Kind: models.RoutineEventGmailMessage, Source: models.RoutineGoogleSourceRef("other", "g-other"), Label: "Gmail (other@acme.com)", Available: true, SkillID: "google"},
		{Kind: models.RoutineEventCalendar, Source: models.RoutineGoogleSourceRef("user", "g-user"), Label: "your Google Calendar (user@acme.com)", Available: false, Reason: routine.EventSourceNeedsSignIn, SkillID: "google"},
	}
	validator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	validator.SetRoutineEvents(offerer, google, hooksAvailable)
	dispatcher, err := routine.NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetRoutineEvents(offerer, google, hooksAvailable)
	return &EventRoutine{
		Store: store, Run: run, Catalog: catalog, Bound: bound, Args: args, Offerer: offerer, Google: google,
		Validator: validator, Dispatcher: dispatcher,
		SlackRef:  models.RoutineConversationSourceRef(owner.ID, SlackAdapter.BindingID, SlackAdapter.AdapterID, "T1"),
		GitHubRef: models.RoutineCallbackSourceRef("reg-api"),
	}
}

// GoogleSources returns its options whoever asks, so the
// kernel's own person check is what is tested.
type GoogleSources []routine.EventSourceOption

func (c GoogleSources) ListRoutineEventSources(context.Context, models.Scope, models.ObjectiveOwner, string) ([]routine.EventSourceOption, error) {
	return append([]routine.EventSourceOption(nil), c...), nil
}

// EventRoutine is the fixture NewEventRoutine builds.
type EventRoutine struct {
	Store      *sqlite.Store
	Run        *models.AgentRun
	Catalog    *skill.Catalog
	Bound      *skill.BoundAction
	Args       map[string]interface{}
	Offerer    *TestOfferer
	Google     GoogleSources
	Validator  *routine.RunbookActionValidator
	Dispatcher *routine.RunbookActionDispatcher
	SlackRef   string
	GitHubRef  string
}

func (f *EventRoutine) BoundAction(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.Catalog.Resolve(t.Context(), f.Bound.Binding.Scope, f.Run.AssignedAgentID, f.Bound.Definition.ID, f.Bound.Definition.Version, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// WithEvent returns create_task arguments whose trigger is this event.
func (f *EventRoutine) WithEvent(event map[string]interface{}) map[string]interface{} {
	args := maps.Clone(f.Args)
	args["trigger"] = map[string]interface{}{"event": event}
	return args
}

func (f *EventRoutine) Validate(t *testing.T, run *models.AgentRun, action string, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	bound := f.BoundAction(t, action)
	if err := f.Catalog.ValidateInput(t.Context(), bound, args); err != nil {
		t.Fatalf("arguments break the %s contract: %v", action, err)
	}
	return f.Validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args})
}

func (f *EventRoutine) Dispatch(t *testing.T, run *models.AgentRun, action string, args map[string]interface{}, callID string) (map[string]interface{}, error) {
	t.Helper()
	bound := f.BoundAction(t, action)
	result, err := f.Dispatcher.DispatchAction(t.Context(), agentrun.ActionDispatchInput{Run: run, Bound: bound, Call: &models.ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: args})
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
	if err := f.Catalog.ValidateOutput(t.Context(), bound, decoded); err != nil {
		t.Fatalf("%s result contract: %v", action, err)
	}
	return result, nil
}

func (f *EventRoutine) SlackEvent(channels ...string) map[string]interface{} {
	condition := map[string]interface{}{"field": "channel", "op": "equals", "value": channels[0]}
	if len(channels) > 1 {
		values := make([]interface{}, 0, len(channels))
		for _, channel := range channels {
			values = append(values, channel)
		}
		condition = map[string]interface{}{"field": "channel", "op": "one_of", "values": values}
	}
	return map[string]interface{}{"kind": "slack.message", "source": f.SlackRef, "filter": []interface{}{condition}}
}

// SetRoutineStatus runs set_status the way a chat does.
func (f *EventRoutine) SetRoutineStatus(t *testing.T, activationID string, status models.RunbookActivationStatus, callID string) (*models.RunbookActivation, error) {
	t.Helper()
	current, err := f.Store.GetRunbookActivation(t.Context(), f.Run.Scope, activationID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.Dispatch(t, f.Run, models.RunbookActionSetStatus, map[string]interface{}{
		"activationId": activationID, "expectedRevision": float64(current.Revision), "status": string(status), "reason": "test",
	}, callID)
	if err != nil {
		return nil, err
	}
	return result["activation"].(*models.RunbookActivation), nil
}

func (f *EventRoutine) SetEndpointStatus(t *testing.T, id string, status models.ExternalConversationEndpointStatus) {
	t.Helper()
	endpoint, err := f.Store.GetExternalConversationEndpoint(t.Context(), f.Run.Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	next := *endpoint
	next.Status, next.Revision = status, endpoint.Revision+1
	if err := f.Store.UpdateExternalConversationEndpoint(t.Context(), &next, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
}

// SlackAdapter is the Slack adapter of the fixture's gateway and endpoints.
var SlackAdapter = models.ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "events"}

// TestOfferer records test events instead of matching them.
type TestOfferer struct {
	Tests []TestOffer
}

func (o *TestOfferer) Listening(models.Scope, models.RoutineEventKind, string) bool {
	return false
}

func (o *TestOfferer) OfferRoutineEvent(context.Context, *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	return externalconversation.RoutineEventOffer{}, nil
}

func (o *TestOfferer) OfferRoutineTestEvent(_ context.Context, scope models.Scope, activationID, testBy string, payload json.RawMessage) (string, externalconversation.RoutineEventOffer, error) {
	o.Tests = append(o.Tests, TestOffer{Scope: scope, ActivationID: activationID, TestBy: testBy, Payload: payload})
	return "test:fixed", externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}, nil
}

// TestOffer is one test event TestOfferer recorded.
type TestOffer struct {
	Scope        models.Scope
	ActivationID string
	TestBy       string
	Payload      json.RawMessage
}
