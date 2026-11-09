package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

type CreateConversationRequest struct {
	ID     string
	Scope  Scope
	Owner  ObjectiveOwner
	Title  string
	Origin *ConversationReference
	// SpaceID places the conversation in a space. When empty the store uses
	// the owner's first space, else the scope's Default space.
	SpaceID        string
	IdempotencyKey string
}

type UpdateConversationRequest struct {
	Scope                Scope
	ConversationID       string
	ExpectedRevision     int64
	Title                *string
	Status               *ConversationStatus
	ParticipationEnabled *bool
}

type ConversationFilter struct {
	Scope    Scope
	Owner    *ObjectiveOwner
	SpaceID  string
	Statuses []ConversationStatus
	// OriginKind keeps only conversations whose Origin has this kind; empty
	// lists every origin.
	OriginKind ConversationReferenceKind
	Limit      int
	Offset     int
}

type PostChannelMessageRequest struct {
	ID                string
	Scope             Scope
	ConversationID    string
	ExpectedRevision  int64
	Sender            ConversationParticipant
	SenderDisplayName string
	Initiator         *ConversationParticipant
	ExternalSource    *ExternalMessageSource
	Intent            ConversationMessageIntent
	Content           string
	ResponseMode      string
	Audience          ConversationAudience
	// StartThread is a host option. The canonical root remains in the channel
	// timeline while its responses are routed to its own durable thread.
	StartThread      bool `json:"-"`
	ReplyToMessageID string
	// QuotedMessageID shows an earlier message of this conversation as a quote.
	QuotedMessageID     string
	BroadcastToChannel  bool
	Mentions            []ConversationParticipant
	References          []ConversationReference
	RequiresResponse    bool
	ResolvesMessageID   string
	SupersedesMessageID string
	// Question lists the options of an Agent's question (ask_user).
	Question *ChannelQuestion
	// SelectedOptionIDs are the options a reply picked from the question it
	// resolves (or replies to).
	SelectedOptionIDs []string
	// BuilderParameters are the form fields of a person's message in an agent
	// builder conversation (D7). Content may then be empty.
	BuilderParameters *AgentBuilderParameters
	// RoutingReason is set by a routed Team member's answer.
	RoutingReason  string
	IdempotencyKey string
	// RunGuard fences a Run's final reply (ChannelMessageCommitRecord.RunGuard).
	// A replay of an existing IdempotencyKey returns that message unguarded.
	RunGuard *ReplyRunGuard
}

type ChannelMessageFilter struct {
	Scope          Scope
	ConversationID string
	ThreadRootID   string
	// ChannelTimeline lists roots and replies explicitly broadcast to the channel.
	// It cannot be combined with ThreadRootID.
	ChannelTimeline bool
	AfterSequence   int64
	BeforeSequence  int64
	Intents         []ConversationMessageIntent
	Limit           int
	Descending      bool
	Viewer          *ConversationViewer
}

// CommitConversationRoutingRequest records who answers one channel message.
type CommitConversationRoutingRequest struct {
	Scope            Scope
	ConversationID   string
	ExpectedRevision int64
	TriggerMessageID string
	Responders       []ConversationResponder
	IdempotencyKey   string
}

type ParticipationRoundFilter struct {
	Scope          Scope
	ConversationID string
	Limit          int
	Offset         int
}

type AdvanceConversationCursorRequest struct {
	Scope             Scope
	ConversationID    string
	Participant       ConversationParticipant
	ExpectedRevision  int64
	DeliveredSequence int64
	ReadSequence      int64
}

type SetConversationPresenceRequest struct {
	Scope            Scope
	ConversationID   string
	Participant      ConversationParticipant
	State            ConversationPresenceState
	Summary          string
	RunID            string
	LeaseID          string
	ExpectedRevision int64
	TTL              time.Duration
}

type ReleaseConversationPresenceRequest struct {
	Scope          Scope
	ConversationID string
	Participant    ConversationParticipant
	LeaseID        string
}

