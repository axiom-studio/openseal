package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Memory backend of AgentSessionStore (run-loop SPEC §2.1, section 01 §2.3).
// Every method runs under s.mu, including the whole path build (SPEC §4.7).
// The …Locked helpers are the in-transaction variants used by stores that
// create a child Run, a dependency group, an AgentRequest or a ConversationTask,
// or promote a task, while they already hold s.mu.

var _ AgentSessionStore = (*MemoryStore)(nil)

type memorySessionEntryRef struct {
	sessionKey string
	index      int
}

func memorySessionSourceKey(scope Scope, sessionID, sourceKey string) string {
	return portfolioKey(scope, sessionID+"\x00"+sourceKey)
}

func memorySessionNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// memoryRunLeaseLocked is the lease check of UpdateAgentRunWithEvent.
func (s *MemoryStore) memoryRunLeaseLocked(scope Scope, runID string, lease AgentRunLeaseGuard) (*AgentRun, error) {
	run := s.agentRuns[portfolioKey(scope, runID)]
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.LeaseOwner != lease.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(lease.Now) {
		return nil, ErrLeaseLost
	}
	return run, nil
}

func validateSessionEvents(scope Scope, runID string, events []*ActivityEvent, kind error) error {
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("%w: %v", kind, err)
		}
		if event.Scope != scope || event.RunID != runID {
			return fmt.Errorf("%w: activity event belongs to another run", kind)
		}
	}
	return nil
}

func (s *MemoryStore) appendSessionEventsLocked(events []*ActivityEvent, now time.Time) []*ActivityEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]*ActivityEvent, 0, len(events))
	for _, event := range events {
		event = cloneActivityEvent(event)
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		out = append(out, cloneActivityEvent(appendMemoryActivityLocked(s, event)))
	}
	return out
}

// insertMemoryAgentSessionLocked stores a new, validated session row and its
// unique-index entries (lane for conversation sessions, owner for run sessions).
func (s *MemoryStore) insertMemoryAgentSessionLocked(session *AgentSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	key := portfolioKey(session.Scope, session.ID)
	if s.agentSessions[key] != nil {
		return ErrAgentSessionConflict
	}
	switch session.Kind {
	case AgentSessionKindConversation:
		laneKey := portfolioKey(session.Scope, session.LaneKey)
		if id, ok := s.agentSessionLanes[laneKey]; ok && id != session.ID {
			return ErrAgentSessionConflict
		}
		s.agentSessionLanes[laneKey] = session.ID
	case AgentSessionKindRun:
		ownerKey := portfolioKey(session.Scope, session.OwnerRunID)
		if id, ok := s.agentSessionOwners[ownerKey]; ok && id != session.ID {
			return ErrAgentSessionConflict
		}
		s.agentSessionOwners[ownerKey] = session.ID
	}
	s.agentSessions[key] = cloneAgentSession(session)
	return nil
}

// ---------------------------------------------------------------------------
// Open / create

func (s *MemoryStore) OpenConversationAgentSession(_ context.Context, req OpenConversationAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	if strings.TrimSpace(req.ConversationID) == "" || strings.TrimSpace(req.AgentID) == "" ||
		strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.Lease.WorkerID) == "" {
		return nil, fmt.Errorf("%w: conversationId, agentId, runId and a lease are required", ErrInvalidAgentSession)
	}
	now := memorySessionNow(req.Now)
	lane := conversationRunLane(req.ConversationID, req.ThreadRootID, req.AgentID)
	id := ConversationAgentSessionID(req.Scope, lane)

	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.memoryRunLeaseLocked(req.Scope, req.RunID, req.Lease)
	if err != nil {
		return nil, err
	}
	if run.SessionID != id {
		return nil, ErrSessionMoved
	}
	if current := s.agentSessions[portfolioKey(req.Scope, id)]; current != nil {
		if current.Kind != AgentSessionKindConversation || current.LaneKey != lane || current.AgentID != req.AgentID {
			return nil, ErrAgentSessionConflict
		}
		return &OpenAgentSessionResult{Session: cloneAgentSession(current)}, nil
	}
	session := &AgentSession{
		ID:             id,
		Scope:          req.Scope,
		Kind:           AgentSessionKindConversation,
		LaneKey:        lane,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		AgentID:        req.AgentID,
		RootSessionID:  id,
		NextSequence:   1,
		Revision:       1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.insertMemoryAgentSessionLocked(session); err != nil {
		return nil, err
	}
	return &OpenAgentSessionResult{Session: cloneAgentSession(session), Created: true}, nil
}

