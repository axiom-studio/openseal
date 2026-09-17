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
	conversationChangeCursorVersion  = 1
	defaultConversationChangeLimit   = 100
	maximumConversationChangeLimit   = 500
	conversationRunProjectionLimit   = 100
	conversationActivityRunBatchSize = 200
)

type ConversationChangeRequest struct {
	Scope          Scope
	ConversationID string
	Cursor         string
	Limit          int
	ActiveAt       time.Time
	Viewer         *ConversationViewer
}

// ConversationChangeSet is a portable, transport-neutral projection for
// reconnect-safe channel observation. Messages and rounds are durable deltas;
// Runs, summary-first Activity projections, and leased presence are
// authoritative current projections when their digest changes. Consumers
// merge by stable identity and revision. Raw Activity payloads never travel on
// the reconnecting channel stream; detail remains available from the bounded
// canonical Activity feed.
type ConversationChangeSet struct {
	Conversation    *Conversation               `json:"conversation"`
	Messages        []*ChannelMessage           `json:"messages"`
	Rounds          []*ParticipationRoundResult `json:"rounds"`
	Runs            []*AgentRun                 `json:"runs"`
	Activity        []ActivityProjection        `json:"activity"`
	Presence        []*ConversationPresence     `json:"presence"`
	RunsChanged     bool                        `json:"runsChanged"`
	ActivityChanged bool                        `json:"activityChanged"`
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
	ActivityDigest       string `json:"activityDigest,omitempty"`
	PresenceDigest       string `json:"presenceDigest,omitempty"`
}

type ConversationChangeService struct {
	conversations *ConversationService
	portfolio     PortfolioStore
	activity      RunActivityStore
	now           func() time.Time
}

