package conversationrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
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

type StartConversationTaskRequest struct {
	Scope                  models.Scope
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
	Budget          *models.BudgetPolicy
}

type GetConversationTaskRequest struct {
	Scope              models.Scope
	Owner              models.ObjectiveOwner
	ConversationID     string
	ThreadRootID       string
	AuthenticatedActor models.ConversationParticipant
	TaskID             string
	Viewer             *models.ConversationViewer
}

type CancelConversationTaskRequest struct {
	GetConversationTaskRequest
	ExpectedWorkRunRevision int64
	Actor                   models.ConversationParticipant
	Summary                 string
}

type ConversationTaskService struct {
	store             models.ConversationTaskKernelStore
	executionPreparer models.AcceptedRunExecutionPreparer
	now               func() time.Time
}

func NewConversationTaskService(store models.ConversationTaskKernelStore, opts ...models.ServiceOption) *ConversationTaskService {
	return &ConversationTaskService{store: store, now: models.ClockFrom(opts)}
}

func (s *ConversationTaskService) SetAcceptedRunExecutionPreparer(preparer models.AcceptedRunExecutionPreparer) {
	if s != nil {
		s.executionPreparer = preparer
	}
}

// Start is a kernel boundary, not a public caller-identity API. It derives
// provenance from the current settled source Turn and canonical triggering
// message, then commits task, work, funding, acknowledgment and audit together.
func (s *ConversationTaskService) Start(ctx context.Context, req StartConversationTaskRequest) (*models.ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, models.ErrInvalidConversationTask
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidOpaqueIdentifier(req.SourceRunID, 128) || !models.IsValidOpaqueIdentifier(req.TurnID, 128) || !models.IsValidOpaqueIdentifier(req.TaskKey, 128) ||
		!models.IsValidOpaqueIdentifier(req.ToolEntryID, 128) || !models.IsValidOpaqueIdentifier(req.WorkerID, 256) || req.ExpectedSourceRevision < 1 {
		return nil, models.ErrInvalidConversationTask
	}
	sessions, ok := s.store.(models.AgentSessionStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	source, err := s.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	// A timeout may adopt this exact Run while an already-started model call
	// is proposing independent work. Reuse the committed continuation before
	// preparing a fresh budget or admitting a second execution identity.
	existing, taskErr := s.store.FindConversationTaskByWorkRunID(ctx, source.Scope, source.ID)
	if taskErr != nil && !errors.Is(taskErr, agentrun.ErrConversationTaskNotFound) {
		return nil, taskErr
	}
	if existing != nil {
		verified, sourceReport, err := conversation.TaskForReport(ctx, s.store, source)
		if err != nil {
			return nil, err
		}
		if existing.Mode != models.ConversationTaskModeContinuation || verified == nil || sourceReport || !models.SameConversationTask(existing, verified) {
			return nil, models.ErrConversationTaskConflict
		}
		return &models.ConversationTaskResult{Task: existing, SourceRun: source, WorkRun: models.CloneAgentRun(source), Replayed: true,
			AcknowledgmentMessageID: models.ConversationTaskAcknowledgmentMessageID(existing)}, nil
	}
	conversationID, _ := source.Context[models.ConversationRunConversationIDContextKey].(string)
	messageID, _ := source.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := s.store.GetConversation(ctx, req.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, models.ErrConversationNotFound
	}
	message, err := s.store.GetChannelMessage(ctx, req.Scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, models.ErrChannelMessageNotFound
	}
	if models.VoiceCallStartedMessage(message) {
		return nil, models.ErrInvalidConversationTask
	}
	turn, err := s.store.GetAgentTurn(ctx, req.Scope, req.TurnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, models.ErrTurnNotFound
	}
	if source.Kind != models.RunKindConversation || source.Source != models.RunSourceChat || source.ParentRunID != "" || source.RootRunID != source.ID ||
		source.Owner != conversation.Owner || conversation.Scope != source.Scope || message.ConversationID != conversation.ID ||
		turn.RunID != source.ID || turn.Status != models.AgentTurnStatusCompleted || turn.Sequence != source.LastAppliedTurn ||
		source.AssignedAgentID == "" || req.AssignedAgentID != source.AssignedAgentID {
		return nil, models.ErrInvalidConversationTask
	}
	if strings.TrimSpace(req.Goal) == "" || len(req.Goal) > 16384 || strings.TrimSpace(req.Acknowledgment) == "" || len(req.Acknowledgment) > 600 {
		return nil, models.ErrInvalidConversationTask
	}
	if req.Budget != nil {
		if err := req.Budget.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", models.ErrInvalidConversationTask, err)
		}
	}
	entry, assistant, err := verifyStartTaskToolEntry(ctx, sessions, source, turn, req)
	if err != nil {
		return nil, err
	}
	onRoster, err := models.AgentOnChatRoster(ctx, models.TeamRosterReaderOf(s.store), conversation.Scope, conversation.Owner, source.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	if !onRoster {
		return nil, models.ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == models.ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != models.ConversationParticipantUser || actor.Validate() != nil {
		return nil, models.ErrInvalidConversationTask
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
			return nil, models.ErrInvalidConversationTask
		}
	}
	id := models.ConversationTaskID(req.Scope, source.ID, req.TaskKey)
	workID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:conversation-task-work:"+id)).String()
	now := s.now().UTC()
	task := &models.ConversationTask{ID: id, Scope: req.Scope, Owner: source.Owner, ConversationID: conversation.ID, SourceMessageID: message.ID,
		ThreadRootID: threadID, SourceRunID: source.ID, SourceTurnID: turn.ID, SourceTurnNumber: turn.Sequence, SourceToolEntryID: entry.ID, AuthenticatedActor: actor,
		TargetAgentID: source.AssignedAgentID, WorkRunID: workID, TaskKey: req.TaskKey, Goal: req.Goal, Acknowledgment: req.Acknowledgment,
		RequestedBudget: models.CloneBudgetPolicy(req.Budget), Revision: 1, CreatedAt: now}
	task.RequestDigest, err = models.ConversationTaskDigest(task)
	if err != nil {
		return nil, err
	}
	existing, err = s.store.GetConversationTask(ctx, req.Scope, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !models.SameConversationTask(existing, task) {
			return nil, models.ErrConversationTaskConflict
		}
		work, workErr := s.store.GetAgentRun(ctx, req.Scope, existing.WorkRunID)
		if workErr != nil {
			return nil, workErr
		}
		if work == nil {
			return nil, models.ErrRunNotFound
		}
		// The admission committed the entry's started result with the task;
		// the entry read above may predate a concurrent admission's commit.
		if entry.State != models.ToolEntryCompleted {
			if entry, err = sessions.GetSessionEntry(ctx, req.Scope, entry.ID); err != nil {
				return nil, err
			}
		}
		if entry.State != models.ToolEntryCompleted || entry.Tool == nil || !models.SameToolResult(entry.Tool.Result, models.ConversationTaskStartedToolResult(existing)) {
			return nil, models.ErrConversationTaskConflict
		}
		return &models.ConversationTaskResult{Task: existing, WorkRun: work, SourceRun: source, Replayed: true}, nil
	}
	if entry.State != models.ToolEntryPending && entry.State != models.ToolEntryRunning {
		return nil, models.ErrConversationTaskConflict
	}
	if len(source.PendingInterventions) != source.ConsumedInterventions {
		return nil, models.ErrSteeringArrived
	}
	if conversation.Status != models.ConversationStatusActive {
		return nil, models.ErrInvalidConversationTask
	}
	if err := models.ValidateConversationTaskSource(source, task, req.ExpectedSourceRevision, req.WorkerID); err != nil {
		return nil, err
	}
	allocation := req.Budget
	if allocation == nil {
		allocation = &models.BudgetPolicy{}
	}
	budget, err := models.CompleteChildBudgetAllocation(source, allocation)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidConversationTask, err)
	}
	if source.Budget != nil {
		hostedBudget, budgetErr := modelstep.ControlRunBudget(source, "")
		if budgetErr != nil {
			return nil, budgetErr
		}
		if err := modelstep.ControlChildBudgetFloor(budget, hostedBudget); err != nil {
			return nil, fmt.Errorf("%w: %v", models.ErrInvalidConversationTask, err)
		}
	}
	// The work acts for the person who asked for it.
	actingUser := ""
	if models.IsValidActingUser(task.AuthenticatedActor.ID) {
		actingUser = task.AuthenticatedActor.ID
	}
	workRequest := models.CreateAgentRunRequest{Scope: req.Scope, Kind: models.RunKindAgentWork, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
		ActingUser: actingUser, ConcurrencyKey: "task:" + id, Goal: req.Goal, SessionID: models.RunAgentSessionID(req.Scope, workID), Source: models.RunSourceChat, Priority: source.Priority, Deadline: source.Deadline, Budget: budget, Policy: maps.Clone(source.Policy),
		Context: map[string]interface{}{models.ConversationTaskContextKey: id, models.ConversationRunConversationIDContextKey: conversation.ID,
			models.ConversationRunTriggerIDContextKey: message.ID, "threadRootMessageId": threadID, models.RunReportingRootRunIDContextKey: workID,
			models.RunReportingMilestonesContextKey: []interface{}{"completed", "failed"}}}
	if err := agentrun.PrepareAcceptedChildRun(ctx, source, &workRequest, s.executionPreparer, false); err != nil {
		return nil, err
	}
	work, err := portfolio.BuildAgentRun(ctx, s.store, workRequest, workID, now)
	if err != nil {
		return nil, err
	}
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, work.ID, budget); err != nil {
		return nil, err
	}
	updated.Status = models.AgentRunStatusCompleted
	updated.Context[models.RunReportingRootRunIDContextKey] = source.ID
	updated.Context[models.RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": req.Acknowledgment, "conversationTaskIds": []string{id}, "conversationTaskWorkRunId": workID}
	updated.Error = ""
	updated.WakeCondition = nil
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	updated.UpdatedAt = now
	event := &models.ActivityEvent{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("conversation-task-admitted:"+id)).String(), Scope: req.Scope, RunID: source.ID,
		AgentID: source.AssignedAgentID, EventType: "conversation.task_started", Summary: "Started independent conversation work", Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID},
		Visibility: models.ActivityVisibilityScope, CorrelationID: id, ConversationRefs: []string{conversation.ID}, Payload: map[string]interface{}{"taskId": id, "workRunId": workID, "sourceTurnId": turn.ID, "sourceToolEntryId": entry.ID}, CreatedAt: now}
	// The work Run inherits the chat up to, not including, the step that
	// decided to start it (A3), then reads its brief.
	seed, err := models.ChildSessionSeed(req.Scope, workID, "task", models.ConversationTaskBrief(req.Goal))
	if err != nil {
		return nil, err
	}
	workSession, err := models.NewChildSession(models.ChildSessionRequest{Scope: req.Scope, ParentSessionID: source.SessionID, ForkSequence: assistant.Sequence - 1,
		OwnerRunID: workID, AgentID: source.AssignedAgentID, ConversationID: conversation.ID, ThreadRootID: threadID, Seed: seed})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidConversationTask, err)
	}
	workSession.Now = now
	transition := &models.ToolEntryTransitionRequest{Scope: req.Scope, EntryID: entry.ID, RunID: source.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: req.WorkerID, Now: now}, From: entry.State, ExpectedRevision: entry.Revision,
		To: models.ToolEntryCompleted, Result: models.ConversationTaskStartedToolResult(task), Now: now}
	return s.store.CreateConversationTask(ctx, models.ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated,
		ExpectedSourceRevision: source.Revision, WorkerID: req.WorkerID, Event: event, ToolTransition: transition, WorkSession: &workSession})
}