type ChannelMessageCommitResult struct {
	Conversation *Conversation   `json:"conversation"`
	Message      *ChannelMessage `json:"message"`
	Run          *AgentRun       `json:"run,omitempty"`
	Replayed     bool            `json:"replayed"`
}

type ParticipationRoundResult struct {
	Conversation *Conversation       `json:"conversation"`
	Round        *ParticipationRound `json:"round"`
	Messages     []*ChannelMessage   `json:"messages"`
	Replayed     bool                `json:"replayed"`
}

type ChannelMessageCommitRecord struct {
	Conversation     *Conversation
	ExpectedRevision int64
	Message          *ChannelMessage
	// RunGuard fences a Run's final reply. When set, the store locks the Run
	// before the conversation and commits only while replyRunGuardHolds; a
	// replay of an already stored IdempotencyKey returns that message
	// without consulting the guard.
	RunGuard *ReplyRunGuard
}

// validateReplyRunGuard checks a guard's shape before any I/O.
func validateReplyRunGuard(guard *ReplyRunGuard) error {
	if guard == nil {
		return nil
	}
	if err := guard.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(guard.RunID) == "" || strings.TrimSpace(guard.WorkerID) == "" || guard.InterventionCount < 0 {
		return ErrInvalidConversation
	}
	return nil
}

// replyRunGuardHolds is the guard check every backend runs on the locked
// Run row: the Run is still running under the guard's live lease, on the
// same lane, with no intervention beyond InterventionCount.
func replyRunGuardHolds(run *AgentRun, guard *ReplyRunGuard, now time.Time) error {
	if run.Status != AgentRunStatusRunning || run.LeaseOwner != guard.WorkerID ||
		run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) ||
		run.ConcurrencyKey != guard.ConcurrencyKey || len(run.PendingInterventions) != guard.InterventionCount {
		return ErrSteeringArrived
	}
	return nil
}

type ParticipationRoundCommitRecord struct {
	Conversation     *Conversation
	ExpectedRevision int64
	Round            *ParticipationRound
	Messages         []*ChannelMessage
}

type ConversationCursorRecord struct {
	Cursor           *ConversationCursor
	ExpectedRevision int64
}

type ConversationPresenceRecord struct {
	Presence         *ConversationPresence
	ExpectedRevision int64
}

type ConversationStore interface {
	CreateConversation(ctx context.Context, conversation *Conversation, idempotencyKey string) (*Conversation, bool, error)
	UpdateConversation(ctx context.Context, conversation *Conversation, expectedRevision int64) (*Conversation, error)
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
	FindConversationByIdempotencyKey(ctx context.Context, scope Scope, key string) (*Conversation, error)
	ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error)
	CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error)
	GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error)
	FindChannelMessageByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ChannelMessage, error)
	ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error)
	CommitParticipationRound(ctx context.Context, record ParticipationRoundCommitRecord) (*ParticipationRoundResult, error)
	GetParticipationRound(ctx context.Context, scope Scope, conversationID, roundID string) (*ParticipationRoundResult, error)
	FindParticipationRoundByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRoundResult, error)
	ListParticipationRounds(ctx context.Context, filter ParticipationRoundFilter) ([]*ParticipationRoundResult, error)
	GetConversationCursor(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error)
	ListConversationCursors(ctx context.Context, scope Scope, conversationID string) ([]*ConversationCursor, error)
	PutConversationCursor(ctx context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error)
	GetConversationPresence(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error)
	PutConversationPresence(ctx context.Context, record ConversationPresenceRecord) (*ConversationPresence, error)
	ReleaseConversationPresence(ctx context.Context, request ReleaseConversationPresenceRequest) error
	ListConversationPresence(ctx context.Context, scope Scope, conversationID string, activeAt time.Time) ([]*ConversationPresence, error)
	// CreateSurfaceInteraction assigns the interaction's Sequence and stores
	// it. It returns the stored interaction and whether it replayed one with
	// the same idempotency key; see SurfaceInteraction for uniqueness.
	CreateSurfaceInteraction(ctx context.Context, interaction *SurfaceInteraction) (*SurfaceInteraction, bool, error)
	ListSurfaceInteractions(ctx context.Context, filter SurfaceInteractionFilter) ([]*SurfaceInteraction, error)
	ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error)
	ConversationReactionStore
	// Terminal failure reports read the Run's session log and its action
	// calls to name what stopped the attempt, and note the reply there.
	AgentSessionStore
	actionCallReader
}