func NewConversationChangeService(conversationStore ConversationStore, portfolio PortfolioStore) (*ConversationChangeService, error) {
	if conversationStore == nil || portfolio == nil {
		return nil, errors.New("conversation and portfolio stores are required")
	}
	service := &ConversationChangeService{conversations: NewConversationService(conversationStore), portfolio: portfolio, now: time.Now}
	service.activity, _ = portfolio.(RunActivityStore)
	return service, nil
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

	rawMessages, err := s.conversations.store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: req.Scope, ConversationID: conversationID, AfterSequence: cursor.MessageSequence, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	messages := rawMessages
	if req.Viewer != nil {
		messages, err = s.conversations.filterVisibleChannelMessages(ctx, req.Scope, conversationID, rawMessages, *req.Viewer)
		if err != nil {
			return nil, err
		}
	}
	nextMessageSequence := cursor.MessageSequence
	if len(rawMessages) > 0 {
		nextMessageSequence = rawMessages[len(rawMessages)-1].Sequence
	}
	messageHasMore := len(rawMessages) == limit && nextMessageSequence < conversation.LastSequence

	rounds, roundHasMore, nextRoundRevision, err := s.listRoundsAfter(ctx, req.Scope, conversationID, cursor.RoundRevision, limit)
	if err != nil {
		return nil, err
	}
	if req.Viewer != nil {
		rounds, err = s.filterVisibleRounds(ctx, req.Scope, conversationID, rounds, *req.Viewer)
		if err != nil {
			return nil, err
		}
	}

	runs, err := s.listConversationRuns(ctx, conversation)
	if err != nil {
		return nil, err
	}
	if req.Viewer != nil {
		runs, err = s.filterVisibleRuns(ctx, req.Scope, conversationID, runs, *req.Viewer)
		if err != nil {
			return nil, err
		}
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
	activity, err := s.listConversationActivity(ctx, runs)
	if err != nil {
		return nil, err
	}
	activityDigest, err := conversationActivityProjectionDigest(activity)
	if err != nil {
		return nil, err
	}
	activityChanged := initial || activityDigest != cursor.ActivityDigest
	projectedActivity := activity
	if !activityChanged {
		projectedActivity = []ActivityProjection{}
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
		RoundRevision: nextRoundRevision, RunDigest: runDigest, ActivityDigest: activityDigest, PresenceDigest: presenceDigest,
	}
	encodedCursor, err := encodeConversationChangeCursor(next)
	if err != nil {
		return nil, err
	}
	hasChanges := initial || conversation.Revision != cursor.ConversationRevision || len(messages) > 0 || len(rounds) > 0 ||
		runsChanged || activityChanged || presenceChanged
	return &ConversationChangeSet{
		Conversation: cloneConversation(conversation), Messages: cloneChannelMessages(messages), Rounds: cloneParticipationRoundResults(rounds),
		Runs: cloneAgentRuns(projectedRuns), Activity: cloneConversationActivity(projectedActivity), Presence: cloneConversationPresences(projectedPresence),
		RunsChanged: runsChanged, ActivityChanged: activityChanged, PresenceChanged: presenceChanged,
		Cursor: encodedCursor, HasChanges: hasChanges, HasMore: messageHasMore || roundHasMore,
	}, nil
}

// A child inherits its originating message's visibility, not its assigned
// agent's identity. Filter before computing digests or projecting activity so
// hidden work cannot leak through either payloads or change notifications.
func (s *ConversationChangeService) filterVisibleRuns(ctx context.Context, scope Scope, conversationID string, runs []*AgentRun, viewer ConversationViewer) ([]*AgentRun, error) {
	visibleRoots := make(map[string]bool)
	for _, run := range runs {
		if run.Kind != RunKindConversation {
			continue
		}
		triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
		if strings.TrimSpace(triggerID) == "" {
			continue // Unknown provenance is not evidence of viewer access.
		}
		if _, err := s.conversations.GetVisibleChannelMessage(ctx, scope, conversationID, triggerID, viewer); err != nil {
			if errors.Is(err, ErrChannelMessageNotFound) {
				continue
			}
			return nil, err
		}
		visibleRoots[run.ID] = true
	}
	visible := make([]*AgentRun, 0, len(runs))
	for _, run := range runs {
		if visibleRoots[run.ID] || visibleRoots[run.RootRunID] {
			visible = append(visible, run)
		}
	}
	return visible, nil
}

func (s *ConversationChangeService) filterVisibleRounds(ctx context.Context, scope Scope, conversationID string, rounds []*ParticipationRoundResult, viewer ConversationViewer) ([]*ParticipationRoundResult, error) {
	visible := make([]*ParticipationRoundResult, 0, len(rounds))
	for _, result := range rounds {
		if result == nil || result.Round == nil {
			continue
		}
		if triggerID := strings.TrimSpace(result.Round.TriggerMessageID); triggerID != "" {
			if _, err := s.conversations.GetVisibleChannelMessage(ctx, scope, conversationID, triggerID, viewer); err != nil {
				if errors.Is(err, ErrChannelMessageNotFound) {
					continue
				}
				return nil, err
			}
		}
		messages, err := s.conversations.filterVisibleChannelMessages(ctx, scope, conversationID, result.Messages, viewer)
		if err != nil {
			return nil, err
		}
		projected := cloneParticipationRoundResult(result, result.Replayed)
		projected.Messages = messages
		visible = append(visible, projected)
	}
	return visible, nil
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
	// Delegated work may outlive the reply that launched it. Batch exact
	// durable root within this scope; do not infer membership from copied goals
	// or require children to share the initiating agent's owner identity.
	roots := append([]*AgentRun(nil), all...)
	rootByID := make(map[string]*AgentRun, len(roots))
	rootIDs := make([]string, 0, len(roots))
	for _, root := range roots {
		rootByID[root.ID] = root
		rootIDs = append(rootIDs, root.ID)
	}
	for start := 0; start < len(rootIDs); start += pageSize {
		batch := rootIDs[start:min(start+pageSize, len(rootIDs))]
		for offset := 0; ; offset += pageSize {
			page, err := s.portfolio.ListAgentRuns(ctx, AgentRunFilter{
				Scope: conversation.Scope, Kind: RunKindAgentWork, RootRunIDs: batch, Limit: pageSize, Offset: offset,
			})
			if err != nil {
				return nil, err
			}
			for _, child := range page {
				if rootByID[child.RootRunID] != nil && child.ID != child.RootRunID && child.Scope == conversation.Scope {
					all = append(all, child)
				}
			}
			if len(page) < pageSize {
				break
			}
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
	// Keep the originating message linkage when a recent/active child survives
	// the terminal-history window but its completed root does not.
	included := make(map[string]bool, len(result))
	for _, run := range result {
		included[run.ID] = true
	}
	for _, run := range append([]*AgentRun(nil), result...) {
		if root := rootByID[run.RootRunID]; root != nil && !included[root.ID] {
			result = append(result, root)
			included[root.ID] = true
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *ConversationChangeService) listConversationActivity(ctx context.Context, runs []*AgentRun) ([]ActivityProjection, error) {
	if s.activity == nil {
		return []ActivityProjection{}, nil
	}
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	if len(runIDs) == 0 {
		return []ActivityProjection{}, nil
	}
	byID := make(map[string]*ActivityEvent)
	for start := 0; start < len(runIDs); start += conversationActivityRunBatchSize {
		end := min(start+conversationActivityRunBatchSize, len(runIDs))
		events, err := s.activity.ListActivity(ctx, ActivityFilter{
			Scope: runs[0].Scope, RunIDs: runIDs[start:end], Descending: true, Limit: conversationRunProjectionLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			byID[event.ID] = event
		}
	}
	result := make([]*ActivityEvent, 0, len(byID))
	for _, event := range byID {
		result = append(result, event)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		if result[i].Sequence != result[j].Sequence {
			return result[i].Sequence > result[j].Sequence
		}
		return result[i].ID > result[j].ID
	})
	if len(result) > conversationRunProjectionLimit {
		result = result[:conversationRunProjectionLimit]
	}
	projected := make([]ActivityProjection, 0, len(result))
	for _, event := range result {
		projected = append(projected, projectActivityEvent(event, false))
	}
	return projected, nil
}

func conversationActivityProjectionDigest(events []ActivityProjection) (string, error) {
	values := make([]struct {
		ID       string `json:"id"`
		Sequence int64  `json:"sequence"`
	}, 0, len(events))
	for _, event := range events {
		values = append(values, struct {
			ID       string `json:"id"`
			Sequence int64  `json:"sequence"`
		}{ID: event.ID, Sequence: event.Sequence})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func cloneConversationActivity(events []ActivityProjection) []ActivityProjection {
	result := make([]ActivityProjection, len(events))
	copy(result, events)
	return result
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
