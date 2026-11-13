package externalconversation

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// cardSurface is a visual with components a channel answers (choices, a
// quiz, a poll) and one it does not (a checklist).
const cardSurface = `{"schema":"` + presentation.Schema + `","title":"Dinner","state":"ready","components":[` +
	`{"id":"pick","type":"choices","summary":"Where to eat","choices":{"chips":[{"id":"thai","label":"Thai"},{"id":"pizza","label":"Pizza"}]}},` +
	`{"id":"check","type":"quiz","summary":"Check","quiz":{"question":"Which is spicy?","options":[{"id":"t","label":"Thai"},{"id":"p","label":"Pizza"}],"answerKey":{"correct":["t"]}}},` +
	`{"id":"vote","type":"poll","summary":"Vote","poll":{"question":"When?","options":[{"id":"six","label":"Six"},{"id":"eight","label":"Eight"}]}},` +
	`{"id":"pack","type":"checklist","summary":"Bring","checklist":{"items":[{"id":"cash","label":"Cash"}]}}]}`

// countingDispatcher hands out one Run per dispatched message.
type countingDispatcher struct{ requests []DispatchRequest }

func (d *countingDispatcher) DispatchExternalConversation(_ context.Context, req DispatchRequest) (*DispatchResult, error) {
	d.requests = append(d.requests, req)
	return &DispatchResult{RunID: "run-" + req.Message.ID}, nil
}

type cardAnswerFixture struct {
	t            *testing.T
	store        *sqlite.Store
	catalog      *skill.Catalog
	endpoint     *models.ExternalConversationEndpoint
	content      artifactBytes
	conversation *models.Conversation
	card         *models.ChannelMessage
	transport    *TransportService
	inbox        *InboxWorker
	dispatcher   *countingDispatcher
	events       int
}

// newCardAnswerFixture is a Slack endpoint that shows visuals and takes card
// answers, with an Agent card (version cardVersion of cardSurface) it
// delivered as provider message "card-ts".
func newCardAnswerFixture(t *testing.T, cardVersion int64) *cardAnswerFixture {
	t.Helper()
	store, catalog, endpoint := endpointFixtureWithOperations(t, t.Context(), "slack",
		[]skill.ConversationDeliveryOperation{skill.ConversationDeliveryMessageSend, skill.ConversationDeliveryMessageUpdate})
	catalog = withAdapterChange(t, catalog, endpoint, func(adapter *skill.ConversationAdapter) {
		adapter.Features = append(adapter.Features, capability.ConversationFeatureVisuals)
		adapter.InboundEventTypes = append(adapter.InboundEventTypes, capability.ConversationEventCardInteracted)
		slices.Sort(adapter.Features)
		slices.Sort(adapter.InboundEventTypes)
	})
	f := &cardAnswerFixture{t: t, store: store, catalog: catalog, endpoint: endpoint, content: artifactBytes{}, dispatcher: &countingDispatcher{}}
	for version := int64(1); version <= cardVersion; version++ {
		f.saveCardVersion(version)
	}
	conversations := runtimeconversation.NewService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Dinner",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "dinner",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.Owner.ID},
		Intent: models.MessageIntentAnswer, Content: "Where shall we eat?",
		Audience:       models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceArtifact, ID: "visual-dinner", Version: cardVersion}},
		IdempotencyKey: "card",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SaveExternalMessageMapping(t.Context(), &models.ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: models.ExternalMessageOutbound,
		ExternalMessageID: "card-ts", ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	f.conversation, f.card = posted.Conversation, posted.Message
	f.transport = NewTransportService(store, catalog)
	people := func(context.Context, *models.Conversation) ([]models.ConversationParticipant, error) { return nil, nil }
	if f.inbox, err = NewInboxWorker(store, f.dispatcher, InboxWorkerConfig{
		WorkerID: "inbox", AttachmentContent: f.content, HumanParticipants: people, Transport: f.transport,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cardAnswerFixture) saveCardVersion(version int64) {
	f.t.Helper()
	ref := "visual-dinner-" + string(rune('0'+version))
	f.content[ref] = []byte(cardSurface)
	owner := f.endpoint.Owner
	if _, err := artifactcatalog.NewCatalog(f.store).Register(f.t.Context(), artifactcatalog.RegisterArtifactRequest{Artifact: &models.Artifact{
		ID: "visual-dinner", Version: version, Scope: f.endpoint.Scope, Name: "Dinner", Type: presentation.ArtifactType, MediaType: presentation.MediaType,
		ContentRef: ref, Digest: models.SHA256Digest([]byte(cardSurface)), SizeBytes: int64(len(cardSurface)),
		Classification: models.ArtifactClassificationInternal, CreatedAt: time.Now().UTC(),
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: owner.ID}, Owner: &owner},
	}, ExpectedLatestVersion: version - 1}); err != nil {
		f.t.Fatal(err)
	}
}

