package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// routineConnectionFixture is a Slack or Telegram endpoint in the harness
// store, owned by the harness owner, on installation T123 and channel C123.
func routineConnectionFixture(t *testing.T, h *routineEventHarness, provider string, selection ExternalConversationMessageSelection) (*skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := slackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Provider, adapter.Name = provider, provider+" conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(h.ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: provider}
	binding := &skill.Binding{
		ID: provider, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: h.owner.ID,
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/" + provider}},
	}
	if err := catalog.Bind(h.ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := NewExternalConversationEndpointService(h.store, catalog).Create(h.ctx, CreateExternalConversationEndpointRequest{
		ID: provider + "-endpoint", Scope: scope, Owner: h.owner, DeploymentID: h.owner.ID, Name: provider + " endpoint",
		Adapter: ExternalConversationAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "C123", InstallationID: "T123", ApplicationID: "A123",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: h.owner.ID},
		Policy:  ExternalConversationPolicy{MessageSelection: selection, ReplyMode: ExternalConversationReplyThread, IgnoreBots: true},
		Status:  ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The routine's chat lives in the endpoint's scope.
	h.scope = endpoint.Scope
	seedTestSpace(h.t, h.store, h.scope)
	h.conversation, h.message = h.chat(provider + " routines")
	return catalog, endpoint
}

// conversationRoutine creates an active routine on the endpoint's connection
// that listens to channel C123.
func conversationRoutine(h *routineEventHarness, endpoint *ExternalConversationEndpoint, kind RoutineEventKind, field string) *RunbookActivation {
	return h.routine(string(kind), func(trigger *RoutineEventTrigger) {
		*trigger = RoutineEventTrigger{
			Kind:        kind,
			SourceRef:   RoutineConversationSourceRef(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID, endpoint.InstallationID),
			SourceLabel: "Acme " + endpoint.Provider, MaxPerHour: 20,
			Filter: []RoutineEventCondition{{Field: field, Op: RoutineFilterEquals, Value: "C123", Label: "#support"}},
		}
	})
}

// waitingRoutineEvents claims the routine's waiting events as one batch and
// returns their records in order.
func waitingRoutineEvents(t *testing.T, h *routineEventHarness, activation *RunbookActivation) []*RoutineEventRecord {
	t.Helper()
	batch, err := h.store.ClaimRoutineEventBatch(h.ctx, h.scope, activation.ID, RoutineEventMaxBatchEvents, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil {
		return nil
	}
	_, records, err := h.store.GetRoutineEventBatch(h.ctx, h.scope, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*RoutineEventRecord{}
	for _, record := range records {
		byID[record.Event.ID] = record
	}
	ordered := make([]*RoutineEventRecord, 0, len(batch.EventIDs))
	for _, id := range batch.EventIDs {
		ordered = append(ordered, byID[id])
	}
	return ordered
}

func slackRoutineMessage(id, participant, name, text string, bot bool) NormalizedExternalConversationEvent {
	return NormalizedExternalConversationEvent{
		ID: "slack:message:T123:C123:" + id, Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: id, ExternalThreadID: id,
		ExternalParticipantID: participant, ParticipantDisplayName: name, ParticipantIsBot: bot,
		Text: text, OrderingKey: "C123:" + id, OccurredAt: time.Now().UTC(),
	}
}

func TestSlackMessageOffersRoutineEvent(t *testing.T) {
	h := newRoutineEventHarness(t)
	// The endpoint answers only mentions, so these messages get no chat
	// reply; the routine hears them anyway (E14).
	catalog, endpoint := routineConnectionFixture(t, h, "slack", ExternalConversationSelectMentions)
	activation := conversationRoutine(h, endpoint, RoutineEventSlackMessage, "channel")
	gateways := NewExternalConversationGatewayService(h.store)
	registration := createActiveExternalConversationGateway(t, h.ctx, gateways, CreateExternalConversationGatewayRequest{
		ID: "acme-slack", Name: "Acme Slack", Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "pinned tenant gateway",
		Gateway: ExternalConversationIngressGateway{
			Scope: endpoint.Scope, Owner: h.owner, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter,
			Provider: "slack", InstallationID: "T123", ApplicationID: "A123",
		},
	})
	human := slackRoutineMessage("171.001", "U1", "Ana Ruiz", "The invoice\nis late", false)
	bot := slackRoutineMessage("171.002", "B1", "Deploy bot", "Deployed", true)
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []ExternalConversationGatewayEvent{
			{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: human},
			{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: bot},
		},
	}}
	service := NewExternalConversationTransportService(h.store, catalog)
	service.SetRoutineEventOfferer(h.matcher)

	result, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"team_id":"T123"}`),
	}, host)
	if err != nil || len(result.Received) != 2 || result.Received[0].Accepted || result.Received[1].Accepted {
		t.Fatalf("gateway ingress = %#v err=%v", result, err)
	}

	stats := h.matcher.RoutineEventStats()
	if stats.In[RoutineEventSlackMessage] != 2 || stats.Matched[RoutineEventSlackMessage] != 1 {
		t.Fatalf("slack stats in=%d matched=%d, want 2 and 1 (bot excluded)", stats.In[RoutineEventSlackMessage], stats.Matched[RoutineEventSlackMessage])
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
	if _, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"team_id":"T123"}`),
	}, host); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.store.CountPendingRoutineEvents(h.ctx, h.scope, activation.ID); err != nil || pending != 1 {
		t.Fatalf("pending after retry = %d err=%v", pending, err)
	}
}

