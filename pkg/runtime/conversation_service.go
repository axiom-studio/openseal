package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

type CreateConversationRequest struct {
	ID     string
	Scope  models.Scope
	Owner  models.ObjectiveOwner
	Title  string
	Origin *models.ConversationReference
	// SpaceID places the conversation in a space. When empty the store uses
	// the owner's first space, else the default space of CreatedBy, else it
	// returns ErrNoDefaultSpace.
	SpaceID string
	// CreatedBy is the person behind the call: the user on the request, or
	// the person a run or routine acts for.
	CreatedBy      string
	IdempotencyKey string
}

type UpdateConversationRequest struct {
	Scope                models.Scope
	ConversationID       string
	ExpectedRevision     int64
	Title                *string
	Status               *models.ConversationStatus
	ParticipationEnabled *bool
}

type PostChannelMessageRequest struct {
	ID                string
	Scope             models.Scope
	ConversationID    string
	ExpectedRevision  int64
	Sender            models.ConversationParticipant
	SenderDisplayName string
	Initiator         *models.ConversationParticipant
	ExternalSource    *models.ExternalMessageSource
	Intent            models.ConversationMessageIntent
	Content           string
	ResponseMode      string
	Audience          models.ConversationAudience
	// StartThread is a host option. The canonical root remains in the channel
	// timeline while its responses are routed to its own durable thread.
	StartThread      bool `json:"-"`
	ReplyToMessageID string
	// QuotedMessageID shows an earlier message of this conversation as a quote.
	QuotedMessageID     string
	BroadcastToChannel  bool
	Mentions            []models.ConversationParticipant
	References          []models.ConversationReference
	RequiresResponse    bool
	ResolvesMessageID   string
	SupersedesMessageID string
	// Question lists the options of an Agent's question (ask_user).
	Question *models.ChannelQuestion
	// SelectedOptionIDs are the options a reply picked from the question it
	// resolves (or replies to).
	SelectedOptionIDs []string
	// BuilderParameters are the form fields of a person's message in an agent
	// builder conversation (D7). Content may then be empty.
	BuilderParameters *models.AgentBuilderParameters
	// RoutingReason is set by a routed Team member's answer.
	RoutingReason  string
	IdempotencyKey string
	// RunGuard fences a Run's final reply (ChannelMessageCommitRecord.RunGuard).
	// A replay of an existing IdempotencyKey returns that message unguarded.
	RunGuard *models.ReplyRunGuard
}

// CommitConversationRoutingRequest records who answers one channel message.
type CommitConversationRoutingRequest struct {
	Scope            models.Scope
	ConversationID   string
	ExpectedRevision int64
	TriggerMessageID string
	Responders       []models.ConversationResponder
	IdempotencyKey   string
}

type AdvanceConversationCursorRequest struct {
	Scope             models.Scope
	ConversationID    string
	Participant       models.ConversationParticipant
	ExpectedRevision  int64
	DeliveredSequence int64
	ReadSequence      int64
}

type SetConversationPresenceRequest struct {
	Scope            models.Scope
	ConversationID   string
	Participant      models.ConversationParticipant
	State            models.ConversationPresenceState
	Summary          string
	RunID            string
	LeaseID          string
	ExpectedRevision int64
	TTL              time.Duration
}

type ConversationService struct {
	store     models.ConversationStore
	now       func() time.Time
	newID     func() string
	reactions MessageReactionProjector
}

func NewConversationService(store models.ConversationStore, opts ...models.ServiceOption) *ConversationService {
	return &ConversationService{store: store, now: models.ClockFrom(opts), newID: uuid.NewString}
}