type ConversationService struct {
	store     ConversationStore
	now       func() time.Time
	newID     func() string
	reactions MessageReactionProjector
}

func NewConversationService(store ConversationStore) *ConversationService {
	return &ConversationService{store: store, now: time.Now, newID: uuid.NewString}
}

func (s *ConversationService) CreateConversation(ctx context.Context, req CreateConversationRequest) (*Conversation, bool, error) {
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
		return nil, false, fmt.Errorf("%w: idempotency key is required", ErrInvalidConversation)
	}
	if existing, err := s.store.FindConversationByIdempotencyKey(ctx, req.Scope, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.Owner != req.Owner || existing.Title != strings.TrimSpace(req.Title) ||
			!reflect.DeepEqual(existing.Origin, req.Origin) || !sameRequestedSpace(existing, req.SpaceID) {
			return nil, false, ErrMessageConflict
		}
		return existing, true, nil
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = stableConversationID(req.Scope, key, "conversation")
	}
	now := s.now().UTC()
	conversation := &Conversation{
		ID: id, Scope: req.Scope, Owner: req.Owner, SpaceID: strings.TrimSpace(req.SpaceID), Title: strings.TrimSpace(req.Title), Status: ConversationStatusActive,
		Origin: cloneConversationReference(req.Origin), ApprovalMode: DefaultConversationApprovalMode, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.CreateConversation(ctx, conversation, key)
}

// sameRequestedSpace reports whether an idempotent replay asked for the space
// the conversation already lives in; an omitted space matches any.
func sameRequestedSpace(existing *Conversation, spaceID string) bool {
	spaceID = strings.TrimSpace(spaceID)
	return spaceID == "" || existing.SpaceID == spaceID
}

func (s *ConversationService) GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error) {
	conversation, err := s.store.GetConversation(ctx, scope, strings.TrimSpace(conversationID))
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	return conversation, nil
}

