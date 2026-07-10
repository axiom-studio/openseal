package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

type CreateConversationRequest struct {
	ID             string
	Scope          Scope
	Owner          ObjectiveOwner
	Title          string
	IdempotencyKey string
}

type ConversationFilter struct {
	Scope    Scope
	Owner    *ObjectiveOwner
	Statuses []ConversationStatus
	Limit    int
	Offset   int
}

type PostChannelMessageRequest struct {
	ID                  string
	Scope               Scope
	ConversationID      string
	ExpectedRevision    int64
	Sender              ConversationParticipant
	Intent              ConversationMessageIntent
	Content             string
	Audience            ConversationAudience
	ReplyToMessageID    string
	Mentions            []ConversationParticipant
	References          []ConversationReference
	RequiresResponse    bool
	ResolvesMessageID   string
	SupersedesMessageID string
	IdempotencyKey      string
}

type ChannelMessageFilter struct {
	Scope          Scope
	ConversationID string
	ThreadRootID   string
	AfterSequence  int64
	Intents        []ConversationMessageIntent
	Limit          int
	Descending     bool
}

type CoordinateParticipationRequest struct {
	ID               string
	Scope            Scope
	ConversationID   string
	ExpectedRevision int64
	TriggerMessageID string
	Policy           ConversationArbitrationPolicy
	Proposals        []ParticipationProposal
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
	PutConversationCursor(ctx context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error)
	GetConversationPresence(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error)
	PutConversationPresence(ctx context.Context, record ConversationPresenceRecord) (*ConversationPresence, error)
	ReleaseConversationPresence(ctx context.Context, request ReleaseConversationPresenceRequest) error
	ListConversationPresence(ctx context.Context, scope Scope, conversationID string, activeAt time.Time) ([]*ConversationPresence, error)
}

type ConversationService struct {
	store ConversationStore
	now   func() time.Time
	newID func() string
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
		if existing.Owner != req.Owner || existing.Title != strings.TrimSpace(req.Title) {
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
		ID: id, Scope: req.Scope, Owner: req.Owner, Title: strings.TrimSpace(req.Title), Status: ConversationStatusActive,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.CreateConversation(ctx, conversation, key)
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
	now := s.now().UTC()
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = stableConversationID(req.Scope, key, "message")
	}
	message := &ChannelMessage{
		ID: id, Scope: req.Scope, ConversationID: conversation.ID, Sequence: conversation.LastSequence + 1,
		Sender: req.Sender, Intent: req.Intent, Content: strings.TrimSpace(req.Content), Audience: req.Audience,
		ThreadRootID: threadRootID, ReplyToMessageID: strings.TrimSpace(req.ReplyToMessageID), Mentions: cloneParticipants(req.Mentions),
		References: cloneConversationReferences(req.References), RequiresResponse: req.RequiresResponse,
		ResolvesMessageID: strings.TrimSpace(req.ResolvesMessageID), SupersedesMessageID: strings.TrimSpace(req.SupersedesMessageID),
		IdempotencyKey: key, CreatedAt: now,
	}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	updated := cloneConversation(conversation)
	updated.LastSequence = message.Sequence
	updated.Revision++
	updated.UpdatedAt = now
	return s.store.CommitChannelMessage(ctx, ChannelMessageCommitRecord{Conversation: updated, ExpectedRevision: conversation.Revision, Message: message})
}