func (s *ConversationService) CreateConversation(ctx context.Context, req CreateConversationRequest) (*models.Conversation, bool, error) {
	if s == nil || s.store == nil {
		return nil, false, errors.New("conversation store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, false, err
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, false, err
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, false, fmt.Errorf("%w: idempotency key is required", models.ErrInvalidConversation)
	}
	if existing, err := s.store.FindConversationByIdempotencyKey(ctx, req.Scope, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.Owner != req.Owner || existing.Title != strings.TrimSpace(req.Title) ||
			!reflect.DeepEqual(existing.Origin, req.Origin) || !models.SameRequestedSpace(existing, req.SpaceID) {
			return nil, false, models.ErrMessageConflict
		}
		return existing, true, nil
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = models.StableConversationID(req.Scope, key, "conversation")
	}
	now := s.now().UTC()
	conversation := &models.Conversation{
		ID: id, Scope: req.Scope, Owner: req.Owner, SpaceID: strings.TrimSpace(req.SpaceID), Title: strings.TrimSpace(req.Title), Status: models.ConversationStatusActive,
		Origin: cloneConversationReference(req.Origin), ApprovalMode: models.DefaultConversationApprovalMode, Revision: 1, CreatedAt: now, UpdatedAt: now,
		CreatedBy: strings.TrimSpace(req.CreatedBy),
	}
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.CreateConversation(ctx, conversation, key)
}

func (s *ConversationService) GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error) {
	conversation, err := s.store.GetConversation(ctx, scope, strings.TrimSpace(conversationID))
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, models.ErrConversationNotFound
	}
	return conversation, nil
}

func (s *ConversationService) UpdateConversation(ctx context.Context, req UpdateConversationRequest) (*models.Conversation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", models.ErrInvalidConversation)
	}
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	updated := models.CloneConversation(conversation)
	if req.Title != nil {
		updated.Title = strings.TrimSpace(*req.Title)
	}
	if req.Status != nil {
		updated.Status = *req.Status
		if updated.Status == models.ConversationStatusArchived {
			archivedAt := s.now().UTC()
			updated.ArchivedAt = &archivedAt
			if updated.Participation != nil {
				updated.Participation.Enabled = false
			}
		} else {
			updated.ArchivedAt = nil
		}
	}
	if req.ParticipationEnabled != nil {
		if *req.ParticipationEnabled && (updated.Status != models.ConversationStatusActive || (updated.Owner.Type != models.OwnerTypeTeam && updated.Owner.Type != models.OwnerTypeAgent)) {
			return nil, fmt.Errorf("%w: participation requires an active Agent or Team channel", models.ErrInvalidConversation)
		}
		if updated.Participation == nil {
			updated.Participation = &models.ConversationParticipation{}
		}
		if *req.ParticipationEnabled && !updated.Participation.Enabled {
			updated.Participation.AfterSequence = conversation.LastSequence
		}
		updated.Participation.Enabled = *req.ParticipationEnabled
	}
	updated.Revision++
	updated.UpdatedAt = s.now().UTC()
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	return s.store.UpdateConversation(ctx, updated, conversation.Revision)
}

func (s *ConversationService) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	return s.store.ListConversations(ctx, filter)
}

func (s *ConversationService) LastConversationActivity(ctx context.Context, scope models.Scope, ownerType models.OwnerType, ownerIDs []string) (map[string]time.Time, error) {
	return s.store.LastConversationActivity(ctx, scope, ownerType, ownerIDs)
}

