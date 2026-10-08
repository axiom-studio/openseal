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
// Runs, summary-first Activity projections, leased presence, action approvals
// and Skill setup requests are authoritative current projections when their
// digest changes. Consumers
// merge by stable identity and revision. Raw Activity payloads never travel on
// the reconnecting channel stream (except the {from, to} of an approval mode
// change); detail remains available from the bounded canonical Activity feed.
type ConversationChangeSet struct {
	Conversation *Conversation               `json:"conversation"`
	Messages     []*ChannelMessage           `json:"messages"`
	Rounds       []*ParticipationRoundResult `json:"rounds"`
	Runs         []*AgentRun                 `json:"runs"`
	Activity     []ActivityProjection        `json:"activity"`
	Presence     []*ConversationPresence     `json:"presence"`
	// Approvals are every pending approval for this conversation's Runs plus
	// the most recently decided ones, each with a resolved ConversationContext.
	Approvals []*ApprovalCheckpoint `json:"approvals"`
	// SkillSetupRequests are every pending setup request for this
	// conversation plus the most recently resolved or dismissed ones.
	SkillSetupRequests        []*ConversationSkillSetupRequest `json:"skillSetupRequests"`
	RunsChanged               bool                             `json:"runsChanged"`
	ActivityChanged           bool                             `json:"activityChanged"`
	PresenceChanged           bool                             `json:"presenceChanged"`
	ApprovalsChanged          bool                             `json:"approvalsChanged"`
	SkillSetupRequestsChanged bool                             `json:"skillSetupRequestsChanged"`
	Cursor                    string                           `json:"cursor"`
	HasChanges                bool                             `json:"hasChanges"`
	HasMore                   bool                             `json:"hasMore"`
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
	ApprovalDigest       string `json:"approvalDigest,omitempty"`
	SkillSetupDigest     string `json:"skillSetupDigest,omitempty"`
}

type ConversationChangeService struct {
	conversations *ConversationService
	portfolio     PortfolioStore
	activity      RunActivityStore
	approvals     conversationApprovalStore
	skillSetups   SkillSetupRequestReader
	now           func() time.Time
}

func NewConversationChangeService(conversationStore ConversationStore, portfolio PortfolioStore) (*ConversationChangeService, error) {
	if conversationStore == nil || portfolio == nil {
		return nil, errors.New("conversation and portfolio stores are required")
	}
	service := &ConversationChangeService{conversations: NewConversationService(conversationStore), portfolio: portfolio, now: time.Now}
	service.activity, _ = portfolio.(RunActivityStore)
	service.approvals, _ = portfolio.(conversationApprovalStore)
	if store, ok := portfolio.(SkillSetupRequestStore); ok {
		service.skillSetups = store.ListSkillSetupRequests
	}
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

	runs, err := s.listConversationRuns(ctx, conversation, req.Viewer)
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
	activity, err := s.listConversationActivity(ctx, conversation, runs)
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

	approvals, err := s.listConversationApprovals(ctx, conversation, runs)
	if err != nil {
		return nil, err
	}
	approvalDigest, err := conversationApprovalProjectionDigest(approvals)
	if err != nil {
		return nil, err
	}
	approvalsChanged := initial || approvalDigest != cursor.ApprovalDigest
	if !approvalsChanged {
		approvals = []*ApprovalCheckpoint{}
	}
	skillSetups, err := s.listConversationSkillSetupRequests(ctx, conversation, runs, req.Viewer)
	if err != nil {
		return nil, err
	}
	skillSetupDigest, err := conversationSkillSetupProjectionDigest(skillSetups)
	if err != nil {
		return nil, err
	}
	skillSetupsChanged := initial || skillSetupDigest != cursor.SkillSetupDigest
	if !skillSetupsChanged {
		skillSetups = []*ConversationSkillSetupRequest{}
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
		ApprovalDigest: approvalDigest, SkillSetupDigest: skillSetupDigest,
	}
	encodedCursor, err := encodeConversationChangeCursor(next)
	if err != nil {
		return nil, err
	}
	hasChanges := initial || conversation.Revision != cursor.ConversationRevision || len(messages) > 0 || len(rounds) > 0 ||
		runsChanged || activityChanged || presenceChanged || approvalsChanged || skillSetupsChanged
	return &ConversationChangeSet{
		Conversation: cloneConversation(conversation), Messages: cloneChannelMessages(messages), Rounds: cloneParticipationRoundResults(rounds),
		Runs: cloneAgentRuns(projectedRuns), Activity: cloneConversationActivity(projectedActivity), Presence: cloneConversationPresences(projectedPresence),
		Approvals: approvals, SkillSetupRequests: skillSetups,
		RunsChanged: runsChanged, ActivityChanged: activityChanged, PresenceChanged: presenceChanged,
		ApprovalsChanged: approvalsChanged, SkillSetupRequestsChanged: skillSetupsChanged,
		Cursor: encodedCursor, HasChanges: hasChanges, HasMore: messageHasMore || roundHasMore,
	}, nil
}

