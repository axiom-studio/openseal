// Package cardinteraction records a person's answer to an interactive card
// component (a visual's choice chip, button, form, quiz, poll, ...) and,
// when the component wakes the Agent, builds and posts that answer as the
// person's message. Every host that takes card answers, the web chat and
// external channels alike, goes through it so the rules of
// presentation.Interaction are applied in one place.
package cardinteraction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// ErrInvalidValues marks an interaction whose values the server could not
// grade, tally or recompute.
var ErrInvalidValues = errors.New("invalid card interaction")

// ErrSurfaceUnavailable marks a card version that exists but could not be
// read.
var ErrSurfaceUnavailable = errors.New("card could not be loaded")

// NotFoundError reports a card version without the answered component.
type NotFoundError struct{ message string }

func (e *NotFoundError) Error() string { return e.message }

// RejectedError reports an interaction the component does not accept: an
// unknown action, a wrong item key, values of the wrong shape or a closed
// poll. Its text is the rule that was broken.
type RejectedError struct{ err error }

func (e *RejectedError) Error() string { return e.err.Error() }
func (e *RejectedError) Unwrap() error { return e.err }

// PeopleLister lists the people in a conversation; polls count them to know
// when everyone has voted.
type PeopleLister func(ctx context.Context, conversation *models.Conversation) ([]models.ConversationParticipant, error)

// Poster posts a channel message and wakes its handler.
type Poster interface {
	PostChannelMessage(ctx context.Context, request runtimeconversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error)
}

// Service records card interactions of one store.
type Service struct {
	store         models.ConversationStore
	conversations *runtimeconversation.Service
	artifacts     *artifactcatalog.Catalog
	content       models.ArtifactContentStore
	people        PeopleLister
	now           func() time.Time
}

func NewService(store models.ConversationStore, artifacts models.ArtifactStore, content models.ArtifactContentStore, people PeopleLister, opts ...models.ServiceOption) *Service {
	return &Service{
		store: store, conversations: runtimeconversation.NewService(store, opts...),
		artifacts: artifactcatalog.NewCatalog(artifacts, opts...), content: content, people: people,
		now: models.ClockFrom(opts),
	}
}

// Request is one answer to a card component. Card is the message that
// carries the card; the host has checked the actor may see it.
type Request struct {
	Conversation      *models.Conversation
	Card              *models.ChannelMessage
	SurfaceArtifactID string
	SurfaceVersion    int64
	ComponentID       string
	ActionID          string
	ItemKey           string
	Values            map[string]interface{}
	Actor             models.ConversationParticipant
	IdempotencyKey    string
}

// Prepared is a validated interaction with its server outcome, ready to
// record.
type Prepared struct {
	request   Request
	Component presentation.Component
	Rule      presentation.InteractionRule
	values    json.RawMessage
	outcome   json.RawMessage
}

// MayWakeAgent reports whether recording it can post an answer for the
// Agent, so hosts can check the chat has a model to answer with first.
func (p *Prepared) MayWakeAgent() bool {
	return p.Rule.NotifyAgent || PollCompleted(p.outcome)
}

// Recorded is a stored interaction. Answer is the text to post as the
// person's message when it wakes the Agent, empty otherwise.
type Recorded struct {
	Interaction *models.SurfaceInteraction
	Replayed    bool
	Answer      string
}

// LoadSurface reads one version of a visual. A missing version, or an
// artifact that is not a visual, is artifactcatalog.ErrArtifactNotFound.
func (s *Service) LoadSurface(ctx context.Context, scope models.Scope, artifactID string, version int64) (*presentation.Surface, error) {
	artifact, err := s.artifacts.Get(ctx, scope, artifactID, version)
	if err != nil {
		return nil, err
	}
	if artifact.Type != presentation.ArtifactType || artifact.Version != version {
		return nil, artifactcatalog.ErrArtifactNotFound
	}
	if s.content == nil {
		return nil, errors.New("artifact content store is unavailable")
	}
	reader, err := s.content.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, presentation.MaxSurfaceBytes+1))
	if err != nil {
		return nil, err
	}
	return presentation.Decode(raw)
}