func (s *ConversationService) PostChannelMessage(ctx context.Context, req PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, fmt.Errorf("%w: message idempotency key is required", models.ErrInvalidConversation)
	}
	if existing, err := s.store.FindChannelMessageByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if !sameChannelMessageRequest(existing, req) {
			return nil, models.ErrMessageConflict
		}
		conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
		return &models.ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, err
	}
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != models.ConversationStatusActive || conversation.Revision != req.ExpectedRevision {
		if conversation.Revision != req.ExpectedRevision {
			if existing, findErr := s.store.FindChannelMessageByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); findErr != nil {
				return nil, findErr
			} else if existing != nil {
				if !sameChannelMessageRequest(existing, req) {
					return nil, models.ErrMessageConflict
				}
				return &models.ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, nil
			}
			return nil, models.ErrRevisionConflict
		}
		return nil, models.ErrInvalidConversation
	}
	threadRootID, err := s.resolveThreadRoot(ctx, req.Scope, conversation.ID, req.ReplyToMessageID)
	if err != nil {
		return nil, err
	}
	quotedID, err := s.answerQuoteID(ctx, req)
	if err != nil {
		return nil, err
	}
	quote, err := s.resolveQuote(ctx, req.Scope, conversation.ID, quotedID)
	if err != nil {
		return nil, err
	}
	if req.StartThread {
		threadRootID = ""
	}
	for _, linked := range []string{req.ResolvesMessageID, req.SupersedesMessageID} {
		if linked != "" {
			if message, err := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, linked); err != nil || message == nil {
				if err != nil {
					return nil, err
				}
				return nil, models.ErrMessageConflict
			}
		}
	}
	if err := s.validateSelectedOptions(ctx, req); err != nil {
		return nil, err
	}
	if req.BuilderParameters != nil && (conversation.Origin == nil || conversation.Origin.Kind != models.ConversationReferenceAgentBuilder) {
		return nil, fmt.Errorf("%w: builder parameters belong to an agent builder conversation", models.ErrInvalidConversation)
	}
	now := s.now().UTC()
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = models.StableConversationID(req.Scope, key, "message")
	}
	message := &models.ChannelMessage{
		ID: id, Scope: req.Scope, ConversationID: conversation.ID, Sequence: conversation.LastSequence + 1,
		Sender: req.Sender, SenderDisplayName: strings.TrimSpace(req.SenderDisplayName),
		Initiator:      models.CloneConversationParticipant(req.Initiator),
		ExternalSource: models.CloneExternalMessageSource(req.ExternalSource),
		Intent:         req.Intent, Content: strings.TrimSpace(req.Content), Audience: req.Audience,
		StartsThread: req.StartThread, ThreadRootID: threadRootID, ReplyToMessageID: strings.TrimSpace(req.ReplyToMessageID), Quote: quote, BroadcastToChannel: req.BroadcastToChannel,
		Question: models.CloneChannelQuestion(req.Question), SelectedOptionIDs: selectedOptionIDs(req.SelectedOptionIDs),
		BuilderParameters: models.NormalizeAgentBuilderParameters(req.BuilderParameters),
		Mentions:          models.CloneConversationParticipants(req.Mentions),
		References:        models.CloneConversationReferences(req.References), RequiresResponse: req.RequiresResponse, ResponseMode: req.ResponseMode,
		ResolvesMessageID: strings.TrimSpace(req.ResolvesMessageID), SupersedesMessageID: strings.TrimSpace(req.SupersedesMessageID),
		RoutingReason: strings.TrimSpace(req.RoutingReason), IdempotencyKey: key, CreatedAt: now,
	}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	if message.StartsThread {
		valid, err := s.validateThreadStartAncestry(ctx, message)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, fmt.Errorf("%w: thread ancestry is missing, cyclic, or too deep", models.ErrInvalidConversation)
		}
	}
	updated := models.CloneConversation(conversation)
	updated.LastSequence = message.Sequence
	updated.Revision++
	updated.UpdatedAt = now
	return s.store.CommitChannelMessage(ctx, models.ChannelMessageCommitRecord{Conversation: updated, ExpectedRevision: conversation.Revision, Message: message, RunGuard: req.RunGuard})
}

func selectedOptionIDs(in []string) []string {
	var out []string
	for _, id := range in {
		out = append(out, strings.TrimSpace(id))
	}
	return out
}

// validateSelectedOptions checks a reply's chosen options against the question
// it resolves (or replies to): every id is one of its options, and at most one
// unless it allows several. A reply that picks nothing is free text.
func (s *ConversationService) validateSelectedOptions(ctx context.Context, req PostChannelMessageRequest) error {
	ids := selectedOptionIDs(req.SelectedOptionIDs)
	if len(ids) == 0 {
		return nil
	}
	target := strings.TrimSpace(req.ResolvesMessageID)
	if target == "" {
		target = strings.TrimSpace(req.ReplyToMessageID)
	}
	if target == "" {
		return fmt.Errorf("%w: selected options must answer a question", models.ErrInvalidConversation)
	}
	question, err := s.store.GetChannelMessage(ctx, req.Scope, req.ConversationID, target)
	if err != nil {
		return err
	}
	if question == nil || question.Question == nil {
		return fmt.Errorf("%w: selected options answer a message that offers none", models.ErrInvalidConversation)
	}
	for _, id := range ids {
		if !question.Question.HasOption(id) {
			return fmt.Errorf("%w: %q is not an option of that question", models.ErrInvalidConversation, id)
		}
	}
	if len(ids) > 1 && !question.Question.AllowMultiple {
		return fmt.Errorf("%w: that question takes one option", models.ErrInvalidConversation)
	}
	return nil
}