func (s *ConversationService) UpdateConversation(ctx context.Context, req UpdateConversationRequest) (*Conversation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", ErrInvalidConversation)
	}
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	updated := cloneConversation(conversation)
	if req.Title != nil {
		updated.Title = strings.TrimSpace(*req.Title)
	}
	if req.Status != nil {
		updated.Status = *req.Status
		if updated.Status == ConversationStatusArchived {
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
		if *req.ParticipationEnabled && (updated.Status != ConversationStatusActive || (updated.Owner.Type != OwnerTypeTeam && updated.Owner.Type != OwnerTypeAgent)) {
			return nil, fmt.Errorf("%w: participation requires an active Agent or Team channel", ErrInvalidConversation)
		}
		if updated.Participation == nil {
			updated.Participation = &ConversationParticipation{}
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

func (s *ConversationService) ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error) {
	return s.store.ListConversations(ctx, filter)
}

func (s *ConversationService) PostChannelMessage(ctx context.Context, req PostChannelMessageRequest) (*ChannelMessageCommitResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, fmt.Errorf("%w: message idempotency key is required", ErrInvalidConversation)
	}
	if existing, err := s.store.FindChannelMessageByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if !sameChannelMessageRequest(existing, req) {
			return nil, ErrMessageConflict
		}
		conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
		return &ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, err
	}
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != ConversationStatusActive || conversation.Revision != req.ExpectedRevision {
		if conversation.Revision != req.ExpectedRevision {
			if existing, findErr := s.store.FindChannelMessageByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); findErr != nil {
				return nil, findErr
			} else if existing != nil {
				if !sameChannelMessageRequest(existing, req) {
					return nil, ErrMessageConflict
				}
				return &ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, nil
			}
			return nil, ErrRevisionConflict
		}
		return nil, ErrInvalidConversation
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
				return nil, ErrMessageConflict
			}
		}
	}
	if err := s.validateSelectedOptions(ctx, req); err != nil {
		return nil, err
	}
	if req.BuilderParameters != nil && (conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceAgentBuilder) {
		return nil, fmt.Errorf("%w: builder parameters belong to an agent builder conversation", ErrInvalidConversation)
	}
	now := s.now().UTC()
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = stableConversationID(req.Scope, key, "message")
	}
	message := &ChannelMessage{
		ID: id, Scope: req.Scope, ConversationID: conversation.ID, Sequence: conversation.LastSequence + 1,
		Sender: req.Sender, SenderDisplayName: strings.TrimSpace(req.SenderDisplayName),
		Initiator:      cloneConversationParticipant(req.Initiator),
		ExternalSource: cloneExternalMessageSource(req.ExternalSource),
		Intent:         req.Intent, Content: strings.TrimSpace(req.Content), Audience: req.Audience,
		StartsThread: req.StartThread, ThreadRootID: threadRootID, ReplyToMessageID: strings.TrimSpace(req.ReplyToMessageID), Quote: quote, BroadcastToChannel: req.BroadcastToChannel,
		Question: cloneChannelQuestion(req.Question), SelectedOptionIDs: selectedOptionIDs(req.SelectedOptionIDs),
		BuilderParameters: normalizeAgentBuilderParameters(req.BuilderParameters),
		Mentions:          cloneParticipants(req.Mentions),
		References:        cloneConversationReferences(req.References), RequiresResponse: req.RequiresResponse, ResponseMode: req.ResponseMode,
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
			return nil, fmt.Errorf("%w: thread ancestry is missing, cyclic, or too deep", ErrInvalidConversation)
		}
	}
	updated := cloneConversation(conversation)
	updated.LastSequence = message.Sequence
	updated.Revision++
	updated.UpdatedAt = now
	return s.store.CommitChannelMessage(ctx, ChannelMessageCommitRecord{Conversation: updated, ExpectedRevision: conversation.Revision, Message: message, RunGuard: req.RunGuard})
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
		return fmt.Errorf("%w: selected options must answer a question", ErrInvalidConversation)
	}
	question, err := s.store.GetChannelMessage(ctx, req.Scope, req.ConversationID, target)
	if err != nil {
		return err
	}
	if question == nil || question.Question == nil {
		return fmt.Errorf("%w: selected options answer a message that offers none", ErrInvalidConversation)
	}
	for _, id := range ids {
		if !question.Question.hasOption(id) {
			return fmt.Errorf("%w: %q is not an option of that question", ErrInvalidConversation, id)
		}
	}
	if len(ids) > 1 && !question.Question.AllowMultiple {
		return fmt.Errorf("%w: that question takes one option", ErrInvalidConversation)
	}
	return nil
}

// CommitConversationRouting durably records a routing decision. It posts no
// messages: every responder commits its own answer from its own Run.
func (s *ConversationService) CommitConversationRouting(ctx context.Context, req CommitConversationRoutingRequest) (*ParticipationRoundResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, fmt.Errorf("%w: routing idempotency key is required", ErrInvalidConversation)
	}
	roundID := stableConversationID(req.Scope, key, "round")
	now := s.now().UTC()
	round := &ParticipationRound{
		ID: roundID, Scope: req.Scope, ConversationID: strings.TrimSpace(req.ConversationID),
		TriggerMessageID: strings.TrimSpace(req.TriggerMessageID), Status: ParticipationRoundCommitted,
		Responders: append([]ConversationResponder(nil), req.Responders...), IdempotencyKey: key, Revision: 1, CreatedAt: now, CommittedAt: now,
	}
	if existing, err := s.replayConversationRouting(ctx, round); err != nil || existing != nil {
		return existing, err
	}
	conversation, err := s.GetConversation(ctx, req.Scope, round.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != ConversationStatusActive {
		return nil, ErrInvalidConversation
	}
	if conversation.Revision != req.ExpectedRevision {
		// A concurrent replica may have committed this same request between the
		// idempotency lookup and the conversation read; that is a replay, not a
		// conflict.
		if existing, err := s.replayConversationRouting(ctx, round); err != nil || existing != nil {
			return existing, err
		}
		return nil, ErrRevisionConflict
	}
	if round.TriggerMessageID != "" {
		if trigger, err := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, round.TriggerMessageID); err != nil {
			return nil, err
		} else if trigger == nil {
			return nil, ErrMessageConflict
		}
	}
	updated := cloneConversation(conversation)
	updated.Revision++
	updated.UpdatedAt = now
	round.ConversationRevision = updated.Revision
	if err := round.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConversation, err)
	}
	return s.store.CommitParticipationRound(ctx, ParticipationRoundCommitRecord{
		Conversation: updated, ExpectedRevision: conversation.Revision, Round: round,
	})
}

