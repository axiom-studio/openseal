package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultConversationChangeLimit = 100
	MaximumConversationChangeLimit = 500
)

// MaximumChannelMessageBatchSize bounds optional exact-ID store reads. A
// channel poll chunks larger selections rather than enlarging the SQL query.
const MaximumChannelMessageBatchSize = 200

// ChannelMessageBatchStore is an optional extension to ConversationStore.
// Reads return only the exact scope, conversation and message IDs requested;
// missing messages are omitted, and result order is unspecified. Callers still
// apply the current viewer's audience and canonical ancestry visibility checks.
type ChannelMessageBatchStore interface {
	GetChannelMessagesByIDs(context.Context, Scope, string, []string) ([]*ChannelMessage, error)
}

func ValidateChannelMessageBatch(scope Scope, ids []string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if len(ids) > MaximumChannelMessageBatchSize {
		return fmt.Errorf("%w: message batch must contain at most %d IDs", ErrInvalidConversation, MaximumChannelMessageBatchSize)
	}
	return nil
}

// MaximumConversationRunbooks bounds the routines linked to one
// conversation; linking more drops the oldest links.
const MaximumConversationRunbooks = 32

type ConversationFilter struct {
	Scope   Scope
	Owner   *ObjectiveOwner
	SpaceID string
	// SpaceIDs, when not nil, keeps only conversations in these spaces; an
	// empty list keeps none.
	SpaceIDs []string
	Statuses []ConversationStatus
	// OriginKind keeps only conversations whose Origin has this kind; empty
	// lists every origin.
	OriginKind ConversationReferenceKind
	// AgentBuilderDraftOf keeps only the builder conversations this person
	// started that have not produced their Agent or Team.
	AgentBuilderDraftOf string
	Limit               int
	Offset              int
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

type ParticipationRoundFilter struct {
	Scope          Scope
	ConversationID string
	Limit          int
	Offset         int
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
	// before the conversation and commits only while ReplyRunGuardHolds; a
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

// ReplyRunGuardHolds is the guard check every backend runs on the locked
// Run row: the Run is still running under the guard's live lease, on the
// same lane, with no intervention beyond InterventionCount.
func ReplyRunGuardHolds(run *AgentRun, guard *ReplyRunGuard, now time.Time) error {
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
	// LastConversationActivity returns, by owner id, the newest UpdatedAt of
	// each owner's active conversations that hold a message. An owner with
	// none is absent.
	LastConversationActivity(ctx context.Context, scope Scope, ownerType OwnerType, ownerIDs []string) (map[string]time.Time, error)
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
	ActionCallReader
}

// SameRequestedSpace reports whether an idempotent replay asked for the space
// the conversation already lives in; an omitted space matches any.
func SameRequestedSpace(existing *Conversation, spaceID string) bool {
	spaceID = strings.TrimSpace(spaceID)
	return spaceID == "" || existing.SpaceID == spaceID
}

func ValidateChannelMessageViewFilter(filter ChannelMessageFilter) error {
	if filter.ChannelTimeline && filter.ThreadRootID != "" {
		return fmt.Errorf("%w: channel timeline and thread root filters cannot be combined", ErrInvalidConversation)
	}
	return nil
}

func StableConversationID(scope Scope, key, suffix string) string {
	seed := strings.Join([]string{"openseal", "conversation", scope.Key(), strings.TrimSpace(key), strings.TrimSpace(suffix)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func CloneConversation(in *Conversation) *Conversation {
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

func CloneChannelMessage(in *ChannelMessage) *ChannelMessage {
	if in == nil {
		return nil
	}
	out := *in
	out.Initiator = CloneConversationParticipant(in.Initiator)
	out.ExternalSource = CloneExternalMessageSource(in.ExternalSource)
	out.Audience.Participants = CloneConversationParticipants(in.Audience.Participants)
	out.Audience.Roles = append([]string(nil), in.Audience.Roles...)
	out.Mentions = CloneConversationParticipants(in.Mentions)
	out.References = CloneConversationReferences(in.References)
	out.Quote = CloneChannelMessageQuote(in.Quote)
	out.Question = CloneChannelQuestion(in.Question)
	out.SelectedOptionIDs = append([]string(nil), in.SelectedOptionIDs...)
	if len(out.SelectedOptionIDs) == 0 {
		out.SelectedOptionIDs = nil
	}
	out.BuilderParameters = cloneAgentBuilderParameters(in.BuilderParameters)
	out.Reactions = CloneMessageReactions(in.Reactions)
	return &out
}

func CloneConversationParticipant(in *ConversationParticipant) *ConversationParticipant {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func CloneConversationParticipants(in []ConversationParticipant) []ConversationParticipant {
	return append([]ConversationParticipant(nil), in...)
}

func CloneConversationReferences(in []ConversationReference) []ConversationReference {
	return append([]ConversationReference(nil), in...)
}

func CloneConversationCursor(in *ConversationCursor) *ConversationCursor {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func CloneConversationPresence(in *ConversationPresence) *ConversationPresence {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func CloneChannelMessages(in []*ChannelMessage) []*ChannelMessage {
	out := make([]*ChannelMessage, len(in))
	for index := range in {
		out[index] = CloneChannelMessage(in[index])
	}
	return out
}