func (s *ConversationService) CoordinateParticipation(ctx context.Context, req CoordinateParticipationRequest) (*ParticipationRoundResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(req.Proposals) == 0 {
		return nil, fmt.Errorf("%w: participation idempotency key and proposals are required", ErrInvalidConversation)
	}
	roundID := strings.TrimSpace(req.ID)
	if roundID == "" {
		roundID = stableConversationID(req.Scope, key, "round")
	}
	proposals := cloneParticipationProposals(req.Proposals)
	for index := range proposals {
		proposals[index].RoundID = roundID
		if strings.TrimSpace(proposals[index].ID) == "" {
			proposals[index].ID = stableConversationID(req.Scope, key, string(proposals[index].Participant.Type)+":"+proposals[index].Participant.ID)
		}
	}
	normalizedPolicy, err := req.Policy.normalize()
	if err != nil {
		return nil, err
	}
	if existing, err := s.store.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if !sameParticipationRoundIntent(existing.Round, req, roundID, proposals, normalizedPolicy) {
			return nil, ErrMessageConflict
		}
		return existing, nil
	}
	conversation, err := s.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != ConversationStatusActive || conversation.Revision != req.ExpectedRevision {
		if conversation.Revision != req.ExpectedRevision {
			if existing, findErr := s.store.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); findErr != nil {
				return nil, findErr
			} else if existing != nil {
				if !sameParticipationRoundIntent(existing.Round, req, roundID, proposals, normalizedPolicy) {
					return nil, ErrMessageConflict
				}
				return existing, nil
			}
			return nil, ErrRevisionConflict
		}
		return nil, ErrInvalidConversation
	}
	if req.TriggerMessageID != "" {
		trigger, err := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, req.TriggerMessageID)
		if err != nil {
			return nil, err
		}
		if trigger == nil {
			return nil, ErrMessageConflict
		}
	}
	recent, err := s.store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: req.Scope, ConversationID: conversation.ID, Limit: 100, Descending: true})
	if err != nil {
		return nil, err
	}
	arbitration, err := ArbitrateParticipation(roundID, proposals, recent, normalizedPolicy)
	if err != nil {
		return nil, err
	}
	proposalByID := make(map[string]ParticipationProposal, len(proposals))
	for _, proposal := range proposals {
		proposalByID[proposal.ID] = proposal
	}
	now := s.now().UTC()
	messages := make([]*ChannelMessage, 0, len(arbitration.Speakers))
	sequence := conversation.LastSequence
	for _, proposalID := range arbitration.Speakers {
		proposal := proposalByID[proposalID]
		threadRootID, err := s.resolveThreadRoot(ctx, req.Scope, conversation.ID, proposal.ReplyToMessageID)
		if err != nil {
			return nil, err
		}
		sequence++
		message := &ChannelMessage{
			ID: stableConversationID(req.Scope, key, "speaker:"+proposal.ID), Scope: req.Scope, ConversationID: conversation.ID,
			Sequence: sequence, Sender: proposal.Participant, Intent: proposal.Intent, Content: strings.TrimSpace(proposal.Content),
			Audience: proposal.Audience, ThreadRootID: threadRootID, ReplyToMessageID: proposal.ReplyToMessageID,
			Mentions: cloneParticipants(proposal.Mentions), References: cloneConversationReferences(proposal.References),
			RequiresResponse: proposal.RequiresResponse, ResolvesMessageID: proposal.ResolvesMessageID,
			IdempotencyKey: key + ":speaker:" + proposal.ID, ParticipationRoundID: roundID, CreatedAt: now,
		}
		if err := message.Validate(); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	round := &ParticipationRound{
		ID: roundID, Scope: req.Scope, ConversationID: conversation.ID, TriggerMessageID: strings.TrimSpace(req.TriggerMessageID),
		Status: ParticipationRoundCommitted, Policy: normalizedPolicy, Proposals: proposals, Arbitration: *arbitration,
		IdempotencyKey: key, Revision: 1, CreatedAt: now, CommittedAt: now,
	}
	if err := round.Validate(); err != nil {
		return nil, err
	}
	updated := cloneConversation(conversation)
	updated.LastSequence = sequence
	updated.Revision++
	updated.UpdatedAt = now
	return s.store.CommitParticipationRound(ctx, ParticipationRoundCommitRecord{
		Conversation: updated, ExpectedRevision: conversation.Revision, Round: round, Messages: messages,
	})
}

func (s *ConversationService) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	return s.store.ListChannelMessages(ctx, filter)
}

func (s *ConversationService) GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	message, err := s.store.GetChannelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrChannelMessageNotFound
	}
	return message, nil
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