// CommitConversationRouting durably records a routing decision. It posts no
// messages: every responder commits its own answer from its own Run.
func (s *ConversationService) CommitConversationRouting(ctx context.Context, req CommitConversationRoutingRequest) (*models.ParticipationRoundResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, fmt.Errorf("%w: routing idempotency key is required", models.ErrInvalidConversation)
	}
	roundID := models.StableConversationID(req.Scope, key, "round")
	now := s.now().UTC()
	round := &models.ParticipationRound{
		ID: roundID, Scope: req.Scope, ConversationID: strings.TrimSpace(req.ConversationID),
		TriggerMessageID: strings.TrimSpace(req.TriggerMessageID), Status: models.ParticipationRoundCommitted,
		Responders: append([]models.ConversationResponder(nil), req.Responders...), IdempotencyKey: key, Revision: 1, CreatedAt: now, CommittedAt: now,
	}
	if existing, err := s.replayConversationRouting(ctx, round); err != nil || existing != nil {
		return existing, err
	}
	conversation, err := s.GetConversation(ctx, req.Scope, round.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != models.ConversationStatusActive {
		return nil, models.ErrInvalidConversation
	}
	if conversation.Revision != req.ExpectedRevision {
		// A concurrent replica may have committed this same request between the
		// idempotency lookup and the conversation read; that is a replay, not a
		// conflict.
		if existing, err := s.replayConversationRouting(ctx, round); err != nil || existing != nil {
			return existing, err
		}
		return nil, models.ErrRevisionConflict
	}
	if round.TriggerMessageID != "" {
		if trigger, err := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, round.TriggerMessageID); err != nil {
			return nil, err
		} else if trigger == nil {
			return nil, models.ErrMessageConflict
		}
	}
	updated := models.CloneConversation(conversation)
	updated.Revision++
	updated.UpdatedAt = now
	round.ConversationRevision = updated.Revision
	if err := round.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidConversation, err)
	}
	return s.store.CommitParticipationRound(ctx, models.ParticipationRoundCommitRecord{
		Conversation: updated, ExpectedRevision: conversation.Revision, Round: round,
	})
}

// replayConversationRouting returns the already committed round for the
// request's idempotency key, or nil when none exists yet.
func (s *ConversationService) replayConversationRouting(ctx context.Context, round *models.ParticipationRound) (*models.ParticipationRoundResult, error) {
	existing, err := s.store.FindParticipationRoundByIdempotencyKey(ctx, round.Scope, round.ConversationID, round.IdempotencyKey)
	if err != nil || existing == nil {
		return nil, err
	}
	if !models.SameParticipationRound(existing.Round, round) {
		return nil, models.ErrMessageConflict
	}
	return existing, nil
}

