package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConversationTaskGoalSummary bounds model-visible status context without
// modifying the original work goal or breaking UTF-8 characters.
func ConversationTaskGoalSummary(goal string) string {
	count := 0
	for index := range goal {
		if count == 512 {
			return goal[:index]
		}
		count++
	}
	return goal
}

var (
	ErrConversationTaskNotFound = errors.New("conversation task not found")
)

type StartConversationTaskRequest struct {
	Scope                  Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	WorkerID               string
	TurnID                 string
	// ToolEntryID is the source step's conversation_start_task tool entry;
	// TaskKey must be "task-" + ToolEntryID.
	ToolEntryID     string
	AssignedAgentID string
	TaskKey         string
	Goal            string
	Acknowledgment  string
	Budget          *BudgetPolicy
}

type GetConversationTaskRequest struct {
	Scope              Scope
	Owner              ObjectiveOwner
	ConversationID     string
	ThreadRootID       string
	AuthenticatedActor ConversationParticipant
	TaskID             string
	Viewer             *ConversationViewer
}

type CancelConversationTaskRequest struct {
	GetConversationTaskRequest
	ExpectedWorkRunRevision int64
	Actor                   ConversationParticipant
	Summary                 string
}

type ConversationTaskService struct {
	store             ConversationTaskKernelStore
	executionPreparer AcceptedRunExecutionPreparer
	now               func() time.Time
}

func NewConversationTaskService(store ConversationTaskKernelStore) *ConversationTaskService {
	return &ConversationTaskService{store: store, now: time.Now}
}

func (s *ConversationTaskService) SetAcceptedRunExecutionPreparer(preparer AcceptedRunExecutionPreparer) {
	if s != nil {
		s.executionPreparer = preparer
	}
}

