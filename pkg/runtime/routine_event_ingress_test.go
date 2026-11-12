package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation/externalconversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// routineConnectionFixture is a Slack or Telegram endpoint in the harness
// store, owned by the harness owner, on installation T123 and channel C123.
func routineConnectionFixture(t *testing.T, h *routinetest.EventHarness, provider string, selection models.ExternalConversationMessageSelection) (*skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Provider, adapter.Name = provider, provider+" conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(h.Ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: provider}
	binding := &skill.Binding{
		ID: provider, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: h.Owner.ID,
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/" + provider}},
	}
	if err := catalog.Bind(h.Ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := externalconversation.NewExternalConversationEndpointService(h.Store, catalog).Create(h.Ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: provider + "-endpoint", Scope: scope, Owner: h.Owner, DeploymentID: h.Owner.ID, Name: provider + " endpoint",
		Adapter: models.ExternalConversationAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "C123", InstallationID: "T123", ApplicationID: "A123",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: h.Owner.ID},
		Policy:  models.ExternalConversationPolicy{MessageSelection: selection, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true},
		Status:  models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The routine's chat lives in the endpoint's scope.
	h.Scope = endpoint.Scope
	seedTestSpace(h.T, h.Store, h.Scope)
	h.Conversation, h.Message = h.Chat(provider + " routines")
	return catalog, endpoint
}

// conversationRoutine creates an active routine on the endpoint's connection
// that listens to channel C123.
func conversationRoutine(h *routinetest.EventHarness, endpoint *models.ExternalConversationEndpoint, kind models.RoutineEventKind, field string) *models.RunbookActivation {
	return h.Routine(string(kind), func(trigger *models.RoutineEventTrigger) {
		*trigger = models.RoutineEventTrigger{
			Kind:        kind,
			SourceRef:   models.RoutineConversationSourceRef(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID, endpoint.InstallationID),
			SourceLabel: "Acme " + endpoint.Provider, MaxPerHour: 20,
			Filter: []models.RoutineEventCondition{{Field: field, Op: models.RoutineFilterEquals, Value: "C123", Label: "#support"}},
		}
	})
}

// waitingRoutineEvents claims the routine's waiting events as one batch and
// returns their records in order.
func waitingRoutineEvents(t *testing.T, h *routinetest.EventHarness, activation *models.RunbookActivation) []*models.RoutineEventRecord {
	t.Helper()
	batch, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil {
		return nil
	}
	_, records, err := h.Store.GetRoutineEventBatch(h.Ctx, h.Scope, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*models.RoutineEventRecord{}
	for _, record := range records {
		byID[record.Event.ID] = record
	}
	ordered := make([]*models.RoutineEventRecord, 0, len(batch.EventIDs))
	for _, id := range batch.EventIDs {
		ordered = append(ordered, byID[id])
	}
	return ordered
}

func slackRoutineMessage(id, participant, name, text string, bot bool) models.NormalizedExternalConversationEvent {
	return models.NormalizedExternalConversationEvent{
		ID: "slack:message:T123:C123:" + id, Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: id, ExternalThreadID: id,
		ExternalParticipantID: participant, ParticipantDisplayName: name, ParticipantIsBot: bot,
		Text: text, OrderingKey: "C123:" + id, OccurredAt: time.Now().UTC(),
	}
}

func TestSlackMessageOffersRoutineEvent(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	// The endpoint answers only mentions, so these messages get no chat
	// reply; the routine hears them anyway (E14).
	catalog, endpoint := routineConnectionFixture(t, h, "slack", models.ExternalConversationSelectMentions)
	activation := conversationRoutine(h, endpoint, models.RoutineEventSlackMessage, "channel")
	gateways := externalconversation.NewExternalConversationGatewayService(h.Store, nil)
	registration := externalconversationtest.CreateActiveGateway(t, h.Ctx, gateways, externalconversation.CreateExternalConversationGatewayRequest{
		ID: "acme-slack", Name: "Acme Slack", Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "pinned tenant gateway",
		Gateway: models.ExternalConversationIngressGateway{
			Scope: endpoint.Scope, Owner: h.Owner, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter,
			Provider: "slack", InstallationID: "T123", ApplicationID: "A123",
		},
	})
	human := slackRoutineMessage("171.001", "U1", "Ana Ruiz", "The invoice\nis late", false)
	bot := slackRoutineMessage("171.002", "B1", "Deploy bot", "Deployed", true)
	host := &externalconversationtest.GatewayHostStub{Result: &externalconversation.ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []externalconversation.ExternalConversationGatewayEvent{
			{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: human},
			{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: bot},
		},
	}}
	service := externalconversation.NewExternalConversationTransportService(h.Store, catalog)
	service.SetRoutineEventOfferer(h.Matcher)

	result, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.Ctx, externalconversation.ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"team_id":"T123"}`),
	}, host)
	if err != nil || len(result.Received) != 2 || result.Received[0].Accepted || result.Received[1].Accepted {
		t.Fatalf("gateway ingress = %#v err=%v", result, err)
	}

	stats := h.Matcher.RoutineEventStats()
	if stats.In[models.RoutineEventSlackMessage] != 2 || stats.Matched[models.RoutineEventSlackMessage] != 1 {
		t.Fatalf("slack stats in=%d matched=%d, want 2 and 1 (bot excluded)", stats.In[models.RoutineEventSlackMessage], stats.Matched[models.RoutineEventSlackMessage])
	}
	records := waitingRoutineEvents(t, h, activation)
	if len(records) != 1 {
		t.Fatalf("waiting events = %d, want 1", len(records))
	}
	event := records[0].Event
	if event.ID != human.ID || event.Summary != "Ana Ruiz: The invoice is late" || event.Subject != "C123" {
		t.Fatalf("event = %#v", event)
	}
	want := map[string]string{"channel": "C123", "sender": "U1", "senderName": "Ana Ruiz", "text": "The invoice\nis late", "inThread": "false", "bot": "false"}
	for field, value := range want {
		if values := event.Fields[field]; len(values) != 1 || values[0] != value {
			t.Fatalf("field %s = %#v, want %q", field, values, value)
		}
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["text"] != "The invoice\nis late" || payload["externalMessageId"] != "171.001" {
		t.Fatalf("payload = %s err=%v", event.Payload, err)
	}

	// A retry of the same provider request is absorbed by dedupe.
	if _, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.Ctx, externalconversation.ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"team_id":"T123"}`),
	}, host); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.Store.CountPendingRoutineEvents(h.Ctx, h.Scope, activation.ID); err != nil || pending != 1 {
		t.Fatalf("pending after retry = %d err=%v", pending, err)
	}
}