// answer receives a card answer from Slack user and applies it.
func (f *cardAnswerFixture) answer(user string, version int64, componentID, actionID string, values map[string]interface{}) *models.ExternalConversationInboxItem {
	f.t.Helper()
	f.events++
	attributes := map[string]interface{}{"artifactId": "visual-dinner", "version": float64(version), "componentId": componentID}
	if actionID != "" {
		attributes["actionId"] = actionID
	}
	if values != nil {
		attributes["values"] = values
	}
	received, err := f.transport.Receive(f.t.Context(), ReceiveExternalConversationEventRequest{
		Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID,
		Event: models.NormalizedExternalConversationEvent{
			ID: "slack:card:" + user + ":" + string(rune('a'+f.events)), Type: capability.ConversationEventCardInteracted,
			ExternalConversationID: "C123", ExternalThreadID: "card-ts", ExternalMessageID: "card-ts",
			ExternalParticipantID: user, ParticipantDisplayName: strings.ToUpper(user),
			OrderingKey: "C123:card-ts", OccurredAt: time.Now().UTC(), Attributes: attributes,
		},
	})
	if err != nil || !received.Accepted {
		f.t.Fatalf("receive = %#v, %v", received, err)
	}
	item, err := f.inbox.ProcessOne(f.t.Context(), f.endpoint.Scope)
	if err != nil {
		f.t.Fatalf("apply card answer: %v", err)
	}
	return item
}