// CreateRunAgentSession creates the parentless run session of OwnerRunID at its
// first claim, fenced by that Run's lease. The Run must be unbound or already
// bound to this session; a Run bound elsewhere gets ErrSessionMoved.
func (s *MemoryStore) CreateRunAgentSession(_ context.Context, req CreateRunAgentSessionRequest) (*OpenAgentSessionResult, error) {
	txReq := CreateRunAgentSessionTxRequest{
		Scope:          req.Scope,
		OwnerRunID:     req.OwnerRunID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		InitialEntries: req.InitialEntries,
		Now:            memorySessionNow(req.Now),
	}
	if strings.TrimSpace(req.Lease.WorkerID) == "" {
		return nil, fmt.Errorf("%w: a lease is required", ErrInvalidAgentSession)
	}
	if err := validateCreateRunAgentSessionTxRequest(txReq); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.memoryRunLeaseLocked(req.Scope, req.OwnerRunID, req.Lease)
	if err != nil {
		return nil, err
	}
	if id := RunAgentSessionID(req.Scope, req.OwnerRunID); run.SessionID != "" && run.SessionID != id {
		return nil, ErrSessionMoved
	}
	return s.createRunAgentSessionCoreLocked(txReq)
}

func validateCreateRunAgentSessionTxRequest(req CreateRunAgentSessionTxRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	if strings.TrimSpace(req.OwnerRunID) == "" || strings.TrimSpace(req.AgentID) == "" {
		return fmt.Errorf("%w: ownerRunId and agentId are required", ErrInvalidAgentSession)
	}
	if req.ForkSequence < 0 || (req.ParentSessionID == "" && req.ForkSequence != 0) {
		return fmt.Errorf("%w: invalid fork sequence", ErrInvalidAgentSession)
	}
	if len(req.InitialEntries) == 0 {
		return nil
	}
	return validateAppendAgentSessionEntries(initialEntriesAppend(req, RunAgentSessionID(req.Scope, req.OwnerRunID)))
}

// initialEntriesAppend is the append that seeds a new run session. The fence
// is the caller's (the creator's lease or its transaction), so the request
// carries a placeholder lease only to pass the shared validation.
func initialEntriesAppend(req CreateRunAgentSessionTxRequest, sessionID string) AppendAgentSessionEntriesRequest {
	return AppendAgentSessionEntriesRequest{
		Scope:     req.Scope,
		SessionID: sessionID,
		RunID:     req.OwnerRunID,
		Lease:     &AgentRunLeaseGuard{WorkerID: "agent-session-create", Now: req.Now},
		Entries:   req.InitialEntries,
		Now:       req.Now,
	}
}

// createRunAgentSessionLocked is the in-transaction create of a run session
// (child Run creation, dependency groups, AgentRequests, ConversationTasks,
// promotion). The caller holds s.mu and owns the fence. The owner Run must
// already be stored; its SessionID is set to the new session in the same
// critical section (Revision+1 when it changes). A replay with the same parent
// and Agent returns the existing session with Created=false and writes nothing.
func (s *MemoryStore) createRunAgentSessionLocked(req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
	req.Now = memorySessionNow(req.Now)
	if err := validateCreateRunAgentSessionTxRequest(req); err != nil {
		return nil, err
	}
	return s.createRunAgentSessionCoreLocked(req)
}