func TestSlackRoutineIgnoresDirectMessages(t *testing.T) {
	h := newRoutineEventHarness(t)
	catalog, endpoint := routineConnectionFixture(t, h, "slack", ExternalConversationSelectAllMessages)
	activation := conversationRoutine(h, endpoint, RoutineEventSlackMessage, "channel")
	direct := slackRoutineMessage("172.001", "U1", "Ana Ruiz", "private note", false)
	direct.Direct = true
	host := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK, Events: []NormalizedExternalConversationEvent{direct},
	}}
	service := NewExternalConversationTransportService(h.store, catalog)
	service.SetRoutineEventOfferer(h.matcher)
	request := ExternalConversationIngressRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: http.MethodPost, Body: []byte(`{}`)}

	if _, err := service.NormalizeExternalConversationIngress(h.ctx, request, host); err != nil {
		t.Fatal(err)
	}
	if stats := h.matcher.RoutineEventStats(); stats.In[RoutineEventSlackMessage] != 0 {
		t.Fatalf("a direct message reached the matcher: in=%d", stats.In[RoutineEventSlackMessage])
	}

	// The same endpoint path offers a channel message.
	host.result.Events = []NormalizedExternalConversationEvent{slackRoutineMessage("172.002", "U1", "Ana Ruiz", "in the channel", false)}
	if _, err := service.NormalizeExternalConversationIngress(h.ctx, request, host); err != nil {
		t.Fatal(err)
	}
	records := waitingRoutineEvents(t, h, activation)
	if len(records) != 1 || records[0].Event.Fields["text"][0] != "in the channel" {
		t.Fatalf("waiting events = %#v", records)
	}
}