func (s *ConversationService) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	if err := models.ValidateChannelMessageViewFilter(filter); err != nil {
		return nil, err
	}
	if filter.BeforeSequence < 0 || filter.AfterSequence < 0 || (filter.BeforeSequence > 0 && filter.AfterSequence >= filter.BeforeSequence) {
		return nil, fmt.Errorf("%w: message sequence bounds are invalid", models.ErrInvalidConversation)
	}
	if filter.Viewer == nil {
		messages, err := s.store.ListChannelMessages(ctx, filter)
		if err != nil {
			return nil, err
		}
		return withMessageReactions(ctx, s.store, messages)
	}
	// Scan through fully hidden pages server-side so an authorized caller does
	// not get stuck on an empty page and does not need hidden sequence metadata.
	desired := filter.Limit
	if desired <= 0 {
		desired = 100
	}
	scan := filter
	scan.Limit = desired
	scan.Viewer = nil
	result := make([]*models.ChannelMessage, 0, desired)
	for len(result) < desired {
		raw, err := s.store.ListChannelMessages(ctx, scan)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		visible, err := s.filterVisibleChannelMessages(ctx, filter.Scope, filter.ConversationID, raw, *filter.Viewer)
		if err != nil {
			return nil, err
		}
		result = append(result, visible...)
		if scan.Descending {
			scan.BeforeSequence = raw[len(raw)-1].Sequence
		} else {
			scan.AfterSequence = raw[len(raw)-1].Sequence
		}
		if len(raw) < scan.Limit {
			break
		}
	}
	if len(result) > desired {
		result = result[:desired]
	}
	return withMessageReactions(ctx, s.store, result)
}

func (s *ConversationService) filterVisibleChannelMessages(ctx context.Context, scope models.Scope, conversationID string, messages []*models.ChannelMessage, viewer models.ConversationViewer) ([]*models.ChannelMessage, error) {
	visible, err := s.filterVisibleChannelMessageParents(ctx, scope, conversationID, messages, viewer)
	if err != nil {
		return nil, err
	}
	return s.redactUnviewableQuotes(ctx, scope, conversationID, visible, viewer)
}

// redactUnviewableQuotes keeps a quote's sender and excerpt only for viewers
// who can see the quoted message itself.
func (s *ConversationService) redactUnviewableQuotes(ctx context.Context, scope models.Scope, conversationID string, messages []*models.ChannelMessage, viewer models.ConversationViewer) ([]*models.ChannelMessage, error) {
	ids := make([]string, 0)
	for _, message := range messages {
		if message != nil && message.Quote != nil && !message.Quote.Unavailable {
			ids = append(ids, message.Quote.MessageID)
		}
	}
	if len(ids) == 0 {
		return messages, nil
	}
	quoted := make(map[string]*models.ChannelMessage, len(ids))
	if batch, ok := s.store.(models.ChannelMessageBatchStore); ok {
		loaded, err := loadChannelMessageBatches(ctx, batch, scope, conversationID, ids)
		if err != nil {
			return nil, err
		}
		quoted = loaded
	} else {
		for _, id := range ids {
			if _, seen := quoted[id]; seen {
				continue
			}
			message, err := s.store.GetChannelMessage(ctx, scope, conversationID, id)
			if err != nil {
				return nil, err
			}
			quoted[id] = message
		}
	}
	out := make([]*models.ChannelMessage, len(messages))
	for i, message := range messages {
		out[i] = message
		if message != nil && message.Quote != nil && !message.Quote.Unavailable && !models.CanViewChannelMessage(quoted[message.Quote.MessageID], viewer) {
			redacted := models.CloneChannelMessage(message)
			redacted.Quote = message.Quote.Redacted()
			out[i] = redacted
		}
	}
	return out, nil
}

func (s *ConversationService) filterVisibleChannelMessageParents(ctx context.Context, scope models.Scope, conversationID string, messages []*models.ChannelMessage, viewer models.ConversationViewer) ([]*models.ChannelMessage, error) {
	if err := viewer.Validate(); err != nil {
		return nil, err
	}
	if batch, ok := s.store.(models.ChannelMessageBatchStore); ok {
		return filterChannelMessagesWithBatchParents(ctx, batch, scope, conversationID, messages, viewer)
	}
	visible := make([]*models.ChannelMessage, 0, len(messages))
	var known map[string]*models.ChannelMessage
	for _, message := range messages {
		if !models.CanViewChannelMessage(message, viewer) {
			continue
		}
		if message.StartsThread && known == nil {
			known = make(map[string]*models.ChannelMessage)
		}
		if known != nil {
			known[message.ID] = message
		}
		allowed := true
		seen := map[string]bool{message.ID: true}
		for _, linked := range []string{message.ThreadRootID, message.ReplyToMessageID} {
			if linked == "" || seen[linked] {
				continue
			}
			seen[linked] = true
			parent, err := s.store.GetChannelMessage(ctx, scope, conversationID, linked)
			if err != nil {
				return nil, err
			}
			if parent != nil && parent.StartsThread && known == nil {
				known = make(map[string]*models.ChannelMessage)
				known[message.ID] = message
			}
			if known != nil {
				known[linked] = parent
			}
			if !models.CanViewChannelMessage(parent, viewer) {
				allowed = false
				break
			}
		}
		if allowed {
			visible = append(visible, message)
		}
	}
	if known == nil {
		return visible, nil
	}
	return filterChannelMessageThreadStarts(ctx, scope, conversationID, visible, known, s.threadStartParentLoader(scope, conversationID), viewer)
}