func (s *MemoryStore) createRunAgentSessionCoreLocked(req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
	id := RunAgentSessionID(req.Scope, req.OwnerRunID)
	runKey := portfolioKey(req.Scope, req.OwnerRunID)
	run := s.agentRuns[runKey]
	if run == nil {
		return nil, ErrRunNotFound
	}
	if current := s.agentSessions[portfolioKey(req.Scope, id)]; current != nil {
		if current.Kind != AgentSessionKindRun || current.OwnerRunID != req.OwnerRunID ||
			current.ParentSessionID != req.ParentSessionID || current.AgentID != req.AgentID {
			return nil, ErrAgentSessionConflict
		}
		return &OpenAgentSessionResult{Session: cloneAgentSession(current)}, nil
	}
	session := &AgentSession{
		ID:             id,
		Scope:          req.Scope,
		Kind:           AgentSessionKindRun,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		AgentID:        req.AgentID,
		OwnerRunID:     req.OwnerRunID,
		RootSessionID:  id,
		NextSequence:   1,
		Revision:       1,
		CreatedAt:      req.Now,
		UpdatedAt:      req.Now,
	}
	if req.ParentSessionID != "" {
		parent := s.agentSessions[portfolioKey(req.Scope, req.ParentSessionID)]
		if parent == nil {
			return nil, ErrAgentSessionNotFound
		}
		if req.ForkSequence > parent.NextSequence-1 {
			return nil, fmt.Errorf("%w: fork sequence %d is past the parent leaf %d", ErrInvalidAgentSession, req.ForkSequence, parent.NextSequence-1)
		}
		if parent.Depth+1 > MaxAgentSessionDepth {
			return nil, ErrAgentSessionForkTooDeep
		}
		session.ParentSessionID = parent.ID
		session.ForkSequence = req.ForkSequence
		if req.ForkSequence > 0 {
			session.ForkEntryID = s.agentSessionEntries[portfolioKey(req.Scope, parent.ID)][req.ForkSequence-1].ID
		}
		session.RootSessionID = parent.RootSessionID
		session.Depth = parent.Depth + 1
		session.ChannelWatermark = parent.ChannelWatermark
		if session.ConversationID == "" {
			session.ConversationID = parent.ConversationID
		}
		if session.ThreadRootID == "" {
			session.ThreadRootID = parent.ThreadRootID
		}
		session.ModelProvider, session.Model = s.memoryForkModelLocked(parent, req.ForkSequence)
	}
	if err := session.Validate(); err != nil {
		return nil, err
	}
	ownerKey := portfolioKey(req.Scope, req.OwnerRunID)
	if other, ok := s.agentSessionOwners[ownerKey]; ok && other != id {
		return nil, ErrAgentSessionConflict
	}

	var plan *memoryAppendPlan
	if len(req.InitialEntries) > 0 {
		appendReq := initialEntriesAppend(req, id)
		var err error
		plan, err = s.planMemoryAppendLocked(appendReq, session, nil)
		if err != nil {
			return nil, err
		}
	}

	if err := s.insertMemoryAgentSessionLocked(session); err != nil {
		return nil, err
	}
	stored := session
	if plan != nil {
		stored = s.commitMemoryAppendLocked(plan)
	}
	if run.SessionID != id {
		next := cloneAgentRun(run)
		next.SessionID = id
		next.Revision++
		next.UpdatedAt = req.Now
		s.saveMemoryAgentRunLocked(runKey, next)
	}
	return &OpenAgentSessionResult{Session: cloneAgentSession(stored), Created: true}, nil
}

// memoryForkModelLocked is the model of the latest model_change entry on the
// parent's chain up to the fork point, or the parent's own model when the
// chain has none in bounds.
func (s *MemoryStore) memoryForkModelLocked(parent *AgentSession, forkSequence int64) (string, string) {
	session, bound := parent, forkSequence
	for depth := 0; session != nil && depth <= MaxAgentSessionDepth; depth++ {
		entries := s.agentSessionEntries[portfolioKey(session.Scope, session.ID)]
		for i := int(bound) - 1; i >= 0; i-- {
			if change := entries[i].ModelChange; entries[i].Type == SessionEntryModelChange && change != nil {
				return change.Provider, change.Model
			}
		}
		if session.ParentSessionID == "" {
			break
		}
		bound = session.ForkSequence
		session = s.agentSessions[portfolioKey(session.Scope, session.ParentSessionID)]
	}
	return parent.ModelProvider, parent.Model
}