// verifyStartTaskToolEntry proves that req is exactly the source step's
// conversation_start_task call (section 04 §7.3): the entry belongs to the
// source Run's session and step, its decoded arguments are the request's goal
// and acknowledgment, and the task key derives from its id. It returns the
// entry and its step's assistant entry (the work session's fork point). The
// entry state is checked by the caller: open for a fresh admission, completed
// for a replay.
func verifyStartTaskToolEntry(ctx context.Context, sessions models.AgentSessionStore, source *models.AgentRun, step *models.AgentTurn, req StartConversationTaskRequest) (*models.SessionEntry, *models.SessionEntry, error) {
	entry, err := models.OptionalSessionEntry(sessions.GetSessionEntry(ctx, req.Scope, req.ToolEntryID))
	if err != nil {
		return nil, nil, err
	}
	if entry == nil || entry.Type != models.SessionEntryTool || entry.Tool == nil || entry.RunID != source.ID ||
		entry.StepID != step.ID || entry.Step != step.Sequence || entry.Tool.Name != string(models.ControlToolStartTask) ||
		source.SessionID == "" || entry.SessionID != source.SessionID || req.TaskKey != models.StartTaskKey(entry.ID) {
		return nil, nil, models.ErrConversationTaskConflict
	}
	args := models.StartTaskEntryArgs(entry)
	if args == nil || args.Goal != req.Goal || args.Acknowledgment != req.Acknowledgment {
		return nil, nil, models.ErrConversationTaskConflict
	}
	assistant, err := models.OptionalSessionEntry(sessions.GetSessionEntry(ctx, req.Scope, models.SessionEntryID(entry.SessionID, models.AssistantEntryKey(entry.StepID))))
	if err != nil {
		return nil, nil, err
	}
	if assistant == nil || assistant.Type != models.SessionEntryAssistant || assistant.SessionID != entry.SessionID || assistant.StepID != entry.StepID ||
		assistant.Sequence < 2 {
		return nil, nil, models.ErrConversationTaskConflict
	}
	return entry, assistant, nil
}