func (s *ConversationService) GetVisibleChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string, viewer models.ConversationViewer) (*models.ChannelMessage, error) {
	message, err := s.GetChannelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	visible, err := s.filterVisibleChannelMessages(ctx, scope, conversationID, []*models.ChannelMessage{message}, viewer)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return nil, models.ErrChannelMessageNotFound
	}
	return visible[0], nil
}

func (s *ConversationService) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string) (*models.ChannelMessage, error) {
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, models.ErrChannelMessageNotFound
	}
	filled, err := withMessageReactions(ctx, s.store, []*models.ChannelMessage{message})
	if err != nil {
		return nil, err
	}
	return filled[0], nil
}

func (s *ConversationService) GetParticipationRound(ctx context.Context, scope models.Scope, conversationID, roundID string) (*models.ParticipationRoundResult, error) {
	result, err := s.store.GetParticipationRound(ctx, scope, conversationID, roundID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, models.ErrParticipationRoundNotFound
	}
	return result, nil
}

func (s *ConversationService) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope models.Scope, conversationID, key string) (*models.ParticipationRoundResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidOpaqueIdentifier(strings.TrimSpace(conversationID), 128) || strings.TrimSpace(key) == "" || len(key) > 256 {
		return nil, fmt.Errorf("%w: conversation and idempotency key are required", models.ErrInvalidConversation)
	}
	return s.store.FindParticipationRoundByIdempotencyKey(ctx, scope, strings.TrimSpace(conversationID), strings.TrimSpace(key))
}

func (s *ConversationService) ListParticipationRounds(ctx context.Context, filter models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error) {
	return s.store.ListParticipationRounds(ctx, filter)
}

func (s *ConversationService) GetCursor(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
	return s.store.GetConversationCursor(ctx, scope, conversationID, participant)
}

// ListCursors returns the durable delivery and read position for every
// participant that has observed a conversation. The result is ordered by
// participant identity so API projections remain deterministic across stores.
func (s *ConversationService) ListCursors(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationCursor, error) {
	if _, err := s.GetConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	return s.store.ListConversationCursors(ctx, scope, conversationID)
}

func (s *ConversationService) AdvanceCursor(ctx context.Context, req AdvanceConversationCursorRequest) (*models.ConversationCursor, bool, error) {
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, false, err
	}
	if err := req.Participant.Validate(); err != nil {
		return nil, false, err
	}
	if req.DeliveredSequence < 0 || req.ReadSequence < 0 || req.ReadSequence > req.DeliveredSequence || req.DeliveredSequence > conversation.LastSequence {
		return nil, false, models.ErrConversationCursorConflict
	}
	current, err := s.store.GetConversationCursor(ctx, req.Scope, conversation.ID, req.Participant)
	if err != nil {
		return nil, false, err
	}
	if current != nil {
		if current.Revision != req.ExpectedRevision || req.DeliveredSequence < current.DeliveredSequence || req.ReadSequence < current.ReadSequence {
			return nil, false, models.ErrConversationCursorConflict
		}
		if req.DeliveredSequence == current.DeliveredSequence && req.ReadSequence == current.ReadSequence {
			return current, true, nil
		}
	} else if req.ExpectedRevision != 0 {
		return nil, false, models.ErrConversationCursorConflict
	}
	revision := int64(1)
	if current != nil {
		revision = current.Revision + 1
	}
	cursor := &models.ConversationCursor{
		Scope: req.Scope, ConversationID: conversation.ID, Participant: req.Participant,
		DeliveredSequence: req.DeliveredSequence, ReadSequence: req.ReadSequence, Revision: revision, UpdatedAt: s.now().UTC(),
	}
	if err := cursor.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.PutConversationCursor(ctx, models.ConversationCursorRecord{Cursor: cursor, ExpectedRevision: req.ExpectedRevision})
}

