package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

type memoryConversationTaskIndexKey struct {
	Scope          Scope
	Owner          ObjectiveOwner
	ConversationID string
	ThreadRootID   string
	Actor          ConversationParticipant
}

type memoryConversationTaskPageKey struct {
	CreatedAt time.Time
	ID        string
}

func memoryConversationTaskPageLess(a, b memoryConversationTaskPageKey) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

func memoryConversationTaskIndexKeys(task *ConversationTask) []memoryConversationTaskIndexKey {
	keys := make([]memoryConversationTaskIndexKey, 0, 4)
	for _, thread := range []string{"", task.ThreadRootID} {
		for _, actor := range []ConversationParticipant{{}, task.AuthenticatedActor} {
			keys = append(keys, memoryConversationTaskIndexKey{Scope: task.Scope, Owner: task.Owner, ConversationID: task.ConversationID, ThreadRootID: thread, Actor: actor})
		}
	}
	return keys
}

func (s *MemoryStore) saveMemoryConversationTaskLocked(task *ConversationTask, run *AgentRun) {
	if s.conversationTasks == nil {
		s.conversationTasks = make(map[string]*ConversationTask)
		s.conversationTaskWorkRuns = make(map[string]string)
		s.conversationTaskOrder = make(map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
		s.conversationTaskActiveOrder = make(map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
	}
	key := portfolioKey(task.Scope, task.ID)
	s.conversationTasks[key] = cloneConversationTask(task)
	s.conversationTaskWorkRuns[portfolioKey(task.Scope, task.WorkRunID)] = key
	page := memoryConversationTaskPageKey{CreatedAt: task.CreatedAt, ID: task.ID}
	for _, index := range memoryConversationTaskIndexKeys(task) {
		s.conversationTaskOrder[index] = memoryOrderedIndexInsert(s.conversationTaskOrder[index], page, memoryConversationTaskPageLess)
		if !isTerminalAgentRunStatus(run.Status) {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexInsert(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		}
	}
}

func (s *MemoryStore) refreshMemoryConversationTaskRunLocked(run *AgentRun) {
	task := s.conversationTasks[s.conversationTaskWorkRuns[portfolioKey(run.Scope, run.ID)]]
	if task == nil {
		return
	}
	page := memoryConversationTaskPageKey{CreatedAt: task.CreatedAt, ID: task.ID}
	for _, index := range memoryConversationTaskIndexKeys(task) {
		if isTerminalAgentRunStatus(run.Status) {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexDelete(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		} else {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexInsert(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		}
	}
}

func (s *MemoryStore) CreateConversationTask(ctx context.Context, record ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConversationTaskCreateRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(record.Task.Scope, record.Task.ID)
	if task := s.conversationTasks[key]; task != nil {
		if !sameConversationTask(task, record.Task) {
			return nil, ErrConversationTaskConflict
		}
		if record.ToolTransition != nil {
			if err := conversationTaskToolReplayMatches(record, s.memorySessionEntryLocked(record.Task.Scope, record.ToolTransition.EntryID)); err != nil {
				return nil, err
			}
		}
		return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(s.agentRuns[portfolioKey(task.Scope, task.WorkRunID)]),
			SourceRun: cloneAgentRun(s.agentRuns[portfolioKey(task.Scope, task.SourceRunID)]), Replayed: true}, nil
	}
	sourceKey := portfolioKey(record.Task.Scope, record.Task.SourceRunID)
	if continuation := s.conversationTasks[s.conversationTaskWorkRuns[sourceKey]]; continuation != nil && continuation.Mode == ConversationTaskModeContinuation {
		return replayConversationTaskContinuation(continuation, s.agentRuns[sourceKey])
	}
	current := s.agentRuns[sourceKey]
	if err := conversationTaskSteeringSettled(record, current); err != nil {
		return nil, err
	}
	if err := validateConversationTaskSourceUpdate(current, record); err != nil {
		return nil, err
	}
	workKey := portfolioKey(record.WorkRun.Scope, record.WorkRun.ID)
	if s.agentRuns[workKey] != nil || s.conversationTaskWorkRuns[workKey] != "" {
		return nil, ErrConversationTaskConflict
	}
	if err := s.validateMemoryRunSkillDependenciesLocked(record.WorkRun); err != nil {
		return nil, err
	}
	if err := s.validateMemoryRunSkillDependenciesLocked(record.SourceRun); err != nil {
		return nil, err
	}
	// Every fallible step runs before the first write: the tool entry
	// transition is computed here and stored after the session exists.
	var entryRef memorySessionEntryRef
	var completed *SessionEntry
	var transition ToolEntryTransitionRequest
	if record.ToolTransition != nil {
		transition = *record.ToolTransition
		transition.Now = memorySessionNow(transition.Now)
		entry := s.memorySessionEntryLocked(record.Task.Scope, transition.EntryID)
		var assistant *SessionEntry
		if entry != nil {
			assistant = s.memorySessionEntryLocked(record.Task.Scope, SessionEntryID(entry.SessionID, AssistantEntryKey(entry.StepID)))
		}
		if err := verifyConversationTaskToolEntry(record, current, entry, assistant); err != nil {
			return nil, err
		}
		next, err := applyToolEntryTransition(entry, transition)
		if err != nil {
			return nil, err
		}
		entryRef, completed = s.agentSessionEntryIndex[portfolioKey(record.Task.Scope, transition.EntryID)], next
	}
	if record.WorkSession != nil {
		// The session helper reads the owner Run from the map and sets
		// nothing on it (WorkRun.SessionID is already the session id). The
		// Run is placed bare for the call and saved with its indexes below.
		s.agentRuns[workKey] = record.WorkRun
		_, err := s.createRunAgentSessionLocked(*record.WorkSession)
		delete(s.agentRuns, workKey)
		if err != nil {
			return nil, err
		}
	}
	s.saveMemoryConversationTaskLocked(record.Task, record.WorkRun)
	s.saveMemoryAgentRunLocked(workKey, record.WorkRun)
	s.saveMemoryAgentRunLocked(sourceKey, record.SourceRun)
	if completed != nil {
		s.agentSessionEntries[entryRef.sessionKey][entryRef.index] = completed
		s.appendSessionEventsLocked(transition.Events, transition.Now)
	}
	appendMemoryActivityLocked(s, record.Event)
	return &ConversationTaskResult{Task: cloneConversationTask(record.Task), WorkRun: cloneAgentRun(record.WorkRun), SourceRun: cloneAgentRun(record.SourceRun)}, nil
}

func (s *MemoryStore) GetConversationTask(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationTask(s.conversationTasks[portfolioKey(scope, id)]), nil
}

func (s *MemoryStore) FindConversationTaskByWorkRunID(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationTask(s.conversationTasks[s.conversationTaskWorkRuns[portfolioKey(scope, id)]]), nil
}

func (s *MemoryStore) ListConversationTasks(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := memoryConversationTaskIndexKey{Scope: filter.Scope, Owner: filter.Owner, ConversationID: filter.ConversationID, ThreadRootID: filter.ThreadRootID, Actor: filter.AuthenticatedActor}
	order := s.conversationTaskOrder[index]
	if filter.ActiveOnly {
		order = s.conversationTaskActiveOrder[index]
	}
	var cursor *memoryConversationTaskPageKey
	if filter.BeforeCreatedAt != nil {
		cursor = &memoryConversationTaskPageKey{CreatedAt: *filter.BeforeCreatedAt, ID: filter.BeforeID}
	}
	pages := memoryOrderedIndexAfter(order, cursor, nil, filter.Limit, memoryConversationTaskPageLess)
	results := make([]*ConversationTaskResult, 0, len(pages))
	for _, page := range pages {
		task := s.conversationTasks[portfolioKey(filter.Scope, page.ID)]
		results = append(results, &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(s.agentRuns[portfolioKey(filter.Scope, task.WorkRunID)])})
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// conversation_start_task admission (A4, section 04 §7.4), shared by every
// backend's CreateConversationTask.

// ConversationTaskStartedToolResult is the conversation_start_task tool result
// written by the admission transaction.
func ConversationTaskStartedToolResult(task *ConversationTask) *ToolResult {
	return controlJSONResult(ToolResultSucceeded, map[string]interface{}{"status": "started", "taskId": task.ID,
		"workRunId": task.WorkRunID, "acknowledgment": task.Acknowledgment})
}

// validateConversationTaskToolRecord checks the tool transition and the work
// session of a create record before its transaction.
func validateConversationTaskToolRecord(record ConversationTaskCreateRecord) error {
	t, w, source := record.Task, record.WorkRun, record.SourceRun
	transition, session := record.ToolTransition, record.WorkSession
	if t.SourceToolEntryID == "" {
		if transition != nil || session != nil {
			return ErrInvalidConversationTask
		}
		return nil
	}
	if transition == nil || session == nil || !validOpaqueIdentifier(t.SourceToolEntryID, 128) {
		return ErrInvalidConversationTask
	}
	if transition.Scope != t.Scope || transition.EntryID != t.SourceToolEntryID || transition.RunID != t.SourceRunID ||
		(transition.From != ToolEntryPending && transition.From != ToolEntryRunning) || transition.To != ToolEntryCompleted ||
		transition.Lease.WorkerID != record.WorkerID || transition.ActionCallID != "" ||
		!sameToolResult(transition.Result, ConversationTaskStartedToolResult(t)) {
		return ErrInvalidConversationTask
	}
	if err := validateToolEntryTransitionRequest(*transition); err != nil {
		return err
	}
	if err := validateSessionEvents(transition.Scope, transition.RunID, transition.Events, ErrInvalidToolEntryTransition); err != nil {
		return err
	}
	if session.Scope != t.Scope || session.OwnerRunID != w.ID || session.AgentID != w.AssignedAgentID ||
		session.ConversationID != t.ConversationID || session.ParentSessionID == "" || session.ParentSessionID != source.SessionID ||
		w.SessionID != RunAgentSessionID(t.Scope, w.ID) {
		return ErrInvalidConversationTask
	}
	return validateCreateRunAgentSessionTxRequest(*session)
}

// conversationTaskSteeringSettled refuses an admission while the source Run
// has steering its loop has not read yet (section 04 §7.2 rule 2).
func conversationTaskSteeringSettled(record ConversationTaskCreateRecord, current *AgentRun) error {
	if record.ToolTransition != nil && current != nil && current.ConsumedInterventions != len(current.PendingInterventions) {
		return ErrSteeringArrived
	}
	return nil
}

// verifyConversationTaskToolEntry checks, inside the transaction, that the
// entry is the source step's conversation_start_task call in the chat session
// and that the work session forks just before that step's assistant entry.
func verifyConversationTaskToolEntry(record ConversationTaskCreateRecord, current *AgentRun, entry, assistant *SessionEntry) error {
	if record.ToolTransition == nil {
		return nil
	}
	t := record.Task
	if entry == nil {
		return ErrSessionEntryNotFound
	}
	if entry.Type != SessionEntryTool || entry.Tool == nil || entry.Tool.Name != string(ControlToolStartTask) ||
		entry.RunID != current.ID || entry.SessionID != current.SessionID || entry.SessionID != record.WorkSession.ParentSessionID ||
		entry.StepID != t.SourceTurnID || entry.Step != t.SourceTurnNumber {
		return ErrConversationTaskConflict
	}
	if entry.State != record.ToolTransition.From {
		return ErrToolEntryStateConflict
	}
	if assistant == nil || assistant.Type != SessionEntryAssistant || assistant.SessionID != entry.SessionID ||
		assistant.StepID != entry.StepID || record.WorkSession.ForkSequence != assistant.Sequence-1 {
		return ErrConversationTaskConflict
	}
	return nil
}

// conversationTaskToolReplayMatches accepts a replayed admission only when
// its tool entry already holds the same started result.
func conversationTaskToolReplayMatches(record ConversationTaskCreateRecord, entry *SessionEntry) error {
	if entry == nil || entry.State != ToolEntryCompleted || entry.Tool == nil || !sameToolResult(entry.Tool.Result, record.ToolTransition.Result) {
		return ErrConversationTaskConflict
	}
	return nil
}

// sameToolResult compares results as JSON values: Postgres jsonb reorders the
// keys of a json content part.
func sameToolResult(a, b *ToolResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	var left, right interface{}
	for _, side := range []struct {
		result *ToolResult
		into   *interface{}
	}{{a, &left}, {b, &right}} {
		raw, err := json.Marshal(side.result)
		if err != nil || json.Unmarshal(raw, side.into) != nil {
			return false
		}
	}
	return reflect.DeepEqual(left, right)
}

// optionalSessionEntry maps a missing entry to nil for the checks above.
func optionalSessionEntry(entry *SessionEntry, err error) (*SessionEntry, error) {
	if errors.Is(err, ErrSessionEntryNotFound) {
		return nil, nil
	}
	return entry, err
}