func TestTelegramMessageOffersRoutineEvent(t *testing.T) {
	h := newRoutineEventHarness(t)
	catalog, endpoint := routineConnectionFixture(t, h, "telegram", ExternalConversationSelectAllMessages)
	activation := conversationRoutine(h, endpoint, RoutineEventTelegramMessage, "chat")
	message := NormalizedExternalConversationEvent{
		ID: "telegram:update:991", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: "C123:55", ExternalThreadID: "C123",
		ExternalParticipantID: "42", ParticipantDisplayName: "Ana", Text: "Order 7 shipped?",
		OrderingKey: "C123:C123", OccurredAt: time.Now().UTC(),
		Attachments: []ExternalConversationAttachment{{ID: "file-1", Name: "label.pdf", MediaType: "application/pdf", SizeBytes: 10}},
		Attributes:  map[string]interface{}{"chatTitle": "Support desk", "participantUsername": "ana_r"},
	}
	// An unpinned gateway: authority comes from each mapped endpoint.
	gateways := NewExternalConversationGatewayService(h.store)
	registration := createActiveExternalConversationGateway(t, h.ctx, gateways, CreateExternalConversationGatewayRequest{
		ID: "telegram-bot", Name: "Telegram bot", Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "unpinned gateway",
		Gateway: ExternalConversationIngressGateway{
			Scope: endpoint.Scope, Owner: h.owner, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: "telegram",
		},
	})
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events:     []ExternalConversationGatewayEvent{{InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: message}},
	}}
	service := NewExternalConversationTransportService(h.store, catalog)
	service.SetRoutineEventOfferer(h.matcher)

	result, err := service.NormalizeExternalConversationRegisteredGatewayIngress(h.ctx, ExternalConversationPublicIngressRequest{
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
	if event.ID != "telegram:update:991" || event.Kind != RoutineEventTelegramMessage || event.Summary != "Ana: Order 7 shipped?" {
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
	h := newRoutineEventHarness(t)
	h.createCallbackRegistration("repo", h.owner.ID, CallbackRegistrationActive)
	h.createCallbackRegistration("quiet", h.owner.ID, CallbackRegistrationActive)
	activation := h.routine("github", func(trigger *RoutineEventTrigger) {
		*trigger = RoutineEventTrigger{Kind: RoutineEventGitHub, SourceRef: RoutineCallbackSourceRef("callback-repo"), SourceLabel: "acme/api (GitHub)", MaxPerHour: 20,
			Filter: []RoutineEventCondition{{Field: "event", Op: RoutineFilterEquals, Value: "issues.opened"}}}
	})
	registration, err := h.store.GetCallbackRegistration(h.ctx, h.scope, "callback-repo")
	if err != nil || registration == nil {
		t.Fatalf("registration = %#v err=%v", registration, err)
	}
	// The shape the GitHub skill's callback adapter emits, reloaded from the
	// receipt with json.Number.
	issue := EventEnvelope{
		ID: "github:delivery-1", Scope: h.scope, Type: "github.issues.opened", Source: "github", Subject: "acme/api#12",
		OccurredAt: h.now, Actor: ActivityActor{Type: "callback", ID: registration.ID},
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
	consumer := NewRoutineCallbackConsumer(h.matcher)
	subscription := CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}

	if err := consumer.ConsumeCallbackEvent(h.ctx, registration, subscription, issue); err != nil {
		t.Fatal(err)
	}
	// A registration no routine listens to is skipped before any mapping.
	quiet, _ := h.store.GetCallbackRegistration(h.ctx, h.scope, "callback-quiet")
	if err := consumer.ConsumeCallbackEvent(h.ctx, quiet, subscription, issue); err != nil {
		t.Fatal(err)
	}
	// Other providers are not routine events.
	other := *registration
	other.Provider = "gitlab"
	if err := consumer.ConsumeCallbackEvent(h.ctx, &other, subscription, issue); err != nil {
		t.Fatal(err)
	}
	if stats := h.matcher.RoutineEventStats(); stats.In[RoutineEventGitHub] != 1 || stats.Matched[RoutineEventGitHub] != 1 {
		t.Fatalf("github stats in=%d matched=%d", stats.In[RoutineEventGitHub], stats.Matched[RoutineEventGitHub])
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
	mapped := gitHubRoutineEvent(h.scope, RoutineCallbackSourceRef(registration.ID), run)
	if mapped.Summary != "Workflow run completed in acme/api: CI (failure)" || mapped.Fields["branch"][0] != "main" || mapped.Fields["conclusion"][0] != "failure" {
		t.Fatalf("workflow run event = %#v", mapped)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.ConsumeCallbackEvent(h.ctx, registration, CallbackSubscription{EventType: run.Type, Consumer: "routines"}, run); err != nil {
		t.Fatal(err)
	}
	if stats := h.matcher.RoutineEventStats(); stats.In[RoutineEventGitHub] != 2 || stats.Matched[RoutineEventGitHub] != 1 {
		t.Fatalf("github stats in=%d matched=%d", stats.In[RoutineEventGitHub], stats.Matched[RoutineEventGitHub])
	}
}

func unboundConversationMessage() (Scope, ExternalConversationAdapterReference, *NormalizedExternalConversationEvent) {
	event := slackRoutineMessage("173.001", "U1", "Ana Ruiz", "hello", false)
	return Scope{Kind: "tenant", ID: "busy"},
		ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "binding", BindingRevision: 1, AdapterID: "slack"},
		&event
}

// TestConversationRoutineOfferUnboundAllocFree: a Slack message on a
// connection no routine listens to makes no store call and allocates nothing
// in the routine path (SPEC §0a).
func TestConversationRoutineOfferUnboundAllocFree(t *testing.T) {
	matcher, store := newUnboundRoutineEventMatcher(t)
	scope, adapter, event := unboundConversationMessage()
	ctx := context.Background()
	allocs := testing.AllocsPerRun(1000, func() {
		if err := offerConversationRoutineEvent(ctx, matcher, scope, "deployment", adapter, "slack", "installation", event); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("unbound conversation offer allocates %.1f times", allocs)
	}
	if store.calls != 0 {
		t.Fatalf("unbound conversation offer made %d store calls", store.calls)
	}
	if stats := matcher.RoutineEventStats(); stats.In[RoutineEventSlackMessage] != 0 {
		t.Fatalf("unbound message reached the matcher: in=%d", stats.In[RoutineEventSlackMessage])
	}
}

func BenchmarkConversationRoutineOfferUnbound(b *testing.B) {
	matcher, _ := newUnboundRoutineEventMatcher(b)
	scope, adapter, event := unboundConversationMessage()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := offerConversationRoutineEvent(ctx, matcher, scope, "deployment", adapter, "slack", "installation", event); err != nil {
			b.Fatal(err)
		}
	}
}