func (s *ConversationService) SetPresence(ctx context.Context, req SetConversationPresenceRequest) (*models.ConversationPresence, error) {
	if _, err := s.GetConversation(ctx, req.Scope, req.ConversationID); err != nil {
		return nil, err
	}
	if err := req.Participant.Validate(); err != nil {
		return nil, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	if ttl < time.Second || ttl > 2*time.Minute {
		return nil, fmt.Errorf("%w: presence TTL must be between one second and two minutes", models.ErrInvalidConversation)
	}
	now := s.now().UTC()
	current, err := s.store.GetConversationPresence(ctx, req.Scope, req.ConversationID, req.Participant)
	if err != nil {
		return nil, err
	}
	leaseID := strings.TrimSpace(req.LeaseID)
	if current != nil && current.ExpiresAt.After(now) {
		if leaseID == "" || leaseID != current.LeaseID || current.Revision != req.ExpectedRevision {
			return nil, models.ErrConversationPresenceConflict
		}
	} else if req.ExpectedRevision != 0 {
		return nil, models.ErrConversationPresenceConflict
	}
	if leaseID == "" {
		leaseID = s.newID()
	}
	revision := int64(1)
	if current != nil && current.ExpiresAt.After(now) {
		revision = current.Revision + 1
	}
	presence := &models.ConversationPresence{
		Scope: req.Scope, ConversationID: req.ConversationID, Participant: req.Participant, State: req.State,
		Summary: strings.TrimSpace(req.Summary), RunID: strings.TrimSpace(req.RunID), LeaseID: leaseID,
		Revision: revision, UpdatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := presence.Validate(); err != nil {
		return nil, err
	}
	return s.store.PutConversationPresence(ctx, models.ConversationPresenceRecord{Presence: presence, ExpectedRevision: req.ExpectedRevision})
}

func (s *ConversationService) ReleasePresence(ctx context.Context, req models.ReleaseConversationPresenceRequest) error {
	if s == nil || s.store == nil {
		return errors.New("conversation store is not configured")
	}
	return s.store.ReleaseConversationPresence(ctx, req)
}

func (s *ConversationService) ListPresence(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationPresence, error) {
	return s.store.ListConversationPresence(ctx, scope, conversationID, s.now().UTC())
}

func (s *ConversationService) resolveThreadRoot(ctx context.Context, scope models.Scope, conversationID, replyTo string) (string, error) {
	replyTo = strings.TrimSpace(replyTo)
	if replyTo == "" {
		return "", nil
	}
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, replyTo)
	if err != nil {
		return "", err
	}
	if message == nil {
		return "", models.ErrMessageConflict
	}
	if message.ThreadRootID != "" {
		return message.ThreadRootID, nil
	}
	return message.ID, nil
}

func (s *ConversationService) resolveQuote(ctx context.Context, scope models.Scope, conversationID, quotedID string) (*models.ChannelMessageQuote, error) {
	quotedID = strings.TrimSpace(quotedID)
	if quotedID == "" {
		return nil, nil
	}
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, quotedID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, models.ErrMessageConflict
	}
	return models.NewChannelMessageQuote(message), nil
}

