package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	conversationChangeCursorVersion = 1
	defaultConversationChangeLimit  = 100
	maximumConversationChangeLimit  = 500
	conversationRunProjectionLimit  = 100
)

type ConversationChangeRequest struct {
	Scope          Scope
	ConversationID string
	Cursor         string
	Limit          int
	ActiveAt       time.Time
}

// ConversationChangeSet is a portable, transport-neutral projection for
// reconnect-safe channel observation. Messages and rounds are durable deltas;
// Runs and leased presence are authoritative current projections when their
// digest changes. Consumers merge by stable identity and revision.
type ConversationChangeSet struct {
	Conversation    *Conversation               `json:"conversation"`
	Messages        []*ChannelMessage           `json:"messages"`
	Rounds          []*ParticipationRoundResult `json:"rounds"`
	Runs            []*AgentRun                 `json:"runs"`
	Presence        []*ConversationPresence     `json:"presence"`
	RunsChanged     bool                        `json:"runsChanged"`
	PresenceChanged bool                        `json:"presenceChanged"`
	Cursor          string                      `json:"cursor"`
	HasChanges      bool                        `json:"hasChanges"`
	HasMore         bool                        `json:"hasMore"`
}

type conversationChangeCursor struct {
	Version              int    `json:"version"`
	ConversationID       string `json:"conversationId"`
	ConversationRevision int64  `json:"conversationRevision"`
	MessageSequence      int64  `json:"messageSequence"`
	RoundRevision        int64  `json:"roundRevision"`
	RunDigest            string `json:"runDigest,omitempty"`
	PresenceDigest       string `json:"presenceDigest,omitempty"`
}

type ConversationChangeService struct {
	conversations *ConversationService
	portfolio     PortfolioStore
	now           func() time.Time
}

func NewConversationChangeService(conversationStore ConversationStore, portfolio PortfolioStore) (*ConversationChangeService, error) {
	if conversationStore == nil || portfolio == nil {
		return nil, errors.New("conversation and portfolio stores are required")
	}
	return &ConversationChangeService{
		conversations: NewConversationService(conversationStore), portfolio: portfolio, now: time.Now,
	}, nil
}