// ---------------------------------------------------------------------------
// Reads

func (s *MemoryStore) GetAgentSession(_ context.Context, scope Scope, sessionID string) (*AgentSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	session := s.agentSessions[portfolioKey(scope, sessionID)]
	if session == nil {
		return nil, ErrAgentSessionNotFound
	}
	return cloneAgentSession(session), nil
}

func (s *MemoryStore) GetSessionEntry(_ context.Context, scope Scope, entryID string) (*SessionEntry, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSessionEntry, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.memorySessionEntryLocked(scope, entryID)
	if entry == nil {
		return nil, ErrSessionEntryNotFound
	}
	return cloneSessionEntry(entry), nil
}

func (s *MemoryStore) memorySessionEntryLocked(scope Scope, entryID string) *SessionEntry {
	ref, ok := s.agentSessionEntryIndex[portfolioKey(scope, entryID)]
	if !ok {
		return nil
	}
	return s.agentSessionEntries[ref.sessionKey][ref.index]
}

func (s *MemoryStore) LoadAgentSessionPath(_ context.Context, scope Scope, sessionID string) (*AgentSessionPath, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	leaf := s.agentSessions[portfolioKey(scope, sessionID)]
	if leaf == nil {
		return nil, ErrAgentSessionNotFound
	}
	path, err := s.memorySessionPathLocked(leaf, nil)
	if err != nil {
		return nil, err
	}
	return cloneAgentSessionPath(path), nil
}