// Start is a kernel boundary, not a public caller-identity API. It derives
// provenance from the current settled source Turn and canonical triggering
// message, then commits task, work, funding, acknowledgment and audit together.
func (s *ConversationTaskService) Start(ctx context.Context, req StartConversationTaskRequest) (*ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalidConversationTask
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.SourceRunID, 128) || !validOpaqueIdentifier(req.TurnID, 128) || !validOpaqueIdentifier(req.TaskKey, 128) ||
		!validOpaqueIdentifier(req.ToolEntryID, 128) || !validOpaqueIdentifier(req.WorkerID, 256) || req.ExpectedSourceRevision < 1 {
		return nil, ErrInvalidConversationTask
	}
	sessions, ok := s.store.(AgentSessionStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	source, err := s.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	// A timeout may adopt this exact Run while an already-started model call
	// is proposing independent work. Reuse the committed continuation before
	// preparing a fresh budget or admitting a second execution identity.
	existing, taskErr := s.store.FindConversationTaskByWorkRunID(ctx, source.Scope, source.ID)
	if taskErr != nil && !errors.Is(taskErr, ErrConversationTaskNotFound) {
		return nil, taskErr
	}
	if existing != nil {
		verified, sourceReport, err := conversationTaskForReport(ctx, s.store, source)
		if err != nil {
			return nil, err
		}
		if existing.Mode != ConversationTaskModeContinuation || verified == nil || sourceReport || !sameConversationTask(existing, verified) {
			return nil, ErrConversationTaskConflict
		}
		return &ConversationTaskResult{Task: existing, SourceRun: source, WorkRun: cloneAgentRun(source), Replayed: true,
			AcknowledgmentMessageID: ConversationTaskAcknowledgmentMessageID(existing)}, nil
	}
	conversationID, _ := source.Context[conversationRunContextConversationID].(string)
	messageID, _ := source.Context[conversationRunContextTriggerID].(string)
	conversation, err := s.store.GetConversation(ctx, req.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	message, err := s.store.GetChannelMessage(ctx, req.Scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrChannelMessageNotFound
	}
	if voiceCallStartedMessage(message) {
		return nil, ErrInvalidConversationTask
	}
	turn, err := s.store.GetAgentTurn(ctx, req.Scope, req.TurnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, ErrTurnNotFound
	}
	if source.Kind != RunKindConversation || source.Source != RunSourceChat || source.ParentRunID != "" || source.RootRunID != source.ID ||
		source.Owner != conversation.Owner || conversation.Scope != source.Scope || message.ConversationID != conversation.ID ||
		turn.RunID != source.ID || turn.Status != AgentTurnStatusCompleted || turn.Sequence != source.LastAppliedTurn ||
		source.AssignedAgentID == "" || req.AssignedAgentID != source.AssignedAgentID {
		return nil, ErrInvalidConversationTask
	}
	if strings.TrimSpace(req.Goal) == "" || len(req.Goal) > 16384 || strings.TrimSpace(req.Acknowledgment) == "" || len(req.Acknowledgment) > 600 {
		return nil, ErrInvalidConversationTask
	}
	if req.Budget != nil {
		if err := req.Budget.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidConversationTask, err)
		}
	}
	entry, assistant, err := verifyStartTaskToolEntry(ctx, sessions, source, turn, req)
	if err != nil {
		return nil, err
	}
	onRoster, err := AgentOnChatRoster(ctx, teamRosterReader(s.store), conversation.Scope, conversation.Owner, source.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	if !onRoster {
		return nil, ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, ErrInvalidConversationTask
	}
	threadID := message.ThreadRootID
	if threadID == "" {
		threadID = message.ID
	}
	if threadID != message.ID {
		root, rootErr := s.store.GetChannelMessage(ctx, req.Scope, conversationID, threadID)
		if rootErr != nil {
			return nil, rootErr
		}
		if root == nil || root.ConversationID != conversation.ID {
			return nil, ErrInvalidConversationTask
		}
	}
	id := conversationTaskID(req.Scope, source.ID, req.TaskKey)
	workID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:conversation-task-work:"+id)).String()
	now := s.now().UTC()
	task := &ConversationTask{ID: id, Scope: req.Scope, Owner: source.Owner, ConversationID: conversation.ID, SourceMessageID: message.ID,
		ThreadRootID: threadID, SourceRunID: source.ID, SourceTurnID: turn.ID, SourceTurnNumber: turn.Sequence, SourceToolEntryID: entry.ID, AuthenticatedActor: actor,
		TargetAgentID: source.AssignedAgentID, WorkRunID: workID, TaskKey: req.TaskKey, Goal: req.Goal, Acknowledgment: req.Acknowledgment,
		RequestedBudget: cloneBudgetPolicy(req.Budget), Revision: 1, CreatedAt: now}
	task.RequestDigest, err = conversationTaskDigest(task)
	if err != nil {
		return nil, err
	}
	existing, err = s.store.GetConversationTask(ctx, req.Scope, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameConversationTask(existing, task) {
			return nil, ErrConversationTaskConflict
		}
		work, workErr := s.store.GetAgentRun(ctx, req.Scope, existing.WorkRunID)
		if workErr != nil {
			return nil, workErr
		}
		if work == nil {
			return nil, ErrRunNotFound
		}
		// The admission committed the entry's started result with the task;
		// the entry read above may predate a concurrent admission's commit.
		if entry.State != ToolEntryCompleted {
			if entry, err = sessions.GetSessionEntry(ctx, req.Scope, entry.ID); err != nil {
				return nil, err
			}
		}
		if entry.State != ToolEntryCompleted || entry.Tool == nil || !sameToolResult(entry.Tool.Result, ConversationTaskStartedToolResult(existing)) {
			return nil, ErrConversationTaskConflict
		}
		return &ConversationTaskResult{Task: existing, WorkRun: work, SourceRun: source, Replayed: true}, nil
	}
	if entry.State != ToolEntryPending && entry.State != ToolEntryRunning {
		return nil, ErrConversationTaskConflict
	}
	if len(source.PendingInterventions) != source.ConsumedInterventions {
		return nil, ErrSteeringArrived
	}
	if conversation.Status != ConversationStatusActive {
		return nil, ErrInvalidConversationTask
	}
	if err := validateConversationTaskSource(source, task, req.ExpectedSourceRevision, req.WorkerID); err != nil {
		return nil, err
	}
	allocation := req.Budget
	if allocation == nil {
		allocation = &BudgetPolicy{}
	}
	budget, err := completeChildBudgetAllocation(source, allocation)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConversationTask, err)
	}
	if source.Budget != nil {
		hostedBudget, budgetErr := controlRunBudget(source, "")
		if budgetErr != nil {
			return nil, budgetErr
		}
		if err := controlChildBudgetFloor(budget, hostedBudget); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidConversationTask, err)
		}
	}
	// The work acts for the person who asked for it.
	actingUser := ""
	if validActingUser(task.AuthenticatedActor.ID) {
		actingUser = task.AuthenticatedActor.ID
	}
	workRequest := CreateAgentRunRequest{Scope: req.Scope, Kind: RunKindAgentWork, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
		ActingUser: actingUser, ConcurrencyKey: "task:" + id, Goal: req.Goal, SessionID: RunAgentSessionID(req.Scope, workID), Source: RunSourceChat, Priority: source.Priority, Deadline: source.Deadline, Budget: budget, Policy: maps.Clone(source.Policy),
		Context: map[string]interface{}{ConversationTaskContextKey: id, conversationRunContextConversationID: conversation.ID,
			conversationRunContextTriggerID: message.ID, "threadRootMessageId": threadID, runReportingContextRootRunID: workID,
			runReportingContextMilestones: []interface{}{"completed", "failed"}}}
	if err := prepareAcceptedChildRun(ctx, source, &workRequest, s.executionPreparer, false); err != nil {
		return nil, err
	}
	work, err := buildAgentRun(ctx, s.store, workRequest, workID, now)
	if err != nil {
		return nil, err
	}
	updated := cloneAgentRun(source)
	if err := addRunBudgetAllocation(updated, work.ID, budget); err != nil {
		return nil, err
	}
	updated.Status = AgentRunStatusCompleted
	updated.Context[runReportingContextRootRunID] = source.ID
	updated.Context[runReportingContextMilestones] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": req.Acknowledgment, "conversationTaskIds": []string{id}, "conversationTaskWorkRunId": workID}
	updated.Error = ""
	updated.WakeCondition = nil
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	updated.UpdatedAt = now
	event := &ActivityEvent{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("conversation-task-admitted:"+id)).String(), Scope: req.Scope, RunID: source.ID,
		AgentID: source.AssignedAgentID, EventType: "conversation.task_started", Summary: "Started independent conversation work", Actor: ActivityActor{Type: string(actor.Type), ID: actor.ID},
		Visibility: ActivityVisibilityScope, CorrelationID: id, ConversationRefs: []string{conversation.ID}, Payload: map[string]interface{}{"taskId": id, "workRunId": workID, "sourceTurnId": turn.ID, "sourceToolEntryId": entry.ID}, CreatedAt: now}
	// The work Run inherits the chat up to, not including, the step that
	// decided to start it (A3), then reads its brief.
	seed, err := childSessionSeed(req.Scope, workID, "task", conversationTaskBrief(req.Goal))
	if err != nil {
		return nil, err
	}
	workSession, err := newChildSession(ChildSessionRequest{Scope: req.Scope, ParentSessionID: source.SessionID, ForkSequence: assistant.Sequence - 1,
		OwnerRunID: workID, AgentID: source.AssignedAgentID, ConversationID: conversation.ID, ThreadRootID: threadID, Seed: seed})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConversationTask, err)
	}
	workSession.Now = now
	transition := &ToolEntryTransitionRequest{Scope: req.Scope, EntryID: entry.ID, RunID: source.ID,
		Lease: AgentRunLeaseGuard{WorkerID: req.WorkerID, Now: now}, From: entry.State, ExpectedRevision: entry.Revision,
		To: ToolEntryCompleted, Result: ConversationTaskStartedToolResult(task), Now: now}
	return s.store.CreateConversationTask(ctx, ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated,
		ExpectedSourceRevision: source.Revision, WorkerID: req.WorkerID, Event: event, ToolTransition: transition, WorkSession: &workSession})
}