// replayConversationRouting returns the already committed round for the
// request's idempotency key, or nil when none exists yet.
func (s *ConversationService) replayConversationRouting(ctx context.Context, round *ParticipationRound) (*ParticipationRoundResult, error) {
	existing, err := s.store.FindParticipationRoundByIdempotencyKey(ctx, round.Scope, round.ConversationID, round.IdempotencyKey)
	if err != nil || existing == nil {
		return nil, err
	}
	if !sameParticipationRound(existing.Round, round) {
		return nil, ErrMessageConflict
	}
	return existing, nil
}

func (s *ConversationService) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	if err := validateChannelMessageViewFilter(filter); err != nil {
		return nil, err
	}
	if filter.BeforeSequence < 0 || filter.AfterSequence < 0 || (filter.BeforeSequence > 0 && filter.AfterSequence >= filter.BeforeSequence) {
		return nil, fmt.Errorf("%w: message sequence bounds are invalid", ErrInvalidConversation)
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
	result := make([]*ChannelMessage, 0, desired)
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

func validateChannelMessageViewFilter(filter ChannelMessageFilter) error {
	if filter.ChannelTimeline && filter.ThreadRootID != "" {
		return fmt.Errorf("%w: channel timeline and thread root filters cannot be combined", ErrInvalidConversation)
	}
	return nil
}

func (s *ConversationService) filterVisibleChannelMessages(ctx context.Context, scope Scope, conversationID string, messages []*ChannelMessage, viewer ConversationViewer) ([]*ChannelMessage, error) {
	visible, err := s.filterVisibleChannelMessageParents(ctx, scope, conversationID, messages, viewer)
	if err != nil {
		return nil, err
	}
	return s.redactUnviewableQuotes(ctx, scope, conversationID, visible, viewer)
}

// redactUnviewableQuotes keeps a quote's sender and excerpt only for viewers
// who can see the quoted message itself.
func (s *ConversationService) redactUnviewableQuotes(ctx context.Context, scope Scope, conversationID string, messages []*ChannelMessage, viewer ConversationViewer) ([]*ChannelMessage, error) {
	ids := make([]string, 0)
	for _, message := range messages {
		if message != nil && message.Quote != nil && !message.Quote.Unavailable {
			ids = append(ids, message.Quote.MessageID)
		}
	}
	if len(ids) == 0 {
		return messages, nil
	}
	quoted := make(map[string]*ChannelMessage, len(ids))
	if batch, ok := s.store.(ChannelMessageBatchStore); ok {
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
	out := make([]*ChannelMessage, len(messages))
	for i, message := range messages {
		out[i] = message
		if message != nil && message.Quote != nil && !message.Quote.Unavailable && !CanViewChannelMessage(quoted[message.Quote.MessageID], viewer) {
			redacted := cloneChannelMessage(message)
			redacted.Quote = message.Quote.unavailable()
			out[i] = redacted
		}
	}
	return out, nil
}

func (s *ConversationService) filterVisibleChannelMessageParents(ctx context.Context, scope Scope, conversationID string, messages []*ChannelMessage, viewer ConversationViewer) ([]*ChannelMessage, error) {
	if err := viewer.Validate(); err != nil {
		return nil, err
	}
	if batch, ok := s.store.(ChannelMessageBatchStore); ok {
		return filterChannelMessagesWithBatchParents(ctx, batch, scope, conversationID, messages, viewer)
	}
	visible := make([]*ChannelMessage, 0, len(messages))
	var known map[string]*ChannelMessage
	for _, message := range messages {
		if !CanViewChannelMessage(message, viewer) {
			continue
		}
		if message.StartsThread && known == nil {
			known = make(map[string]*ChannelMessage)
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
				known = make(map[string]*ChannelMessage)
				known[message.ID] = message
			}
			if known != nil {
				known[linked] = parent
			}
			if !CanViewChannelMessage(parent, viewer) {
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

func (s *ConversationService) GetVisibleChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string, viewer ConversationViewer) (*ChannelMessage, error) {
	message, err := s.GetChannelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	visible, err := s.filterVisibleChannelMessages(ctx, scope, conversationID, []*ChannelMessage{message}, viewer)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return nil, ErrChannelMessageNotFound
	}
	return visible[0], nil
}

func (s *ConversationService) GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrChannelMessageNotFound
	}
	filled, err := withMessageReactions(ctx, s.store, []*ChannelMessage{message})
	if err != nil {
		return nil, err
	}
	return filled[0], nil
}

func (s *ConversationService) GetParticipationRound(ctx context.Context, scope Scope, conversationID, roundID string) (*ParticipationRoundResult, error) {
	result, err := s.store.GetParticipationRound(ctx, scope, conversationID, roundID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, ErrParticipationRoundNotFound
	}
	return result, nil
}

func (s *ConversationService) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRoundResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(strings.TrimSpace(conversationID), 128) || strings.TrimSpace(key) == "" || len(key) > 256 {
		return nil, fmt.Errorf("%w: conversation and idempotency key are required", ErrInvalidConversation)
	}
	return s.store.FindParticipationRoundByIdempotencyKey(ctx, scope, strings.TrimSpace(conversationID), strings.TrimSpace(key))
}

