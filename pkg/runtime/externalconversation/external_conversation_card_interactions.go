package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/cardinteraction"
)

// externalCardComponents are the visual components whose answers an
// external channel may send. Each records one person's answer and at most
// posts it as that person's message, so a provider identity is enough to
// give one. Approval and entity buttons are never card interactions
// (presentation.Interaction) and keep their own gates; ticks, flashcards,
// drafts, comparisons and explorers have no channel rendering.
var externalCardComponents = map[string]bool{"actions": true, "choices": true, "form": true, "poll": true, "quiz": true}

// externalCardAnswer is what a conversation.card.interacted event carries in
// its attributes: the visual version the person saw, the component and
// action they answered and the values, shaped as for
// presentation.ValidateInteractionValues.
type externalCardAnswer struct {
	ArtifactID  string
	Version     int64
	ComponentID string
	ActionID    string
	Values      map[string]interface{}
}

func externalCardInteraction(event models.NormalizedExternalConversationEvent) (externalCardAnswer, error) {
	artifactID, _ := event.Attributes["artifactId"].(string)
	componentID, _ := event.Attributes["componentId"].(string)
	actionID, _ := event.Attributes["actionId"].(string)
	version, ok := integerAttribute(event.Attributes["version"])
	answer := externalCardAnswer{
		ArtifactID: strings.TrimSpace(artifactID), Version: version,
		ComponentID: strings.TrimSpace(componentID), ActionID: strings.TrimSpace(actionID),
	}
	if !ok || version < 1 || !models.IsValidOpaqueIdentifier(answer.ArtifactID, 256) ||
		!models.IsValidOpaqueIdentifier(answer.ComponentID, 256) || len(answer.ActionID) > 256 {
		return externalCardAnswer{}, fmt.Errorf("%w: card answer attributes are invalid", models.ErrInvalidExternalConversation)
	}
	if raw, present := event.Attributes["values"]; present {
		values, ok := raw.(map[string]interface{})
		if !ok {
			return externalCardAnswer{}, fmt.Errorf("%w: card answer values must be an object", models.ErrInvalidExternalConversation)
		}
		answer.Values = values
	}
	return answer, nil
}

// applyCardInteraction records a person's answer to a visual this endpoint
// delivered, as the mapped external participant. Anyone in the provider
// conversation may answer; single-use components take one answer per
// person. An answer that wakes the Agent is posted as the person's message
// and dispatched like an inbound message, so the reply goes back out. The
// card is then refreshed in place to show the answers. An answer the card
// cannot take (an unknown card, an older version than the one shown, a
// component channels do not answer, a second answer) is ignored with the
// reason as the item's summary.
func (w *InboxWorker) applyCardInteraction(ctx context.Context, item *models.ExternalConversationInboxItem) error {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != models.ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!models.ExternalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return models.ErrExternalConversationConflict
	}
	answer, err := externalCardInteraction(item.Event)
	if err != nil {
		return err
	}
	mapping, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, models.ExternalMessageOutbound, item.Event.ExternalMessageID)
	if err != nil {
		return err
	}
	if mapping == nil || mapping.Scope != endpoint.Scope || mapping.EndpointID != endpoint.ID {
		return w.ignore(ctx, item, "", "Ignored a card answer: this connection did not send that card.")
	}
	conversation, err := w.conversations.GetConversation(ctx, endpoint.Scope, mapping.ConversationID)
	if err != nil {
		return err
	}
	card, err := w.conversations.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, mapping.ChannelMessageID)
	if err != nil {
		return err
	}
	shown := cardVersion(card, answer.ArtifactID)
	if shown == 0 {
		return w.ignore(ctx, item, conversation.ID, "Ignored a card answer: the message does not carry that visual.")
	}
	if answer.Version != shown {
		// The provider still shows an older version; show it the current one.
		if err := w.refreshCard(ctx, endpoint, item, card); err != nil {
			return err
		}
		return w.ignore(ctx, item, conversation.ID, fmt.Sprintf("Ignored a card answer: it answered version %d of the visual, which is now version %d.", answer.Version, shown))
	}
	cards, err := w.cardInteractions()
	if err != nil {
		return err
	}
	surface, err := cards.LoadSurface(ctx, endpoint.Scope, answer.ArtifactID, answer.Version)
	if reason := cardRejection(err); reason != "" {
		return w.ignore(ctx, item, conversation.ID, "Ignored a card answer: "+reason)
	}
	if err != nil {
		return err
	}
	if component, found := surface.Find(answer.ComponentID); found && !externalCardComponents[component.Type] {
		return w.ignore(ctx, item, conversation.ID, fmt.Sprintf("Ignored a card answer: %s components do not take answers from channels.", component.Type))
	}
	participant, err := w.ensureParticipant(ctx, endpoint, item.Event)
	if err != nil {
		return err
	}
	prepared, err := cards.Prepare(ctx, cardinteraction.Request{
		Conversation: conversation, Card: card, SurfaceArtifactID: answer.ArtifactID, SurfaceVersion: answer.Version,
		ComponentID: answer.ComponentID, ActionID: answer.ActionID, Values: answer.Values,
		Actor: participant.Participant, IdempotencyKey: "external-card:" + item.ID,
	})
	if reason := cardRejection(err); reason != "" {
		return w.ignore(ctx, item, conversation.ID, "Ignored a card answer: "+reason)
	}
	if err != nil {
		return err
	}
	recorded, err := cards.Record(ctx, prepared)
	var conflict *models.SurfaceInteractionConflictError
	switch {
	case errors.As(err, &conflict):
		if err := w.refreshCard(ctx, endpoint, item, card); err != nil {
			return err
		}
		return w.ignore(ctx, item, conversation.ID, "Ignored a card answer: this person already answered it.")
	case errors.Is(err, models.ErrInvalidSurfaceInteraction):
		return w.ignore(ctx, item, conversation.ID, "Ignored a card answer: "+err.Error())
	case err != nil:
		return err
	}
	if err := w.refreshCard(ctx, endpoint, item, card); err != nil {
		return err
	}
	if recorded.Answer == "" {
		return w.complete(ctx, item, conversation.ID, "", "")
	}
	request := cards.AnswerRequest(ctx, conversation, card, recorded.Answer, recorded.Interaction)
	source := activity.NewExternalMessageSource(endpoint, item.Event)
	source.ParticipantDisplayName = models.ExternalSourceLabel(participant.DisplayName, 160)
	request.SenderDisplayName, request.ExternalSource = participant.DisplayName, source
	request.References = append(request.References, models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision})
	commit, err := cards.PostAnswer(ctx, w.conversations, conversation, request)
	if err != nil {
		return err
	}
	// The answer is imported like an inbound message, so it carries the same
	// import proof: Core bills its Run to the endpoint owner only when that
	// proof exists. A provider card message takes many answers, so the
	// answer is keyed by its interaction, not the card's provider message.
	if err := w.ensureInboundMessageMapping(ctx, endpoint, cardAnswerExternalMessageID(recorded.Interaction.ID), commit.Message); err != nil {
		return err
	}
	dispatch, err := w.dispatcher.DispatchExternalConversation(ctx, DispatchRequest{
		Endpoint: endpoint, Conversation: commit.Conversation, Message: commit.Message,
		IdempotencyKey: "external-conversation-dispatch:" + item.ID,
	})
	if err != nil {
		return err
	}
	if dispatch == nil || !models.IsValidOpaqueIdentifier(dispatch.RunID, 256) {
		return errors.New("external conversation dispatcher did not return a canonical Run")
	}
	return w.complete(ctx, item, conversation.ID, commit.Message.ID, dispatch.RunID)
}