func TestSlackRoutineIgnoresDirectMessages(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	catalog, endpoint := routineConnectionFixture(t, h, "slack", models.ExternalConversationSelectAllMessages)
	activation := conversationRoutine(h, endpoint, models.RoutineEventSlackMessage, "channel")
	direct := slackRoutineMessage("172.001", "U1", "Ana Ruiz", "private note", false)
	direct.Direct = true
	host := &externalconversationtest.IngressHostStub{Result: &externalconversation.ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK, Events: []models.NormalizedExternalConversationEvent{direct},
	}}
	service := externalconversation.NewExternalConversationTransportService(h.Store, catalog)
	service.SetRoutineEventOfferer(h.Matcher)
	request := externalconversation.ExternalConversationIngressRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: http.MethodPost, Body: []byte(`{}`)}

	if _, err := service.NormalizeExternalConversationIngress(h.Ctx, request, host); err != nil {
		t.Fatal(err)
	}
	if stats := h.Matcher.RoutineEventStats(); stats.In[models.RoutineEventSlackMessage] != 0 {
		t.Fatalf("a direct message reached the matcher: in=%d", stats.In[models.RoutineEventSlackMessage])
	}

	// The same endpoint path offers a channel message.
	host.Result.Events = []models.NormalizedExternalConversationEvent{slackRoutineMessage("172.002", "U1", "Ana Ruiz", "in the channel", false)}
	if _, err := service.NormalizeExternalConversationIngress(h.Ctx, request, host); err != nil {
		t.Fatal(err)
	}
	records := waitingRoutineEvents(t, h, activation)
	if len(records) != 1 || records[0].Event.Fields["text"][0] != "in the channel" {
		t.Fatalf("waiting events = %#v", records)
	}
}