func (s *ConversationService) ListParticipationRounds(ctx context.Context, filter ParticipationRoundFilter) ([]*ParticipationRoundResult, error) {
	return s.store.ListParticipationRounds(ctx, filter)
}

func (s *ConversationService) GetCursor(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	return s.store.GetConversationCursor(ctx, scope, conversationID, participant)
}

// ListCursors returns the durable delivery and read position for every
// participant that has observed a conversation. The result is ordered by
// participant identity so API projections remain deterministic across stores.
func (s *ConversationService) ListCursors(ctx context.Context, scope Scope, conversationID string) ([]*ConversationCursor, error) {
	if _, err := s.GetConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	return s.store.ListConversationCursors(ctx, scope, conversationID)
}

func (s *ConversationService) AdvanceCursor(ctx context.Context, req AdvanceConversationCursorRequest) (*ConversationCursor, bool, error) {
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, false, err
	}
	if err := req.Participant.Validate(); err != nil {
		return nil, false, err
	}
	if req.DeliveredSequence < 0 || req.ReadSequence < 0 || req.ReadSequence > req.DeliveredSequence || req.DeliveredSequence > conversation.LastSequence {
		return nil, false, ErrConversationCursorConflict
	}
	current, err := s.store.GetConversationCursor(ctx, req.Scope, conversation.ID, req.Participant)
	if err != nil {
		return nil, false, err
	}
	if current != nil {
		if current.Revision != req.ExpectedRevision || req.DeliveredSequence < current.DeliveredSequence || req.ReadSequence < current.ReadSequence {
			return nil, false, ErrConversationCursorConflict
		}
		if req.DeliveredSequence == current.DeliveredSequence && req.ReadSequence == current.ReadSequence {
			return current, true, nil
		}
	} else if req.ExpectedRevision != 0 {
		return nil, false, ErrConversationCursorConflict
	}
	revision := int64(1)
	if current != nil {
		revision = current.Revision + 1
	}
	cursor := &ConversationCursor{
		Scope: req.Scope, ConversationID: conversation.ID, Participant: req.Participant,
		DeliveredSequence: req.DeliveredSequence, ReadSequence: req.ReadSequence, Revision: revision, UpdatedAt: s.now().UTC(),
	}
	if err := cursor.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.PutConversationCursor(ctx, ConversationCursorRecord{Cursor: cursor, ExpectedRevision: req.ExpectedRevision})
}