func (f *cardAnswerFixture) cardRefreshes() []*models.ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.store.ListExternalConversationDeliveries(f.t.Context(), models.ExternalConversationDeliveryFilter{Scope: f.endpoint.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var refreshes []*models.ExternalConversationDelivery
	for _, delivery := range deliveries {
		if delivery.Operation == capability.ConversationDeliveryMessageUpdate && delivery.ChannelMessageID == f.card.ID {
			refreshes = append(refreshes, delivery)
		}
	}
	return refreshes
}

func TestCardAnswerFromAChannelIsPostedAsThePersonAndWakesTheAgent(t *testing.T) {
	f := newCardAnswerFixture(t, 1)

	item := f.answer("U1", 1, "pick", "", map[string]interface{}{"chipId": "thai"})

	if item.Status != models.ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID != f.conversation.ID {
		t.Fatalf("item = %#v", item)
	}
	answer, err := f.store.GetChannelMessage(t.Context(), f.endpoint.Scope, f.conversation.ID, item.ChannelMessageID)
	if err != nil {
		t.Fatal(err)
	}
	participant, err := f.store.GetExternalParticipantMapping(t.Context(), f.endpoint.Scope, f.endpoint.ID, "U1")
	if err != nil || participant == nil {
		t.Fatalf("participant = %#v, %v", participant, err)
	}
	component, _ := mustSurface(t).Find("pick")
	want := presentation.CardResponseText(*component, "", "", []byte(`{"chipId":"thai"}`), nil)
	if answer.Sender != participant.Participant || answer.Content != want || answer.SenderDisplayName != "U1" ||
		answer.ExternalSource == nil || answer.ExternalSource.ParticipantID != "U1" || !answer.RequiresResponse {
		t.Fatalf("answer = %#v, want content %q", answer, want)
	}
	if len(f.dispatcher.requests) != 1 || f.dispatcher.requests[0].Message.ID != answer.ID {
		t.Fatalf("dispatches = %#v", f.dispatcher.requests)
	}
	refreshes := f.cardRefreshes()
	if len(refreshes) != 1 || refreshes[0].Parameters["providerMessageId"] != "card-ts" || refreshes[0].ExternalThreadID != "card-ts" {
		t.Fatalf("card refreshes = %#v", refreshes)
	}
}

func TestCardAnswerFromAChannelIsSingleUsePerPerson(t *testing.T) {
	f := newCardAnswerFixture(t, 1)
	if item := f.answer("U1", 1, "check", "", map[string]interface{}{"selected": []interface{}{"t"}}); item.Status != models.ExternalConversationInboxApplied {
		t.Fatalf("first answer = %#v", item)
	}

	second := f.answer("U1", 1, "check", "", map[string]interface{}{"selected": []interface{}{"p"}})
	other := f.answer("U2", 1, "check", "", map[string]interface{}{"selected": []interface{}{"p"}})

	if second.Status != models.ExternalConversationInboxIgnored || !strings.Contains(second.Summary, "already answered") || second.RunID != "" {
		t.Fatalf("second answer = %#v", second)
	}
	if other.Status != models.ExternalConversationInboxApplied {
		t.Fatalf("another person's answer = %#v", other)
	}
	if len(f.dispatcher.requests) != 2 {
		t.Fatalf("dispatches = %d", len(f.dispatcher.requests))
	}
	// Every answer, the refused one too, shows the card as it now stands.
	if refreshes := f.cardRefreshes(); len(refreshes) != 3 {
		t.Fatalf("card refreshes = %d", len(refreshes))
	}
}

func TestCardAnswerFromAChannelRejectsStaleAndUnansweredCards(t *testing.T) {
	f := newCardAnswerFixture(t, 2)

	stale := f.answer("U1", 1, "pick", "", map[string]interface{}{"chipId": "thai"})
	checklist := f.answer("U1", 2, "pack", "", map[string]interface{}{"checked": true})
	badChip := f.answer("U1", 2, "pick", "", map[string]interface{}{"chipId": "sushi"})

	for _, test := range []struct {
		item    *models.ExternalConversationInboxItem
		summary string
	}{
		{stale, "answered version 1 of the visual, which is now version 2"},
		{checklist, "checklist components do not take answers from channels"},
		{badChip, "chipId must be one of thai, pizza"},
	} {
		if test.item.Status != models.ExternalConversationInboxIgnored || !strings.Contains(test.item.Summary, test.summary) {
			t.Fatalf("item = %#v, want summary with %q", test.item, test.summary)
		}
	}
	if len(f.dispatcher.requests) != 0 {
		t.Fatalf("a refused answer woke the Agent: %#v", f.dispatcher.requests)
	}
	interactions, err := f.store.ListSurfaceInteractions(t.Context(), models.SurfaceInteractionFilter{Scope: f.endpoint.Scope, ConversationID: f.conversation.ID, Limit: 10})
	if err != nil || len(interactions) != 0 {
		t.Fatalf("interactions = %#v, %v", interactions, err)
	}
	// The stale card is refreshed to the version the message now carries.
	if refreshes := f.cardRefreshes(); len(refreshes) != 1 {
		t.Fatalf("card refreshes = %d", len(refreshes))
	}
}

func TestCardAnswerOnAnUnknownMessageIsIgnored(t *testing.T) {
	f := newCardAnswerFixture(t, 1)
	received, err := f.transport.Receive(t.Context(), ReceiveExternalConversationEventRequest{
		Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID,
		Event: models.NormalizedExternalConversationEvent{
			ID: "slack:card:elsewhere", Type: capability.ConversationEventCardInteracted,
			ExternalConversationID: "C123", ExternalMessageID: "other-ts", ExternalParticipantID: "U1",
			OrderingKey: "C123:other-ts", OccurredAt: time.Now().UTC(),
			Attributes: map[string]interface{}{"artifactId": "visual-dinner", "version": float64(1), "componentId": "pick", "values": map[string]interface{}{"chipId": "thai"}},
		},
	})
	if err != nil || !received.Accepted {
		t.Fatalf("receive = %#v, %v", received, err)
	}
	item, err := f.inbox.ProcessOne(t.Context(), f.endpoint.Scope)
	if err != nil || item.Status != models.ExternalConversationInboxIgnored || !strings.Contains(item.Summary, "did not send that card") {
		t.Fatalf("item = %#v, %v", item, err)
	}
}

func TestCardAnswerWithoutItsVisualIsRefusedAtTheDoor(t *testing.T) {
	f := newCardAnswerFixture(t, 1)
	_, err := f.transport.Receive(t.Context(), ReceiveExternalConversationEventRequest{
		Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID,
		Event: models.NormalizedExternalConversationEvent{
			ID: "slack:card:bad", Type: capability.ConversationEventCardInteracted,
			ExternalConversationID: "C123", ExternalMessageID: "card-ts", ExternalParticipantID: "U1",
			OrderingKey: "C123:card-ts", OccurredAt: time.Now().UTC(),
			Attributes: map[string]interface{}{"componentId": "pick"},
		},
	})
	if err == nil {
		t.Fatal("a card answer without its visual was accepted")
	}
}

func mustSurface(t *testing.T) *presentation.Surface {
	t.Helper()
	surface, err := presentation.Decode([]byte(cardSurface))
	if err != nil {
		t.Fatal(err)
	}
	return surface
}