func TestTelegramMessageOffersRoutineEvent(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	catalog, endpoint := routineConnectionFixture(t, h, "telegram", models.ExternalConversationSelectAllMessages)
	activation := conversationRoutine(h, endpoint, models.RoutineEventTelegramMessage, "chat")
	message := models.NormalizedExternalConversationEvent{
		ID: "telegram:update:991", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: "C123:55", ExternalThreadID: "C123",
		ExternalParticipantID: "42", ParticipantDisplayName: "Ana", Text: "Order 7 shipped?",
		OrderingKey: "C123:C123", OccurredAt: time.Now().UTC(),
		Attachments: []models.ExternalConversationAttachment{{ID: "file-1", Name: "label.pdf", MediaType: "application/pdf", SizeBytes: 10}},
		Attributes:  map[string]interface{}{"chatTitle": "Support desk", "participantUsername": "ana_r"},
	}
	// An unpinned gateway: authority comes from each mapped endpoint.
	gateways := externalconversation.NewExternalConversationGatewayService(h.Store, nil)
	registration := externalconversationtest.CreateActiveGateway(t, h.Ctx, gateways, externalconversation.CreateExternalConversationGatewayRequest{
		ID: "telegram-bot", Name: "Telegram bot", Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "unpinned gateway",
		Gateway: models.ExternalConversationIngressGateway{
			Scope: endpoint.Scope, Owner: h.Owner, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: "telegram",
		},
	})
	host := &externalconversationtest.GatewayHostStub{Result: &externalconversation.ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events:     []externalconversation.ExternalConversationGatewayEvent{{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: message}},
	}}
	service := externalconversation.NewExternalConversationTransportService(h.Store, catalog)
	service.SetRoutineEventOfferer(h.Matcher)

	result, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.Ctx, externalconversation.ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"update_id":991}`),
	}, host)
	if err != nil || len(result.Received) != 1 {
		t.Fatalf("gateway ingress = %#v err=%v", result, err)
	}
	records := waitingRoutineEvents(t, h, activation)
	if len(records) != 1 {
		t.Fatalf("waiting events = %d, want 1", len(records))
	}
	event := records[0].Event
	if event.ID != "telegram:update:991" || event.Kind != models.RoutineEventTelegramMessage || event.Summary != "Ana: Order 7 shipped?" {
		t.Fatalf("event = %#v", event)
	}
	want := map[string]string{"chat": "C123", "chatTitle": "Support desk", "sender": "42", "senderName": "Ana", "senderUsername": "ana_r", "bot": "false"}
	for field, value := range want {
		if values := event.Fields[field]; len(values) != 1 || values[0] != value {
			t.Fatalf("field %s = %#v, want %q", field, values, value)
		}
	}
	if _, ok := event.Fields["inThread"]; ok {
		t.Fatal("telegram events have no inThread field")
	}
	if !strings.Contains(string(event.Payload), `"attachments":[{"name":"label.pdf","mimeType":"application/pdf"}]`) ||
		!strings.Contains(string(event.Payload), `"username":"ana_r"`) {
		t.Fatalf("payload = %s", event.Payload)
	}
}

func TestGitHubCallbackRoutinesConsumer(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	h.CreateCallbackRegistration("repo", h.Owner.ID, models.CallbackRegistrationActive)
	h.CreateCallbackRegistration("quiet", h.Owner.ID, models.CallbackRegistrationActive)
	activation := h.Routine("github", func(trigger *models.RoutineEventTrigger) {
		*trigger = models.RoutineEventTrigger{Kind: models.RoutineEventGitHub, SourceRef: models.RoutineCallbackSourceRef("callback-repo"), SourceLabel: "acme/api (GitHub)", MaxPerHour: 20,
			Filter: []models.RoutineEventCondition{{Field: "event", Op: models.RoutineFilterEquals, Value: "issues.opened"}}}
	})
	registration, err := h.Store.GetCallbackRegistration(h.Ctx, h.Scope, "callback-repo")
	if err != nil || registration == nil {
		t.Fatalf("registration = %#v err=%v", registration, err)
	}
	// The shape the GitHub skill's callback adapter emits, reloaded from the
	// receipt with json.Number.
	issue := models.EventEnvelope{
		ID: "github:delivery-1", Scope: h.Scope, Type: "github.issues.opened", Source: "github", Subject: "acme/api#12",
		OccurredAt: h.Now, Actor: models.ActivityActor{Type: "callback", ID: registration.ID},
		Attributes: map[string]interface{}{"owner": "acme", "repository": "api", "action": "opened", "number": json.Number("12")},
		Payload: map[string]interface{}{
			"repository": map[string]interface{}{"owner": "acme", "name": "api", "fullName": "acme/api"},
			"sender":     "octocat",
			"issue": map[string]interface{}{
				"number": json.Number("12"), "title": "Fix login", "body": "It fails",
				"labels": []interface{}{map[string]interface{}{"name": "bug"}, map[string]interface{}{"name": "p1"}},
			},
		},
	}
	offered := &recordingRoutineOfferer{RoutineEventOfferer: h.Matcher}
	consumer := externalconversation.NewRoutineCallbackConsumer(offered)
	subscription := models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}

	if err := consumer.ConsumeCallbackEvent(h.Ctx, registration, subscription, issue); err != nil {
		t.Fatal(err)
	}
	// A registration no routine listens to is skipped before any mapping.
	quiet, _ := h.Store.GetCallbackRegistration(h.Ctx, h.Scope, "callback-quiet")
	if err := consumer.ConsumeCallbackEvent(h.Ctx, quiet, subscription, issue); err != nil {
		t.Fatal(err)
	}
	// Other providers are not routine events.
	other := *registration
	other.Provider = "gitlab"
	if err := consumer.ConsumeCallbackEvent(h.Ctx, &other, subscription, issue); err != nil {
		t.Fatal(err)
	}
	if stats := h.Matcher.RoutineEventStats(); stats.In[models.RoutineEventGitHub] != 1 || stats.Matched[models.RoutineEventGitHub] != 1 {
		t.Fatalf("github stats in=%d matched=%d", stats.In[models.RoutineEventGitHub], stats.Matched[models.RoutineEventGitHub])
	}
	records := waitingRoutineEvents(t, h, activation)
	if len(records) != 1 {
		t.Fatalf("waiting events = %d, want 1", len(records))
	}
	event := records[0].Event
	if event.ID != "github:delivery-1" || event.Summary != "Issue opened in acme/api: #12 Fix login" || event.Subject != "acme/api#12" {
		t.Fatalf("event = %#v", event)
	}
	want := map[string][]string{
		"event": {"issues.opened"}, "repository": {"acme/api"}, "action": {"opened"}, "sender": {"octocat"},
		"title": {"Fix login"}, "labels": {"bug", "p1"},
	}
	for field, values := range want {
		if got := event.Fields[field]; strings.Join(got, ",") != strings.Join(values, ",") {
			t.Fatalf("field %s = %#v, want %#v", field, got, values)
		}
	}
	if !strings.Contains(string(event.Payload), `"title":"Fix login"`) {
		t.Fatalf("payload = %s", event.Payload)
	}

	// A workflow run that this routine's filter does not take is offered but
	// not matched.
	run := issue
	run.ID, run.Type, run.Subject = "github:delivery-2", "github.workflow_run.completed", "acme/api/actions/runs/9"
	run.Attributes = map[string]interface{}{"owner": "acme", "repository": "api", "action": "completed", "runId": json.Number("9")}
	run.Payload = map[string]interface{}{
		"repository":  map[string]interface{}{"fullName": "acme/api"},
		"workflowRun": map[string]interface{}{"name": "CI", "branch": "main", "conclusion": "failure"},
	}
	if err := consumer.ConsumeCallbackEvent(h.Ctx, registration, models.CallbackSubscription{EventType: run.Type, Consumer: "routines"}, run); err != nil {
		t.Fatal(err)
	}
	mapped := offered.events[len(offered.events)-1]
	if mapped.ID != run.ID || mapped.SourceRef != models.RoutineCallbackSourceRef(registration.ID) ||
		mapped.Summary != "Workflow run completed in acme/api: CI (failure)" || mapped.Fields["branch"][0] != "main" || mapped.Fields["conclusion"][0] != "failure" {
		t.Fatalf("workflow run event = %#v", mapped)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatal(err)
	}
	if stats := h.Matcher.RoutineEventStats(); stats.In[models.RoutineEventGitHub] != 2 || stats.Matched[models.RoutineEventGitHub] != 1 {
		t.Fatalf("github stats in=%d matched=%d", stats.In[models.RoutineEventGitHub], stats.Matched[models.RoutineEventGitHub])
	}
}

// recordingRoutineOfferer records each event offered to its offerer.
type recordingRoutineOfferer struct {
	externalconversation.RoutineEventOfferer
	events []*models.RoutineEvent
}

func (o *recordingRoutineOfferer) OfferRoutineEvent(ctx context.Context, event *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	o.events = append(o.events, event)
	return o.RoutineEventOfferer.OfferRoutineEvent(ctx, event)
}