// cardAnswerExternalMessageID is the endpoint-scoped inbound message ID of
// one card answer.
func cardAnswerExternalMessageID(interactionID string) string {
	return "card-interaction:" + interactionID
}

// cardVersion is the newest version of a visual that a message carries, or 0.
func cardVersion(message *models.ChannelMessage, artifactID string) int64 {
	version := int64(0)
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceArtifact && reference.ID == artifactID && reference.Version > version {
			version = reference.Version
		}
	}
	return version
}

// cardRejection is why a card cannot take an answer, or "" when err is not
// about the answer.
func cardRejection(err error) string {
	var notFound *cardinteraction.NotFoundError
	var rejected *cardinteraction.RejectedError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, artifactcatalog.ErrArtifactNotFound):
		return "that visual version does not exist."
	case errors.As(err, &notFound), errors.As(err, &rejected), errors.Is(err, cardinteraction.ErrInvalidValues):
		return err.Error()
	}
	return ""
}

func (w *InboxWorker) cardInteractions() (*cardinteraction.Service, error) {
	artifacts, ok := w.store.(models.ArtifactStore)
	if !ok || w.config.AttachmentContent == nil {
		return nil, errors.New("card answers need the artifact store and its content")
	}
	return cardinteraction.NewService(w.store, artifacts, w.config.AttachmentContent, w.config.HumanParticipants, models.WithClock(w.now)), nil
}

// refreshCard edits the card in place so the provider shows its current
// version and answers. Adapters that cannot edit messages keep the card.
func (w *InboxWorker) refreshCard(ctx context.Context, endpoint *models.ExternalConversationEndpoint, item *models.ExternalConversationInboxItem, card *models.ChannelMessage) error {
	if w.config.Transport == nil {
		return errors.New("card refresh needs the external conversation transport")
	}
	_, adapter, err := w.config.Transport.resolveActiveEndpoint(ctx, endpoint.Scope, endpoint.ID)
	if err != nil {
		return err
	}
	if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageUpdate) {
		return nil
	}
	_, err = w.config.Transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageUpdate,
		ConversationID: card.ConversationID, ChannelMessageID: card.ID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: item.Event.ExternalThreadID,
		Parameters:     map[string]interface{}{"providerMessageId": item.Event.ExternalMessageID},
		IdempotencyKey: "card-refresh:" + item.ID,
	})
	return err
}

// ignore settles an item that is not applied, with summary saying why.
func (w *InboxWorker) ignore(ctx context.Context, item *models.ExternalConversationInboxItem, conversationID, summary string) error {
	now := w.now().UTC()
	next := models.CloneExternalConversationInboxItem(item)
	next.Status = models.ExternalConversationInboxIgnored
	next.ConversationID = conversationID
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	if len(summary) > 1024 {
		summary = strings.ToValidUTF8(summary[:1021], "") + "..."
	}
	next.ErrorCode, next.Summary = "", summary
	next.AppliedAt, next.UpdatedAt = now, now
	next.Revision++
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationInbox(ctx, next, item.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*item = *next
	return nil
}