func (s *ConversationService) SetPresence(ctx context.Context, req SetConversationPresenceRequest) (*ConversationPresence, error) {
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
		return nil, fmt.Errorf("%w: presence TTL must be between one second and two minutes", ErrInvalidConversation)
	}
	now := s.now().UTC()
	current, err := s.store.GetConversationPresence(ctx, req.Scope, req.ConversationID, req.Participant)
	if err != nil {
		return nil, err
	}
	leaseID := strings.TrimSpace(req.LeaseID)
	if current != nil && current.ExpiresAt.After(now) {
		if leaseID == "" || leaseID != current.LeaseID || current.Revision != req.ExpectedRevision {
			return nil, ErrConversationPresenceConflict
		}
	} else if req.ExpectedRevision != 0 {
		return nil, ErrConversationPresenceConflict
	}
	if leaseID == "" {
		leaseID = s.newID()
	}
	revision := int64(1)
	if current != nil && current.ExpiresAt.After(now) {
		revision = current.Revision + 1
	}
	presence := &ConversationPresence{
		Scope: req.Scope, ConversationID: req.ConversationID, Participant: req.Participant, State: req.State,
		Summary: strings.TrimSpace(req.Summary), RunID: strings.TrimSpace(req.RunID), LeaseID: leaseID,
		Revision: revision, UpdatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := presence.Validate(); err != nil {
		return nil, err
	}
	return s.store.PutConversationPresence(ctx, ConversationPresenceRecord{Presence: presence, ExpectedRevision: req.ExpectedRevision})
}

func (s *ConversationService) ReleasePresence(ctx context.Context, req ReleaseConversationPresenceRequest) error {
	if s == nil || s.store == nil {
		return errors.New("conversation store is not configured")
	}
	return s.store.ReleaseConversationPresence(ctx, req)
}

func (s *ConversationService) ListPresence(ctx context.Context, scope Scope, conversationID string) ([]*ConversationPresence, error) {
	return s.store.ListConversationPresence(ctx, scope, conversationID, s.now().UTC())
}

func (s *ConversationService) resolveThreadRoot(ctx context.Context, scope Scope, conversationID, replyTo string) (string, error) {
	replyTo = strings.TrimSpace(replyTo)
	if replyTo == "" {
		return "", nil
	}
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, replyTo)
	if err != nil {
		return "", err
	}
	if message == nil {
		return "", ErrMessageConflict
	}
	if message.ThreadRootID != "" {
		return message.ThreadRootID, nil
	}
	return message.ID, nil
}

func (s *ConversationService) resolveQuote(ctx context.Context, scope Scope, conversationID, quotedID string) (*ChannelMessageQuote, error) {
	quotedID = strings.TrimSpace(quotedID)
	if quotedID == "" {
		return nil, nil
	}
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, quotedID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrMessageConflict
	}
	return newChannelMessageQuote(message), nil
}

// answerQuoteID returns the explicit quote, or quotes the message an Agent or
// Team is answering when that answer would otherwise read out of order.
func (s *ConversationService) answerQuoteID(ctx context.Context, req PostChannelMessageRequest) (string, error) {
	if quoted := strings.TrimSpace(req.QuotedMessageID); quoted != "" {
		return quoted, nil
	}
	replyTo := strings.TrimSpace(req.ReplyToMessageID)
	if replyTo == "" || req.Intent != MessageIntentAnswer ||
		(req.Sender.Type != ConversationParticipantAgent && req.Sender.Type != ConversationParticipantTeam) {
		return "", nil
	}
	recent, err := s.store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: req.Scope, ConversationID: req.ConversationID, Limit: 50, Descending: true})
	if err != nil {
		return "", err
	}
	if answerNeedsQuote(replyTo, req.Sender, recent) {
		return replyTo, nil
	}
	return "", nil
}