func (s *ConversationService) ListParticipationRounds(ctx context.Context, filter ParticipationRoundFilter) ([]*ParticipationRoundResult, error) {
	return s.store.ListParticipationRounds(ctx, filter)
}

func (s *ConversationService) GetCursor(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	return s.store.GetConversationCursor(ctx, scope, conversationID, participant)
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

func stableConversationID(scope Scope, key, suffix string) string {
	seed := strings.Join([]string{"openseal", "conversation", scope.key(), strings.TrimSpace(key), strings.TrimSpace(suffix)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func sameChannelMessageRequest(existing *ChannelMessage, req PostChannelMessageRequest) bool {
	return existing != nil && existing.Scope == req.Scope && existing.ConversationID == strings.TrimSpace(req.ConversationID) &&
		existing.Sender == req.Sender && existing.Intent == req.Intent && existing.Content == strings.TrimSpace(req.Content) &&
		existing.Audience.Kind == req.Audience.Kind && reflect.DeepEqual(existing.Audience.Participants, req.Audience.Participants) &&
		reflect.DeepEqual(existing.Audience.Roles, req.Audience.Roles) && existing.ReplyToMessageID == strings.TrimSpace(req.ReplyToMessageID) &&
		reflect.DeepEqual(existing.Mentions, req.Mentions) && reflect.DeepEqual(existing.References, req.References) &&
		existing.RequiresResponse == req.RequiresResponse && existing.ResolvesMessageID == strings.TrimSpace(req.ResolvesMessageID) &&
		existing.SupersedesMessageID == strings.TrimSpace(req.SupersedesMessageID)
}

func sameParticipationRoundIntent(existing *ParticipationRound, req CoordinateParticipationRequest, roundID string, proposals []ParticipationProposal, policy ConversationArbitrationPolicy) bool {
	return existing != nil && existing.ID == roundID && existing.Scope == req.Scope &&
		existing.ConversationID == strings.TrimSpace(req.ConversationID) && existing.TriggerMessageID == strings.TrimSpace(req.TriggerMessageID) &&
		existing.Policy == policy && reflect.DeepEqual(existing.Proposals, proposals)
}

func cloneConversation(in *Conversation) *Conversation {
	if in == nil {
		return nil
	}
	out := *in
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
	out.Audience.Participants = cloneParticipants(in.Audience.Participants)
	out.Audience.Roles = append([]string(nil), in.Audience.Roles...)
	out.Mentions = cloneParticipants(in.Mentions)
	out.References = cloneConversationReferences(in.References)
	return &out
}

func cloneParticipants(in []ConversationParticipant) []ConversationParticipant {
	return append([]ConversationParticipant(nil), in...)
}

func cloneConversationReferences(in []ConversationReference) []ConversationReference {
	return append([]ConversationReference(nil), in...)
}

func cloneParticipationProposals(in []ParticipationProposal) []ParticipationProposal {
	out := make([]ParticipationProposal, len(in))
	for index := range in {
		out[index] = in[index]
		out[index].SemanticRoles = append([]string(nil), in[index].SemanticRoles...)
		out[index].Audience.Participants = cloneParticipants(in[index].Audience.Participants)
		out[index].Audience.Roles = append([]string(nil), in[index].Audience.Roles...)
		out[index].Mentions = cloneParticipants(in[index].Mentions)
		out[index].References = cloneConversationReferences(in[index].References)
	}
	return out
}

func cloneParticipationRound(in *ParticipationRound) *ParticipationRound {
	if in == nil {
		return nil
	}
	out := *in
	out.Proposals = cloneParticipationProposals(in.Proposals)
	out.Arbitration.Decisions = append([]ParticipationDecision(nil), in.Arbitration.Decisions...)
	for index := range out.Arbitration.Decisions {
		out.Arbitration.Decisions[index].Reasons = append([]ParticipationReason(nil), in.Arbitration.Decisions[index].Reasons...)
	}
	out.Arbitration.Speakers = append([]string(nil), in.Arbitration.Speakers...)
	return &out
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