func (s *ConversationTaskService) Get(ctx context.Context, req GetConversationTaskRequest) (*models.ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, models.ErrInvalidConversationTask
	}
	if err := (models.ConversationTaskFilter{Scope: req.Scope, Owner: req.Owner, ConversationID: req.ConversationID, ThreadRootID: req.ThreadRootID, AuthenticatedActor: req.AuthenticatedActor, Limit: 1}).Validate(); err != nil {
		return nil, err
	}
	task, err := s.store.GetConversationTask(ctx, req.Scope, req.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.Owner != req.Owner || task.ConversationID != req.ConversationID || req.ThreadRootID != "" && task.ThreadRootID != req.ThreadRootID || req.AuthenticatedActor.Type != "" && req.AuthenticatedActor != task.AuthenticatedActor {
		return nil, agentrun.ErrConversationTaskNotFound
	}
	if req.Viewer != nil {
		_, getErr := conversation.NewService(s.store, models.WithClock(s.now)).GetVisibleChannelMessage(ctx, req.Scope, task.ConversationID, task.SourceMessageID, *req.Viewer)
		if getErr != nil {
			if errors.Is(getErr, models.ErrChannelMessageNotFound) {
				return nil, agentrun.ErrConversationTaskNotFound
			}
			return nil, getErr
		}
	}
	work, err := s.store.GetAgentRun(ctx, req.Scope, task.WorkRunID)
	if err != nil {
		return nil, err
	}
	if work == nil {
		return nil, models.ErrRunNotFound
	}
	result := &models.ConversationTaskResult{Task: task, WorkRun: work}
	if models.IsTerminalAgentRunStatus(work.Status) {
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

func (s *ConversationTaskService) List(ctx context.Context, filter models.ConversationTaskFilter) ([]*models.ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, models.ErrInvalidConversationTask
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	conversation, err := s.store.GetConversation(ctx, filter.Scope, filter.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Owner != filter.Owner {
		return nil, models.ErrConversationNotFound
	}
	return s.store.ListConversationTasks(ctx, filter)
}

func (s *ConversationTaskService) Cancel(ctx context.Context, req CancelConversationTaskRequest) (*models.ConversationTaskResult, error) {
	result, err := s.Get(ctx, req.GetConversationTaskRequest)
	if err != nil {
		return nil, err
	}
	if req.Actor.Type != models.ConversationParticipantUser || req.Actor.Validate() != nil || req.Actor != result.Task.AuthenticatedActor {
		return nil, models.ErrInvalidConversationTask
	}
	store, ok := s.store.(models.RunCommandStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	if models.IsTerminalAgentRunStatus(result.WorkRun.Status) {
		return result, nil
	}
	if req.ExpectedWorkRunRevision < 1 {
		return nil, models.ErrInvalidConversationTask
	}
	command, err := agentrun.NewRunCommandService(store, models.WithClock(s.now)).CommandAgentRun(ctx, agentrun.CommandRequest{Scope: req.Scope, RunID: result.WorkRun.ID,
		ExpectedRevision: req.ExpectedWorkRunRevision, Kind: agentrun.CommandCancel, Actor: models.ActivityActor{Type: string(req.Actor.Type), ID: req.Actor.ID}, Summary: req.Summary})
	if err != nil {
		return nil, err
	}
	result.WorkRun = command.Run
	return result, nil
}