// verifyStartTaskToolEntry proves that req is exactly the source step's
// conversation_start_task call (section 04 §7.3): the entry belongs to the
// source Run's session and step, its decoded arguments are the request's goal
// and acknowledgment, and the task key derives from its id. It returns the
// entry and its step's assistant entry (the work session's fork point). The
// entry state is checked by the caller: open for a fresh admission, completed
// for a replay.
func verifyStartTaskToolEntry(ctx context.Context, sessions AgentSessionStore, source *AgentRun, step *AgentTurn, req StartConversationTaskRequest) (*SessionEntry, *SessionEntry, error) {
	entry, err := optionalSessionEntry(sessions.GetSessionEntry(ctx, req.Scope, req.ToolEntryID))
	if err != nil {
		return nil, nil, err
	}
	if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.RunID != source.ID ||
		entry.StepID != step.ID || entry.Step != step.Sequence || entry.Tool.Name != string(ControlToolStartTask) ||
		source.SessionID == "" || entry.SessionID != source.SessionID || req.TaskKey != startTaskKey(entry.ID) {
		return nil, nil, ErrConversationTaskConflict
	}
	args := startTaskArguments(entry)
	if args == nil || args.Goal != req.Goal || args.Acknowledgment != req.Acknowledgment {
		return nil, nil, ErrConversationTaskConflict
	}
	assistant, err := optionalSessionEntry(sessions.GetSessionEntry(ctx, req.Scope, SessionEntryID(entry.SessionID, AssistantEntryKey(entry.StepID))))
	if err != nil {
		return nil, nil, err
	}
	if assistant == nil || assistant.Type != SessionEntryAssistant || assistant.SessionID != entry.SessionID || assistant.StepID != entry.StepID ||
		assistant.Sequence < 2 {
		return nil, nil, ErrConversationTaskConflict
	}
	return entry, assistant, nil
}