func quotedMessageID(quote *ChannelMessageQuote) string {
	if quote == nil {
		return ""
	}
	return quote.MessageID
}

// answerNeedsQuote reports whether an answer to triggerID would read out of
// order: someone other than the answerer has already spoken after the trigger
// in the same view (main chat or the same started thread). recent is newest first.
func answerNeedsQuote(triggerID string, answerer ConversationParticipant, recent []*ChannelMessage) bool {
	byID := make(map[string]*ChannelMessage, len(recent))
	for _, message := range recent {
		if message != nil {
			byID[message.ID] = message
		}
	}
	trigger := byID[triggerID]
	if trigger == nil {
		return false
	}
	view := func(message *ChannelMessage) string {
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
		if message.Sender != answerer && message.Intent != MessageIntentSystem && message.Sender.Type != ConversationParticipantService && view(message) == triggerView {
			return true
		}
	}
	return false
}

func stableConversationID(scope Scope, key, suffix string) string {
	seed := strings.Join([]string{"openseal", "conversation", scope.key(), strings.TrimSpace(key), strings.TrimSpace(suffix)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func sameChannelMessageRequest(existing *ChannelMessage, req PostChannelMessageRequest) bool {
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
		reflect.DeepEqual(existing.BuilderParameters, normalizeAgentBuilderParameters(req.BuilderParameters)) &&
		existing.RequiresResponse == req.RequiresResponse && existing.ResolvesMessageID == strings.TrimSpace(req.ResolvesMessageID) &&
		existing.SupersedesMessageID == strings.TrimSpace(req.SupersedesMessageID)
}

func cloneConversationReference(in *ConversationReference) *ConversationReference {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneConversation(in *Conversation) *Conversation {
	if in == nil {
		return nil
	}
	out := *in
	out.RunbookActivationIDs = append([]string(nil), in.RunbookActivationIDs...)
	if len(out.RunbookActivationIDs) == 0 {
		out.RunbookActivationIDs = nil
	}
	if in.Participation != nil {
		participation := *in.Participation
		out.Participation = &participation
	}
	if in.Origin != nil {
		origin := *in.Origin
		out.Origin = &origin
	}
	if in.ArchivedAt != nil {
		archived := *in.ArchivedAt
		out.ArchivedAt = &archived
	}
	return &out
}

func cloneChannelMessage(in *ChannelMessage) *ChannelMessage {
	if in == nil {
		return nil
	}
	out := *in
	out.Initiator = cloneConversationParticipant(in.Initiator)
	out.ExternalSource = cloneExternalMessageSource(in.ExternalSource)
	out.Audience.Participants = cloneParticipants(in.Audience.Participants)
	out.Audience.Roles = append([]string(nil), in.Audience.Roles...)
	out.Mentions = cloneParticipants(in.Mentions)
	out.References = cloneConversationReferences(in.References)
	out.Quote = cloneChannelMessageQuote(in.Quote)
	out.Question = cloneChannelQuestion(in.Question)
	out.SelectedOptionIDs = append([]string(nil), in.SelectedOptionIDs...)
	if len(out.SelectedOptionIDs) == 0 {
		out.SelectedOptionIDs = nil
	}
	out.BuilderParameters = cloneAgentBuilderParameters(in.BuilderParameters)
	out.Reactions = cloneMessageReactions(in.Reactions)
	return &out
}

func cloneConversationParticipant(in *ConversationParticipant) *ConversationParticipant {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneParticipants(in []ConversationParticipant) []ConversationParticipant {
	return append([]ConversationParticipant(nil), in...)
}

func cloneConversationReferences(in []ConversationReference) []ConversationReference {
	return append([]ConversationReference(nil), in...)
}

func cloneConversationCursor(in *ConversationCursor) *ConversationCursor {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneConversationPresence(in *ConversationPresence) *ConversationPresence {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneChannelMessages(in []*ChannelMessage) []*ChannelMessage {
	out := make([]*ChannelMessage, len(in))
	for index := range in {
		out[index] = cloneChannelMessage(in[index])
	}
	return out
}