// answerQuoteID returns the explicit quote, or quotes the message an Agent or
// Team is answering when that answer would otherwise read out of order.
func (s *ConversationService) answerQuoteID(ctx context.Context, req PostChannelMessageRequest) (string, error) {
	if quoted := strings.TrimSpace(req.QuotedMessageID); quoted != "" {
		return quoted, nil
	}
	replyTo := strings.TrimSpace(req.ReplyToMessageID)
	if replyTo == "" || req.Intent != models.MessageIntentAnswer ||
		(req.Sender.Type != models.ConversationParticipantAgent && req.Sender.Type != models.ConversationParticipantTeam) {
		return "", nil
	}
	recent, err := s.store.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: req.Scope, ConversationID: req.ConversationID, Limit: 50, Descending: true})
	if err != nil {
		return "", err
	}
	if answerNeedsQuote(replyTo, req.Sender, recent) {
		return replyTo, nil
	}
	return "", nil
}

func quotedMessageID(quote *models.ChannelMessageQuote) string {
	if quote == nil {
		return ""
	}
	return quote.MessageID
}

// answerNeedsQuote reports whether an answer to triggerID would read out of
// order: someone other than the answerer has already spoken after the trigger
// in the same view (main chat or the same started thread). recent is newest first.
func answerNeedsQuote(triggerID string, answerer models.ConversationParticipant, recent []*models.ChannelMessage) bool {
	byID := make(map[string]*models.ChannelMessage, len(recent))
	for _, message := range recent {
		if message != nil {
			byID[message.ID] = message
		}
	}
	trigger := byID[triggerID]
	if trigger == nil {
		return false
	}
	view := func(message *models.ChannelMessage) string {
		if message.StartsThread {
			return ""
		}
		if root := byID[message.ThreadRootID]; root != nil && root.StartsThread {
			return root.ID
		}
		return ""
	}
	triggerView := view(trigger)
	for _, message := range recent {
		if message == nil || message.Sequence <= trigger.Sequence {
			continue
		}
		if message.Sender != answerer && message.Intent != models.MessageIntentSystem && message.Sender.Type != models.ConversationParticipantService && view(message) == triggerView {
			return true
		}
	}
	return false
}

func sameChannelMessageRequest(existing *models.ChannelMessage, req PostChannelMessageRequest) bool {
	if existing == nil || existing.ResponseMode != req.ResponseMode {
		return false
	}
	return existing != nil && existing.Scope == req.Scope && existing.ConversationID == strings.TrimSpace(req.ConversationID) &&
		existing.Sender == req.Sender && existing.SenderDisplayName == strings.TrimSpace(req.SenderDisplayName) &&
		reflect.DeepEqual(existing.Initiator, req.Initiator) &&
		reflect.DeepEqual(existing.ExternalSource, req.ExternalSource) &&
		existing.Intent == req.Intent && existing.Content == strings.TrimSpace(req.Content) &&
		existing.Audience.Kind == req.Audience.Kind && reflect.DeepEqual(existing.Audience.Participants, req.Audience.Participants) &&
		reflect.DeepEqual(existing.Audience.Roles, req.Audience.Roles) && existing.ReplyToMessageID == strings.TrimSpace(req.ReplyToMessageID) &&
		(strings.TrimSpace(req.QuotedMessageID) == "" || quotedMessageID(existing.Quote) == strings.TrimSpace(req.QuotedMessageID)) &&
		existing.StartsThread == req.StartThread && existing.BroadcastToChannel == req.BroadcastToChannel &&
		reflect.DeepEqual(existing.Mentions, req.Mentions) && reflect.DeepEqual(existing.References, req.References) &&
		existing.RoutingReason == strings.TrimSpace(req.RoutingReason) &&
		reflect.DeepEqual(existing.Question, req.Question) && slices.Equal(existing.SelectedOptionIDs, selectedOptionIDs(req.SelectedOptionIDs)) &&
		reflect.DeepEqual(existing.BuilderParameters, models.NormalizeAgentBuilderParameters(req.BuilderParameters)) &&
		existing.RequiresResponse == req.RequiresResponse && existing.ResolvesMessageID == strings.TrimSpace(req.ResolvesMessageID) &&
		existing.SupersedesMessageID == strings.TrimSpace(req.SupersedesMessageID)
}

func cloneConversationReference(in *models.ConversationReference) *models.ConversationReference {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