// memorySessionPathLocked builds the path ending at leaf (which may be a
// modified copy of the stored row), with extra entries appended to the leaf's
// stored segment. The result shares stored values; clone before returning it.
func (s *MemoryStore) memorySessionPathLocked(leaf *AgentSession, extra []*SessionEntry) (*AgentSessionPath, error) {
	chain := []*AgentSession{leaf}
	for current := leaf; current.ParentSessionID != ""; {
		if len(chain) > MaxAgentSessionDepth {
			return nil, ErrAgentSessionForkTooDeep
		}
		parent := s.agentSessions[portfolioKey(current.Scope, current.ParentSessionID)]
		if parent == nil {
			return nil, fmt.Errorf("%w: parent session %s is missing", ErrInvalidAgentSession, current.ParentSessionID)
		}
		chain = append(chain, parent)
		current = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	segments := make([][]*SessionEntry, len(chain))
	for i, session := range chain {
		segments[i] = s.agentSessionEntries[portfolioKey(session.Scope, session.ID)]
	}
	if len(extra) > 0 {
		last := len(chain) - 1
		merged := make([]*SessionEntry, 0, len(segments[last])+len(extra))
		merged = append(merged, segments[last]...)
		segments[last] = append(merged, extra...)
	}
	return buildAgentSessionPath(chain, segments, nil)
}

func cloneAgentSessionPath(in *AgentSessionPath) *AgentSessionPath {
	out := &AgentSessionPath{
		Session:    cloneAgentSession(in.Session),
		Chain:      make([]*AgentSession, len(in.Chain)),
		Compaction: cloneSessionEntry(in.Compaction),
		Entries:    make([]*SessionEntry, len(in.Entries)),
		Tokens:     in.Tokens,
	}
	for i, session := range in.Chain {
		out.Chain[i] = cloneAgentSession(session)
	}
	for i, entry := range in.Entries {
		out.Entries[i] = cloneSessionEntry(entry)
	}
	return out
}

func (s *MemoryStore) ListAgentSessionEntries(_ context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	filter, err := normalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var candidates []*SessionEntry
	if filter.SessionID != "" {
		entries := s.agentSessionEntries[portfolioKey(filter.Scope, filter.SessionID)]
		var after int64
		if filter.AfterCompaction {
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Type == SessionEntryCompaction {
					after = entries[i].Sequence
					break
				}
			}
		}
		for _, entry := range entries {
			if entry.Sequence > after && sessionEntryMatchesFilter(entry, filter) {
				candidates = append(candidates, entry)
			}
		}
	} else {
		for _, entries := range s.agentSessionEntries {
			for _, entry := range entries {
				if sessionEntryMatchesFilter(entry, filter) {
					candidates = append(candidates, entry)
				}
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			switch {
			case !a.CreatedAt.Equal(b.CreatedAt):
				return a.CreatedAt.Before(b.CreatedAt)
			case a.SessionID != b.SessionID:
				return a.SessionID < b.SessionID
			default:
				return a.Sequence < b.Sequence
			}
		})
	}
	if filter.Descending {
		for i, j := 0, len(candidates)-1; i < j; i, j = i+1, j-1 {
			candidates[i], candidates[j] = candidates[j], candidates[i]
		}
	}
	if len(candidates) > filter.Limit {
		candidates = candidates[:filter.Limit]
	}
	out := make([]*SessionEntry, len(candidates))
	for i, entry := range candidates {
		out[i] = cloneSessionEntry(entry)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Append

// AppendAgentSessionEntries appends one batch, fenced by the Run lease (or,
// with Lease nil, by the Run being terminal) and by run.SessionID ==
// req.SessionID. The watermark CAS, the intervention cursor, the entries and
// the events commit together.
func (s *MemoryStore) AppendAgentSessionEntries(_ context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	req.Now = memorySessionNow(req.Now)
	if err := validateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := validateSessionEvents(req.Scope, req.RunID, req.Events, ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Lease != nil {
		if _, err := s.memoryRunLeaseLocked(req.Scope, req.RunID, *req.Lease); err != nil {
			return nil, err
		}
	}
	run := s.agentRuns[portfolioKey(req.Scope, req.RunID)]
	if run == nil {
		return nil, ErrRunNotFound
	}
	if req.Lease == nil && !isTerminalAgentRunStatus(run.Status) {
		return nil, ErrLeaseLost
	}
	if run.SessionID != req.SessionID {
		return nil, ErrSessionMoved
	}
	return s.appendAgentSessionEntriesCoreLocked(req)
}

// appendAgentSessionEntriesLocked is the in-transaction append. The caller
// holds s.mu and owns the fence (lease and session binding are not checked);
// everything else, including the watermark CAS and the cursor, is as
// AppendAgentSessionEntries.
func (s *MemoryStore) appendAgentSessionEntriesLocked(req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	req.Now = memorySessionNow(req.Now)
	if err := validateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := validateSessionEvents(req.Scope, req.RunID, req.Events, ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	return s.appendAgentSessionEntriesCoreLocked(req)
}

func (s *MemoryStore) appendAgentSessionEntriesCoreLocked(req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	session := s.agentSessions[portfolioKey(req.Scope, req.SessionID)]
	if session == nil {
		return nil, ErrAgentSessionNotFound
	}
	if replayed, ok, err := s.memoryAppendReplayLocked(req, session); err != nil || ok {
		return replayed, err
	}
	var run *AgentRun
	if req.ConsumeInterventionsThrough != nil {
		run = s.agentRuns[portfolioKey(req.Scope, req.RunID)]
		if run == nil {
			return nil, ErrRunNotFound
		}
		through := *req.ConsumeInterventionsThrough
		if through < run.ConsumedInterventions || through > len(run.PendingInterventions) {
			return nil, invalidEntry("intervention cursor %d is outside %d..%d", through, run.ConsumedInterventions, len(run.PendingInterventions))
		}
	}
	plan, err := s.planMemoryAppendLocked(req, session, run)
	if err != nil {
		return nil, err
	}
	stored := s.commitMemoryAppendLocked(plan)
	result := &AppendAgentSessionEntriesResult{
		Session: cloneAgentSession(stored),
		Entries: make([]*SessionEntry, len(plan.entries)),
		Skipped: plan.skipped,
		Events:  s.appendSessionEventsLocked(req.Events, req.Now),
	}
	for i, entry := range plan.entries {
		result.Entries[i] = cloneSessionEntry(entry)
	}
	return result, nil
}

// memoryAppendReplayLocked reports a whole-batch replay: every request entry is
// stored in this session already, or would be skipped by its SourceKey. An
// entry id stored in another session, or a partial batch, is a conflict.
func (s *MemoryStore) memoryAppendReplayLocked(req AppendAgentSessionEntriesRequest, session *AgentSession) (*AppendAgentSessionEntriesResult, bool, error) {
	if len(req.Entries) == 0 {
		return nil, false, nil
	}
	sessionKey := portfolioKey(req.Scope, session.ID)
	found := false
	for _, entry := range req.Entries {
		if ref, ok := s.agentSessionEntryIndex[portfolioKey(req.Scope, entry.ID)]; ok {
			if ref.sessionKey != sessionKey {
				return nil, false, ErrAgentSessionConflict
			}
			found = true
		}
	}
	if !found {
		return nil, false, nil
	}
	result := &AppendAgentSessionEntriesResult{Session: cloneAgentSession(session), Replayed: true, Entries: []*SessionEntry{}}
	for _, entry := range req.Entries {
		if stored := s.memorySessionEntryLocked(req.Scope, entry.ID); stored != nil {
			result.Entries = append(result.Entries, cloneSessionEntry(stored))
			continue
		}
		if entry.SourceKey != "" {
			if _, ok := s.agentSessionSourceKeys[memorySessionSourceKey(req.Scope, session.ID, entry.SourceKey)]; ok {
				result.Skipped = append(result.Skipped, entry.ID)
				continue
			}
		}
		return nil, false, ErrAgentSessionConflict
	}
	return result, true, nil
}

type memoryAppendPlan struct {
	req       AppendAgentSessionEntriesRequest
	session   *AgentSession // the session row after the append
	entries   []*SessionEntry
	skipped   []string
	run       *AgentRun // the Run row after the cursor write; nil when unchanged
	sessionUp bool      // the session row changes
}

// planMemoryAppendLocked runs every check and builds the new rows without
// writing anything, so a failure leaves the store untouched. session is the
// current row (or the not-yet-inserted row of a new session); run is the
// current Run row when the request moves the cursor.
func (s *MemoryStore) planMemoryAppendLocked(req AppendAgentSessionEntriesRequest, session *AgentSession, run *AgentRun) (*memoryAppendPlan, error) {
	if req.ExpectedChannelWatermark != nil && session.ChannelWatermark != *req.ExpectedChannelWatermark {
		return nil, ErrChannelWatermarkMoved
	}
	next := cloneAgentSession(session)
	plan := &memoryAppendPlan{req: req, session: next, skipped: []string{}}
	skippedSteps := make(map[string]bool)
	batchKeys := make(map[string]bool)
	for _, entry := range req.Entries {
		if entry.SourceKey != "" {
			if _, ok := s.agentSessionSourceKeys[memorySessionSourceKey(req.Scope, session.ID, entry.SourceKey)]; ok || batchKeys[entry.SourceKey] {
				plan.skipped = append(plan.skipped, entry.ID)
				if entry.Type == SessionEntryAssistant {
					skippedSteps[entry.StepID] = true
				}
				continue
			}
			batchKeys[entry.SourceKey] = true
		}
		if entry.Type == SessionEntryTool && len(entry.Tool.Directive) == 0 && skippedSteps[entry.StepID] {
			plan.skipped = append(plan.skipped, entry.ID)
			continue
		}
		if s.memorySessionEntryLocked(req.Scope, entry.ID) != nil {
			return nil, ErrAgentSessionConflict
		}
		stamped := stampAppendedSessionEntry(entry, req, next.NextSequence+int64(len(plan.entries)))
		if err := stamped.Validate(); err != nil {
			return nil, err
		}
		if stamped.Type == SessionEntryCompaction {
			leaf := cloneAgentSession(session)
			leaf.NextSequence = stamped.Sequence
			path, err := s.memorySessionPathLocked(leaf, plan.entries)
			if err != nil {
				return nil, err
			}
			if err := validateCompactionAgainstPath(path, stamped); err != nil {
				return nil, err
			}
		}
		if stamped.Type == SessionEntryModelChange {
			next.ModelProvider = stamped.ModelChange.Provider
			next.Model = stamped.ModelChange.Model
		}
		plan.entries = append(plan.entries, stamped)
	}
	if len(plan.entries) > 0 {
		next.NextSequence += int64(len(plan.entries))
		plan.sessionUp = true
	}
	if req.ExpectedChannelWatermark != nil && req.ChannelWatermark != next.ChannelWatermark {
		next.ChannelWatermark = req.ChannelWatermark
		plan.sessionUp = true
	}
	if plan.sessionUp {
		next.Revision++
		next.UpdatedAt = req.Now
	}
	if run != nil && *req.ConsumeInterventionsThrough != run.ConsumedInterventions {
		updated := cloneAgentRun(run)
		updated.ConsumedInterventions = *req.ConsumeInterventionsThrough
		updated.Revision++
		updated.UpdatedAt = req.Now
		plan.run = updated
	}
	return plan, nil
}

// commitMemoryAppendLocked writes a plan; it cannot fail. It returns the
// stored session row.
func (s *MemoryStore) commitMemoryAppendLocked(plan *memoryAppendPlan) *AgentSession {
	req := plan.req
	key := portfolioKey(req.Scope, plan.session.ID)
	for _, entry := range plan.entries {
		s.agentSessionEntries[key] = append(s.agentSessionEntries[key], entry)
		s.agentSessionEntryIndex[portfolioKey(req.Scope, entry.ID)] = memorySessionEntryRef{sessionKey: key, index: int(entry.Sequence - 1)}
		if entry.SourceKey != "" {
			s.agentSessionSourceKeys[memorySessionSourceKey(req.Scope, plan.session.ID, entry.SourceKey)] = struct{}{}
		}
	}
	if plan.sessionUp {
		s.agentSessions[key] = cloneAgentSession(plan.session)
	}
	if plan.run != nil {
		s.saveMemoryAgentRunLocked(portfolioKey(plan.run.Scope, plan.run.ID), plan.run)
	}
	return s.agentSessions[key]
}

// ---------------------------------------------------------------------------
// Tool entry transitions

// TransitionToolEntry moves one tool entry (CAS on state and revision), fenced
// by the lease of the entry's Run. The session binding is not checked, so a
// promoted Run can still settle entries it left in the parent session.
func (s *MemoryStore) TransitionToolEntry(_ context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	req.Now = memorySessionNow(req.Now)
	if err := validateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := validateSessionEvents(req.Scope, req.RunID, req.Events, ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Lease.WorkerID == "" {
		// Terminal close: the Run has no lease any more, so its being
		// terminal is the fence.
		run := s.agentRuns[portfolioKey(req.Scope, req.RunID)]
		if run == nil {
			return nil, ErrRunNotFound
		}
		if !isTerminalAgentRunStatus(run.Status) {
			return nil, ErrLeaseLost
		}
	} else if _, err := s.memoryRunLeaseLocked(req.Scope, req.RunID, req.Lease); err != nil {
		return nil, err
	}
	return s.transitionToolEntryCoreLocked(req)
}

// transitionToolEntryLocked is the in-transaction transition. The caller holds
// s.mu and owns the lease check.
func (s *MemoryStore) transitionToolEntryLocked(req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	req.Now = memorySessionNow(req.Now)
	if err := validateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := validateSessionEvents(req.Scope, req.RunID, req.Events, ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	return s.transitionToolEntryCoreLocked(req)
}

func (s *MemoryStore) transitionToolEntryCoreLocked(req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	ref, ok := s.agentSessionEntryIndex[portfolioKey(req.Scope, req.EntryID)]
	if !ok {
		return nil, ErrSessionEntryNotFound
	}
	next, err := applyToolEntryTransition(s.agentSessionEntries[ref.sessionKey][ref.index], req)
	if err != nil {
		return nil, err
	}
	s.agentSessionEntries[ref.sessionKey][ref.index] = next
	return &ToolEntryTransitionResult{
		Entry:  cloneSessionEntry(next),
		Events: s.appendSessionEventsLocked(req.Events, req.Now),
	}, nil
}