func (s *ConversationTaskService) Get(ctx context.Context, req GetConversationTaskRequest) (*ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalidConversationTask
	}
	if err := (ConversationTaskFilter{Scope: req.Scope, Owner: req.Owner, ConversationID: req.ConversationID, ThreadRootID: req.ThreadRootID, AuthenticatedActor: req.AuthenticatedActor, Limit: 1}).Validate(); err != nil {
		return nil, err
	}
	task, err := s.store.GetConversationTask(ctx, req.Scope, req.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.Owner != req.Owner || task.ConversationID != req.ConversationID || req.ThreadRootID != "" && task.ThreadRootID != req.ThreadRootID || req.AuthenticatedActor.Type != "" && req.AuthenticatedActor != task.AuthenticatedActor {
		return nil, ErrConversationTaskNotFound
	}
	if req.Viewer != nil {
		_, getErr := NewConversationService(s.store).GetVisibleChannelMessage(ctx, req.Scope, task.ConversationID, task.SourceMessageID, *req.Viewer)
		if getErr != nil {
			if errors.Is(getErr, ErrChannelMessageNotFound) {
				return nil, ErrConversationTaskNotFound
			}
			return nil, getErr
		}
	}
	work, err := s.store.GetAgentRun(ctx, req.Scope, task.WorkRunID)
	if err != nil {
		return nil, err
	}
	if work == nil {
		return nil, ErrRunNotFound
	}
	result := &ConversationTaskResult{Task: task, WorkRun: work}
	if isTerminalAgentRunStatus(work.Status) {
		message, getErr := FindConversationTaskResultMessage(ctx, s.store, task, work)
		if getErr != nil {
			return nil, getErr
		}
		if message != nil && message.ConversationID == task.ConversationID {
			result.TerminalReportMessageID = message.ID
		}
	}
	return result, nil
}

func (s *ConversationTaskService) List(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalidConversationTask
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	conversation, err := s.store.GetConversation(ctx, filter.Scope, filter.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Owner != filter.Owner {
		return nil, ErrConversationNotFound
	}
	return s.store.ListConversationTasks(ctx, filter)
}

func (s *ConversationTaskService) Cancel(ctx context.Context, req CancelConversationTaskRequest) (*ConversationTaskResult, error) {
	result, err := s.Get(ctx, req.GetConversationTaskRequest)
	if err != nil {
		return nil, err
	}
	if req.Actor.Type != ConversationParticipantUser || req.Actor.Validate() != nil || req.Actor != result.Task.AuthenticatedActor {
		return nil, ErrInvalidConversationTask
	}
	store, ok := s.store.(RunCommandStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	if isTerminalAgentRunStatus(result.WorkRun.Status) {
		return result, nil
	}
	if req.ExpectedWorkRunRevision < 1 {
		return nil, ErrInvalidConversationTask
	}
	command, err := NewRunCommandService(store).CommandAgentRun(ctx, AgentRunCommandRequest{Scope: req.Scope, RunID: result.WorkRun.ID,
		ExpectedRevision: req.ExpectedWorkRunRevision, Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: string(req.Actor.Type), ID: req.Actor.ID}, Summary: req.Summary})
	if err != nil {
		return nil, err
	}
	result.WorkRun = command.Run
	return result, nil
}

// PersistedTaskOrigin resolves an exact independent work identity. Caller
// context is only a hint; hosting applications must verify canonical source
// conversation/message/actor and their existing authorization policy.
func PersistedTaskOrigin(ctx context.Context, store ConversationTaskStore, scope Scope, workRunID string) (*ConversationTask, error) {
	if store == nil {
		return nil, ErrInvalidConversationTask
	}
	task, err := store.FindConversationTaskByWorkRunID(ctx, scope, workRunID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrConversationTaskNotFound
	}
	if task.Scope != scope || task.WorkRunID != workRunID || task.Validate() != nil {
		return nil, ErrInvalidConversationTask
	}
	return task, nil
}