// A child inherits its originating message's visibility, not its assigned
// agent's identity. Filter before computing digests or projecting activity so
// hidden work cannot leak through either payloads or change notifications.
func (s *ConversationChangeService) filterVisibleRuns(ctx context.Context, scope Scope, conversationID string, runs []*AgentRun, viewer ConversationViewer) ([]*AgentRun, error) {
	visibleRoots := make(map[string]bool)
	var batchedMessages map[string]bool
	if batch, ok := s.conversations.store.(ChannelMessageBatchStore); ok {
		ids := make([]string, 0, len(runs))
		for _, run := range runs {
			if run.Kind == RunKindConversation {
				if triggerID, _ := run.Context[conversationRunContextTriggerID].(string); strings.TrimSpace(triggerID) != "" {
					ids = append(ids, triggerID)
				}
			}
		}
		messages, err := loadChannelMessageBatches(ctx, batch, scope, conversationID, ids)
		if err != nil {
			return nil, err
		}
		// Missing provenance still excludes a Run. Preserve the point-read
		// path's viewer validation only when a trigger message exists.
		batchedMessages = make(map[string]bool, len(messages))
		if len(messages) > 0 {
			selected := make([]*ChannelMessage, 0, len(messages))
			for _, id := range ids {
				if message := messages[id]; message != nil {
					selected = append(selected, message)
					delete(messages, id)
				}
			}
			visible, err := s.conversations.filterVisibleChannelMessages(ctx, scope, conversationID, selected, viewer)
			if err != nil {
				return nil, err
			}
			for _, message := range visible {
				batchedMessages[message.ID] = true
			}
		}
	}
	for _, run := range runs {
		if run.Kind != RunKindConversation {
			continue
		}
		triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
		if strings.TrimSpace(triggerID) == "" {
			continue // Unknown provenance is not evidence of viewer access.
		}
		if batchedMessages != nil {
			if batchedMessages[triggerID] {
				visibleRoots[run.ID] = true
			}
			continue
		}
		if _, err := s.conversations.GetVisibleChannelMessage(ctx, scope, conversationID, triggerID, viewer); err != nil {
			if errors.Is(err, ErrChannelMessageNotFound) {
				continue
			}
			return nil, err
		}
		visibleRoots[run.ID] = true
	}
	if tasks, ok := s.portfolio.(ConversationTaskStore); ok {
		for _, run := range runs {
			if run.Kind != RunKindAgentWork && run.Kind != RunKindConversation || run.ParentRunID != "" || run.RootRunID != run.ID {
				continue
			}
			task, err := tasks.FindConversationTaskByWorkRunID(ctx, scope, run.ID)
			if errors.Is(err, ErrConversationTaskNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if task == nil {
				continue
			}
			conversation, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if err != nil {
				return nil, err
			}
			if source, err := s.proveConversationTaskRun(ctx, conversation, task, run, &viewer); err != nil {
				return nil, err
			} else if source != nil {
				visibleRoots[run.ID] = true
			}
		}
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

func (s *ConversationChangeService) listConversationRuns(ctx context.Context, conversation *Conversation, viewers ...*ConversationViewer) ([]*AgentRun, error) {
	const pageSize = 500
	all := make([]*AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		var page []*AgentRun
		var err error
		if indexed, ok := s.portfolio.(ConversationForegroundRunsReadStore); ok {
			page, err = indexed.ListConversationForegroundRuns(ctx, conversation.Scope, conversation.Owner, conversation.ID, pageSize, offset)
		} else {
			page, err = s.portfolio.ListAgentRuns(ctx, AgentRunFilter{
				Scope: conversation.Scope, Kind: RunKindConversation, Owner: &conversation.Owner, Limit: pageSize, Offset: offset,
				ConcurrencyKey: conversation.ID,
			})
		}
		if err != nil {
			return nil, err
		}
		for _, run := range page {
			if run == nil {
				continue
			}
			if run.ConcurrencyKey == conversation.ID {
				all = append(all, run)
				continue
			}
			if run.Scope != conversation.Scope || run.Owner != conversation.Owner || run.Kind != RunKindConversation ||
				run.Context[conversationRunContextConversationID] != conversation.ID || s.conversations == nil {
				continue
			}
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			trigger, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, triggerID)
			if err != nil {
				if errors.Is(err, ErrChannelMessageNotFound) {
					continue
				}
				return nil, err
			}
			thread := externalConversationThreadRoot(conversation, trigger)
			if thread != "" && run.Context["threadRootMessageId"] == thread && run.ConcurrencyKey == conversation.ID+":thread:"+thread {
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
	var viewer *ConversationViewer
	if len(viewers) > 0 {
		viewer = viewers[0]
	}
	taskRuns, taskSources, err := s.listConversationTaskRuns(ctx, conversation, viewer)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*AgentRun, len(all)+len(taskRuns))
	for _, run := range all {
		byID[run.ID] = run
	}
	for _, run := range taskRuns {
		byID[run.ID] = run
	}
	// A continued conversation root has moved out of the foreground index.
	// Keep its original children visible through their exact durable root.
	taskRootIDs := make([]string, 0, len(taskRuns))
	for _, run := range taskRuns {
		taskRootIDs = append(taskRootIDs, run.ID)
	}
	for start := 0; start < len(taskRootIDs); start += pageSize {
		batch := taskRootIDs[start:min(start+pageSize, len(taskRootIDs))]
		for offset := 0; ; offset += pageSize {
			page, err := s.portfolio.ListAgentRuns(ctx, AgentRunFilter{Scope: conversation.Scope, Kind: RunKindAgentWork, RootRunIDs: batch, Limit: pageSize, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, child := range page {
				if source := taskSources[child.RootRunID]; source != nil && child.ID != child.RootRunID && child.Scope == conversation.Scope {
					byID[child.ID] = child
					taskSources[child.ID] = source
				}
			}
			if len(page) < pageSize {
				break
			}
		}
	}
	all = all[:0]
	for _, run := range byID {
		all = append(all, run)
	}
	if viewer != nil {
		all, err = s.filterVisibleRuns(ctx, conversation.Scope, conversation.ID, all, *viewer)
		if err != nil {
			return nil, err
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
		root := rootByID[run.RootRunID]
		if source := taskSources[run.ID]; source != nil {
			root = source
		}
		if root != nil && !included[root.ID] {
			result = append(result, root)
			included[root.ID] = true
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *ConversationChangeService) listConversationTaskRuns(ctx context.Context, conversation *Conversation, viewer *ConversationViewer) ([]*AgentRun, map[string]*AgentRun, error) {
	sources := make(map[string]*AgentRun)
	tasks, ok := s.portfolio.(ConversationTaskStore)
	if !ok || s.conversations == nil {
		return nil, sources, nil
	}
	filter := ConversationTaskFilter{Scope: conversation.Scope, Owner: conversation.Owner, ConversationID: conversation.ID, Limit: conversationRunProjectionLimit}
	if viewer != nil && viewer.Participant.Type == ConversationParticipantUser {
		filter.AuthenticatedActor = viewer.Participant
	}
	// A recent terminal window must not displace older active work. Both reads
	// use the exact conversation/actor index, and neither walks task history.
	activeFilter := filter
	activeFilter.ActiveOnly = true
	active, err := tasks.ListConversationTasks(ctx, activeFilter)
	if err != nil {
		return nil, nil, err
	}
	recent, err := tasks.ListConversationTasks(ctx, filter)
	if err != nil {
		return nil, nil, err
	}
	runs := make([]*AgentRun, 0, len(active)+len(recent))
	seen := make(map[string]bool, len(active)+len(recent))
	for _, result := range append(active, recent...) {
		if result == nil || result.Task == nil || result.WorkRun == nil || seen[result.WorkRun.ID] {
			continue
		}
		source, err := s.proveConversationTaskRun(ctx, conversation, result.Task, result.WorkRun, viewer)
		if err != nil {
			return nil, nil, err
		}
		if source == nil {
			continue
		}
		seen[result.WorkRun.ID] = true
		runs = append(runs, result.WorkRun)
		sources[result.WorkRun.ID] = source
	}
	return runs, sources, nil
}

type conversationChangeTaskProofStore struct {
	ConversationStore
	ConversationTaskStore
	PortfolioStore
}

// A task is its own run root. Its source, actor, thread, and message visibility
// must therefore be proven from persisted task identity rather than inferred
// from a copied conversation/task context on arbitrary work.
func (s *ConversationChangeService) proveConversationTaskRun(ctx context.Context, conversation *Conversation, task *ConversationTask, work *AgentRun, viewer *ConversationViewer) (*AgentRun, error) {
	if task == nil || work == nil || task.Validate() != nil || task.Scope != conversation.Scope || task.Owner != conversation.Owner ||
		task.ConversationID != conversation.ID || !ConversationTaskMatchesWorkRun(task, work) ||
		task.Mode != ConversationTaskModeContinuation && work.Context["threadRootMessageId"] != task.ThreadRootID ||
		viewer != nil && viewer.Participant.Type == ConversationParticipantUser && viewer.Participant != task.AuthenticatedActor {
		return nil, nil
	}
	tasks, ok := s.portfolio.(ConversationTaskStore)
	if !ok {
		return nil, nil
	}
	proofStore := conversationChangeTaskProofStore{ConversationStore: s.conversations.store, ConversationTaskStore: tasks, PortfolioStore: s.portfolio}
	workTask, sourceReport, err := conversationTaskForReport(ctx, proofStore, work)
	if errors.Is(err, ErrInvalidConversationTask) || errors.Is(err, ErrConversationTaskNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if workTask == nil || sourceReport || !sameConversationTask(task, workTask) {
		return nil, nil
	}
	source, err := s.portfolio.GetAgentRun(ctx, task.Scope, task.SourceRunID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if source == nil || source.ParentRunID != "" || source.RootRunID != source.ID {
		return nil, nil
	}
	if task.Mode == ConversationTaskModeContinuation {
		if !ConversationTaskMatchesWorkRun(task, source) {
			return nil, nil
		}
	} else {
		if source.Status != AgentRunStatusCompleted || source.LastAppliedTurn != task.SourceTurnNumber || validateConversationRun(source) != nil {
			return nil, nil
		}
		sourceTask, sourceReport, err := conversationTaskForReport(ctx, proofStore, source)
		if errors.Is(err, ErrInvalidConversationTask) || errors.Is(err, ErrConversationTaskNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if sourceTask == nil || !sourceReport || !sameConversationTask(task, sourceTask) {
			return nil, nil
		}
	}
	var message *ChannelMessage
	if viewer != nil {
		message, err = s.conversations.GetVisibleChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID, *viewer)
	} else {
		message, err = s.conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	}
	if errors.Is(err, ErrChannelMessageNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if message == nil || message.ID != task.SourceMessageID || message.Scope != task.Scope || message.ConversationID != task.ConversationID {
		return nil, nil
	}
	sourceThread, _ := source.Context["threadRootMessageId"].(string)
	if sourceThread != externalConversationThreadRoot(conversation, message) {
		return nil, nil
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	thread := message.ThreadRootID
	if thread == "" {
		thread = message.ID
	}
	if actor != task.AuthenticatedActor || thread != task.ThreadRootID {
		return nil, nil
	}
	return source, nil
}

func (s *ConversationChangeService) listConversationActivity(ctx context.Context, conversation *Conversation, runs []*AgentRun) ([]ActivityProjection, error) {
	if s.activity == nil || conversation == nil {
		return []ActivityProjection{}, nil
	}
	// The conversation's own activity stream (for example approval mode
	// changes) is listed together with its Runs' activity.
	runIDs := make([]string, 0, len(runs)+1)
	runIDs = append(runIDs, activityStreamID(&ActivityEvent{ConversationRefs: []string{conversation.ID}}))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	byID := make(map[string]*ActivityEvent)
	for start := 0; start < len(runIDs); start += conversationActivityRunBatchSize {
		end := min(start+conversationActivityRunBatchSize, len(runIDs))
		events, err := s.activity.ListActivity(ctx, ActivityFilter{
			Scope: conversation.Scope, RunIDs: runIDs[start:end], Descending: true, Limit: conversationRunProjectionLimit,
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
		projection := projectActivityEvent(event, false)
		// An approval mode change carries only the two mode names, which the
		// conversation view renders; other payloads stay on the Activity feed.
		if event.EventType == ConversationApprovalModeChangedEvent {
			projection.Payload = map[string]interface{}{"from": event.Payload["from"], "to": event.Payload["to"]}
		}
		projected = append(projected, projection)
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