// Prepare checks req against its card version and works out the server
// outcome: a quiz grade, a finished flashcards tally, the tally of the poll
// vote that has everyone voted, or an explorer share's outputs.
func (s *Service) Prepare(ctx context.Context, req Request) (*Prepared, error) {
	if req.Conversation == nil || req.Card == nil {
		return nil, errors.New("card interaction needs its conversation and card message")
	}
	surface, err := s.LoadSurface(ctx, req.Conversation.Scope, req.SurfaceArtifactID, req.SurfaceVersion)
	if errors.Is(err, artifactcatalog.ErrArtifactNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSurfaceUnavailable, err)
	}
	component, found := surface.Find(req.ComponentID)
	if !found {
		return nil, &NotFoundError{message: fmt.Sprintf("card version %d has no component %q", req.SurfaceVersion, req.ComponentID)}
	}
	rule, err := presentation.Interaction(*component, req.ActionID)
	if err != nil {
		return nil, &RejectedError{err: err}
	}
	if err := presentation.ValidateInteractionItem(*component, req.ActionID, req.ItemKey); err != nil {
		return nil, &RejectedError{err: err}
	}
	if err := presentation.ValidateInteractionValues(*component, req.ActionID, req.Values); err != nil {
		return nil, &RejectedError{err: err}
	}
	if component.Type == "poll" {
		if err := presentation.PollOpen(*component, s.now()); err != nil {
			return nil, &RejectedError{err: err}
		}
	}
	if req.Values == nil {
		req.Values = map[string]interface{}{}
	}
	encodedValues, err := json.Marshal(req.Values)
	if err != nil {
		return nil, err
	}
	outcome, err := s.outcome(ctx, req, *component)
	if err != nil {
		return nil, err
	}
	return &Prepared{request: req, Component: *component, Rule: rule, values: encodedValues, outcome: outcome}, nil
}

// Record stores a prepared interaction. A single-use interaction answered
// before by the same actor is a *models.SurfaceInteractionConflictError
// carrying the stored one. On replay the answer is returned again, so
// posting it completes an attempt that stored the interaction but failed
// before its message.
func (s *Service) Record(ctx context.Context, prepared *Prepared) (*Recorded, error) {
	req := prepared.request
	interaction, replayed, err := s.conversations.RecordSurfaceInteraction(ctx, runtimeconversation.RecordSurfaceInteractionRequest{
		Scope: req.Conversation.Scope, ConversationID: req.Conversation.ID, SurfaceArtifactID: req.SurfaceArtifactID,
		// A card's artifact id is its identity across versions.
		SurfaceID:      req.SurfaceArtifactID,
		SurfaceVersion: req.SurfaceVersion, MessageID: req.Card.ID, ComponentID: req.ComponentID, ItemKey: req.ItemKey,
		ActionID: req.ActionID, Actor: req.Actor, Repeatable: !prepared.Rule.SingleUse, Values: prepared.values,
		Outcome: prepared.outcome, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	result := &Recorded{Interaction: interaction, Replayed: replayed}
	notify := prepared.Rule.NotifyAgent
	if prepared.Rule.WakeWhenEveryoneVoted && PollCompleted(interaction.Outcome) {
		// Two votes that each completed the poll at once both carry the
		// tally; only the earlier one wakes the Agent.
		first, err := s.firstCompletingVote(ctx, req.Conversation, req.SurfaceArtifactID, req.ComponentID, req.SurfaceVersion)
		if err != nil {
			return nil, err
		}
		notify = first != nil && first.ID == interaction.ID
	}
	if notify {
		result.Answer = presentation.CardResponseText(prepared.Component, interaction.ActionID, interaction.ItemKey, interaction.Values, interaction.Outcome)
	}
	return result, nil
}

// AnswerRequest is the message that posts an interaction as the person's
// answer, with content as its text, so the Agent wakes with it as typed
// data. It replies in the card's thread when the card is in one.
func (s *Service) AnswerRequest(ctx context.Context, current *models.Conversation, card *models.ChannelMessage, content string, interaction *models.SurfaceInteraction) runtimeconversation.PostChannelMessageRequest {
	replyTo := ""
	if root := strings.TrimSpace(card.ThreadRootID); root != "" {
		if rootMessage, err := s.conversations.GetChannelMessage(ctx, current.Scope, current.ID, root); err == nil && rootMessage.StartsThread {
			replyTo = root
		}
	}
	return runtimeconversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID,
		Sender:           models.ConversationParticipant{Type: interaction.ActorType, ID: interaction.ActorID},
		Intent:           models.MessageIntentAnswer,
		Content:          content,
		Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ReplyToMessageID: replyTo, RequiresResponse: true,
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceArtifact, ID: interaction.SurfaceArtifactID, Version: interaction.SurfaceVersion}},
		IdempotencyKey: "card-interaction:" + interaction.ID,
	}
}

// PostAnswer posts request through poster, retrying a conversation that
// moved on since it was read.
func (s *Service) PostAnswer(ctx context.Context, poster Poster, current *models.Conversation, request runtimeconversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error) {
	for attempt := 0; attempt < 3; attempt++ {
		request.ExpectedRevision = current.Revision
		result, err := poster.PostChannelMessage(ctx, request)
		if !errors.Is(err, models.ErrRevisionConflict) {
			return result, err
		}
		if current, err = s.conversations.GetConversation(ctx, current.Scope, current.ID); err != nil {
			return nil, err
		}
	}
	return nil, models.ErrRevisionConflict
}