func (s *ConversationChangeService) ListChanges(ctx context.Context, req ConversationChangeRequest) (*ConversationChangeSet, error) {
	if s == nil || s.conversations == nil || s.portfolio == nil {
		return nil, errors.New("conversation change service is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	conversationID := strings.TrimSpace(req.ConversationID)
	if !validOpaqueIdentifier(conversationID, 128) {
		return nil, fmt.Errorf("%w: conversation id is required", ErrInvalidConversation)
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultConversationChangeLimit
	}
	if limit < 1 || limit > maximumConversationChangeLimit {
		return nil, fmt.Errorf("%w: conversation change limit must be between 1 and %d", ErrInvalidConversation, maximumConversationChangeLimit)
	}
	cursor, initial, err := decodeConversationChangeCursor(req.Cursor, conversationID)
	if err != nil {
		return nil, err
	}
	conversation, err := s.conversations.GetConversation(ctx, req.Scope, conversationID)
	if err != nil {
		return nil, err
	}

	messages, err := s.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: req.Scope, ConversationID: conversationID, AfterSequence: cursor.MessageSequence, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	nextMessageSequence := cursor.MessageSequence
	if len(messages) > 0 {
		nextMessageSequence = messages[len(messages)-1].Sequence
	}
	messageHasMore := len(messages) == limit && nextMessageSequence < conversation.LastSequence

	rounds, roundHasMore, nextRoundRevision, err := s.listRoundsAfter(ctx, req.Scope, conversationID, cursor.RoundRevision, limit)
	if err != nil {
		return nil, err
	}

	runs, err := s.listConversationRuns(ctx, conversation)
	if err != nil {
		return nil, err
	}
	runDigest, err := conversationRunProjectionDigest(runs)
	if err != nil {
		return nil, err
	}
	runsChanged := initial || runDigest != cursor.RunDigest
	projectedRuns := runs
	if !runsChanged {
		projectedRuns = []*AgentRun{}
	}

	activeAt := req.ActiveAt
	if activeAt.IsZero() {
		activeAt = s.now().UTC()
	}
	presence, err := s.conversations.store.ListConversationPresence(ctx, req.Scope, conversationID, activeAt)
	if err != nil {
		return nil, err
	}
	presenceDigest, err := conversationPresenceProjectionDigest(presence)
	if err != nil {
		return nil, err
	}
	presenceChanged := initial || presenceDigest != cursor.PresenceDigest
	projectedPresence := presence
	if !presenceChanged {
		projectedPresence = []*ConversationPresence{}
	}

	next := conversationChangeCursor{
		Version: conversationChangeCursorVersion, ConversationID: conversationID,
		ConversationRevision: conversation.Revision, MessageSequence: nextMessageSequence,
		RoundRevision: nextRoundRevision, RunDigest: runDigest, PresenceDigest: presenceDigest,
	}
	encodedCursor, err := encodeConversationChangeCursor(next)
	if err != nil {
		return nil, err
	}
	hasChanges := initial || conversation.Revision != cursor.ConversationRevision || len(messages) > 0 || len(rounds) > 0 ||
		runsChanged || presenceChanged
	return &ConversationChangeSet{
		Conversation: cloneConversation(conversation), Messages: cloneChannelMessages(messages), Rounds: cloneParticipationRoundResults(rounds),
		Runs: cloneAgentRuns(projectedRuns), Presence: cloneConversationPresences(projectedPresence),
		RunsChanged: runsChanged, PresenceChanged: presenceChanged,
		Cursor: encodedCursor, HasChanges: hasChanges, HasMore: messageHasMore || roundHasMore,
	}, nil
}

func (s *ConversationChangeService) listRoundsAfter(
	ctx context.Context,
	scope Scope,
	conversationID string,
	afterRevision int64,
	limit int,
) ([]*ParticipationRoundResult, bool, int64, error) {
	const pageSize = 500
	all := make([]*ParticipationRoundResult, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.conversations.ListParticipationRounds(ctx, ParticipationRoundFilter{
			Scope: scope, ConversationID: conversationID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, false, afterRevision, err
		}
		for _, result := range page {
			if result != nil && result.Round != nil && result.Round.ConversationRevision > afterRevision {
				all = append(all, result)
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Round.ConversationRevision != all[j].Round.ConversationRevision {
			return all[i].Round.ConversationRevision < all[j].Round.ConversationRevision
		}
		return all[i].Round.ID < all[j].Round.ID
	})
	hasMore := len(all) > limit
	if hasMore {
		all = all[:limit]
	}
	nextRevision := afterRevision
	if len(all) > 0 {
		nextRevision = all[len(all)-1].Round.ConversationRevision
	}
	return all, hasMore, nextRevision, nil
}

func (s *ConversationChangeService) listConversationRuns(ctx context.Context, conversation *Conversation) ([]*AgentRun, error) {
	const pageSize = 500
	all := make([]*AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.portfolio.ListAgentRuns(ctx, AgentRunFilter{
			Scope: conversation.Scope, Kind: RunKindConversation, Owner: &conversation.Owner, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range page {
			if run.ConcurrencyKey == conversation.ID {
				all = append(all, run)
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].UpdatedAt.Equal(all[j].UpdatedAt) {
			return all[i].UpdatedAt.After(all[j].UpdatedAt)
		}
		return all[i].ID > all[j].ID
	})
	active := make([]*AgentRun, 0, len(all))
	terminal := make([]*AgentRun, 0, min(len(all), conversationRunProjectionLimit))
	for _, run := range all {
		if isTerminalAgentRunStatus(run.Status) {
			if len(terminal) < conversationRunProjectionLimit {
				terminal = append(terminal, run)
			}
			continue
		}
		active = append(active, run)
	}
	result := append(active, terminal...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func conversationRunProjectionDigest(runs []*AgentRun) (string, error) {
	values := make([]struct {
		ID       string         `json:"id"`
		Revision int64          `json:"revision"`
		Status   AgentRunStatus `json:"status"`
	}, 0, len(runs))
	for _, run := range runs {
		values = append(values, struct {
			ID       string         `json:"id"`
			Revision int64          `json:"revision"`
			Status   AgentRunStatus `json:"status"`
		}{ID: run.ID, Revision: run.Revision, Status: run.Status})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func conversationPresenceProjectionDigest(presence []*ConversationPresence) (string, error) {
	values := cloneConversationPresences(presence)
	sort.Slice(values, func(i, j int) bool {
		left := string(values[i].Participant.Type) + ":" + values[i].Participant.ID
		right := string(values[j].Participant.Type) + ":" + values[j].Participant.ID
		return left < right
	})
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func encodeConversationChangeCursor(cursor conversationChangeCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeConversationChangeCursor(value, conversationID string) (conversationChangeCursor, bool, error) {
	if strings.TrimSpace(value) == "" {
		return conversationChangeCursor{Version: conversationChangeCursorVersion, ConversationID: conversationID}, true, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return conversationChangeCursor{}, false, fmt.Errorf("%w: invalid conversation change cursor", ErrInvalidConversation)
	}
	var cursor conversationChangeCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Version != conversationChangeCursorVersion ||
		cursor.ConversationID != conversationID || cursor.ConversationRevision < 0 || cursor.MessageSequence < 0 || cursor.RoundRevision < 0 {
		return conversationChangeCursor{}, false, fmt.Errorf("%w: invalid conversation change cursor", ErrInvalidConversation)
	}
	return cursor, false, nil
}

func cloneParticipationRoundResults(in []*ParticipationRoundResult) []*ParticipationRoundResult {
	out := make([]*ParticipationRoundResult, 0, len(in))
	for _, result := range in {
		out = append(out, cloneParticipationRoundResult(result, result != nil && result.Replayed))
	}
	return out
}

func cloneAgentRuns(in []*AgentRun) []*AgentRun {
	out := make([]*AgentRun, 0, len(in))
	for _, run := range in {
		out = append(out, cloneAgentRun(run))
	}
	return out
}

func cloneConversationPresences(in []*ConversationPresence) []*ConversationPresence {
	out := make([]*ConversationPresence, 0, len(in))
	for _, presence := range in {
		out = append(out, cloneConversationPresence(presence))
	}
	return out
}
