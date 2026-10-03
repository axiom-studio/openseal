package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

const ConversationTaskContextKey = "openseal.conversationTaskId"

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
	ErrInvalidConversationTask  = errors.New("invalid conversation task")
	ErrConversationTaskNotFound = errors.New("conversation task not found")
	ErrConversationTaskConflict = errors.New("conversation task request conflicts with accepted work")
)

// ConversationTask is immutable provenance for independently admitted work.
// Lifecycle state remains authoritative on WorkRunID; a source chat never owns
// or cancels that run through the parent/child lifecycle.
type ConversationTask struct {
	ID                 string                  `json:"id"`
	Scope              Scope                   `json:"scope"`
	Owner              ObjectiveOwner          `json:"owner"`
	ConversationID     string                  `json:"conversationId"`
	SourceMessageID    string                  `json:"sourceMessageId"`
	ThreadRootID       string                  `json:"threadRootId"`
	SourceRunID        string                  `json:"sourceRunId"`
	SourceTurnID       string                  `json:"sourceTurnId"`
	SourceTurnNumber   int64                   `json:"sourceTurnNumber"`
	AuthenticatedActor ConversationParticipant `json:"authenticatedActor"`
	TargetAgentID      string                  `json:"targetAgentId"`
	WorkRunID          string                  `json:"workRunId"`
	TaskKey            string                  `json:"taskKey"`
	Goal               string                  `json:"goal"`
	Acknowledgment     string                  `json:"acknowledgment"`
	RequestedBudget    *BudgetPolicy           `json:"requestedBudget,omitempty"`
	RequestDigest      string                  `json:"requestDigest"`
	Revision           int64                   `json:"revision"`
	CreatedAt          time.Time               `json:"createdAt"`
}

func (t *ConversationTask) Validate() error {
	if t == nil {
		return ErrInvalidConversationTask
	}
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if err := t.Owner.Validate(); err != nil {
		return err
	}
	for _, value := range []string{t.ID, t.ConversationID, t.SourceMessageID, t.ThreadRootID, t.SourceRunID, t.SourceTurnID, t.TargetAgentID, t.WorkRunID, t.TaskKey} {
		if !validOpaqueIdentifier(value, 128) {
			return ErrInvalidConversationTask
		}
	}
	if t.SourceRunID == t.WorkRunID || t.SourceTurnNumber < 1 || t.Revision != 1 || t.CreatedAt.IsZero() ||
		t.AuthenticatedActor.Type != ConversationParticipantUser || t.AuthenticatedActor.Validate() != nil ||
		strings.TrimSpace(t.Goal) == "" || len(t.Goal) > 16384 || strings.TrimSpace(t.Acknowledgment) == "" || len(t.Acknowledgment) > 600 {
		return ErrInvalidConversationTask
	}
	if t.RequestedBudget != nil {
		if err := t.RequestedBudget.Validate(); err != nil {
			return err
		}
	}
	digest, err := conversationTaskDigest(t)
	if err != nil || t.RequestDigest != digest {
		return ErrInvalidConversationTask
	}
	return nil
}

func conversationTaskDigest(task *ConversationTask) (string, error) {
	if task == nil {
		return "", ErrInvalidConversationTask
	}
	copy := *task
	copy.RequestDigest = ""
	copy.CreatedAt = time.Time{}
	copy.Revision = 0
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func cloneConversationTask(task *ConversationTask) *ConversationTask {
	if task == nil {
		return nil
	}
	copy := *task
	copy.RequestedBudget = cloneBudgetPolicy(task.RequestedBudget)
	return &copy
}

func sameConversationTask(a, b *ConversationTask) bool {
	return a != nil && b != nil && a.ID == b.ID && a.Scope == b.Scope && a.RequestDigest == b.RequestDigest
}

type ConversationTaskResult struct {
	Task                    *ConversationTask `json:"task"`
	WorkRun                 *AgentRun         `json:"workRun"`
	SourceRun               *AgentRun         `json:"sourceRun,omitempty"`
	Replayed                bool              `json:"replayed,omitempty"`
	TerminalReportMessageID string            `json:"terminalReportMessageId,omitempty"`
}

type ConversationTaskFilter struct {
	Scope              Scope
	Owner              ObjectiveOwner
	ConversationID     string
	ThreadRootID       string
	AuthenticatedActor ConversationParticipant
	ActiveOnly         bool
	Limit              int
	BeforeCreatedAt    *time.Time
	BeforeID           string
}

func (f ConversationTaskFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	if err := f.Owner.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(f.ConversationID, 128) || f.ThreadRootID != "" && !validOpaqueIdentifier(f.ThreadRootID, 128) ||
		f.Limit < 1 || f.Limit > 100 || (f.BeforeCreatedAt == nil) != (f.BeforeID == "") ||
		f.BeforeID != "" && (!validOpaqueIdentifier(f.BeforeID, 128) || f.BeforeCreatedAt.IsZero()) {
		return ErrInvalidConversationTask
	}
	if f.AuthenticatedActor.Type != "" || f.AuthenticatedActor.ID != "" {
		if f.AuthenticatedActor.Type != ConversationParticipantUser || f.AuthenticatedActor.Validate() != nil {
			return ErrInvalidConversationTask
		}
	}
	return nil
}

type ConversationTaskCreateRecord struct {
	Task                   *ConversationTask
	WorkRun                *AgentRun
	SourceRun              *AgentRun
	ExpectedSourceRevision int64
	WorkerID               string
	Event                  *ActivityEvent
}

type ConversationTaskStore interface {
	CreateConversationTask(context.Context, ConversationTaskCreateRecord) (*ConversationTaskResult, error)
	GetConversationTask(context.Context, Scope, string) (*ConversationTask, error)
	FindConversationTaskByWorkRunID(context.Context, Scope, string) (*ConversationTask, error)
	ListConversationTasks(context.Context, ConversationTaskFilter) ([]*ConversationTaskResult, error)
}

type ConversationTaskKernelStore interface {
	ConversationTaskStore
	ConversationStore
	PortfolioStore
	AgentTurnStore
	RunActivityStore
}

type StartConversationTaskRequest struct {
	Scope                  Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	WorkerID               string
	TurnID                 string
	AssignedAgentID        string
	TaskKey                string
	Goal                   string
	Acknowledgment         string
	Budget                 *BudgetPolicy
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

func conversationTaskID(scope Scope, sourceRunID, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:conversation-task:"+scope.key()+":"+sourceRunID+":"+key)).String()
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
		!validOpaqueIdentifier(req.WorkerID, 256) || req.ExpectedSourceRevision < 1 {
		return nil, ErrInvalidConversationTask
	}
	source, err := s.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
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
		turn.RunID != source.ID || turn.Status != AgentTurnStatusCompleted || turn.Sequence != source.LastAppliedTurn || turn.RequestedTask == nil ||
		!slices.Equal(turn.InputInterventionIDs, interventionIDs(source)) ||
		source.AssignedAgentID == "" || req.AssignedAgentID != source.AssignedAgentID {
		return nil, ErrInvalidConversationTask
	}
	proposal := &TurnTaskProposal{TaskKey: req.TaskKey, Goal: req.Goal, Acknowledgment: req.Acknowledgment, Budget: cloneBudgetPolicy(req.Budget)}
	if err := proposal.Validate(); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(proposal, turn.RequestedTask) {
		return nil, ErrConversationTaskConflict
	}
	if source.Owner.Type == OwnerTypeAgent && source.Owner.ID != source.AssignedAgentID {
		return nil, ErrInvalidConversationTask
	}
	if source.Owner.Type == OwnerTypeTeam {
		teams, ok := s.store.(collaborationTeamStore)
		if !ok {
			return nil, ErrInvalidConversationTask
		}
		team, teamErr := teams.GetTeamDeployment(ctx, capability.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}, source.Owner.ID)
		if teamErr != nil {
			return nil, teamErr
		}
		found := false
		if team != nil && string(team.Status) == "active" {
			for _, member := range team.Roster {
				if member.AgentDeploymentID == source.AssignedAgentID {
					found = true
					break
				}
			}
		}
		if !found {
			return nil, ErrInvalidConversationTask
		}
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
		ThreadRootID: threadID, SourceRunID: source.ID, SourceTurnID: turn.ID, SourceTurnNumber: turn.Sequence, AuthenticatedActor: actor,
		TargetAgentID: source.AssignedAgentID, WorkRunID: workID, TaskKey: req.TaskKey, Goal: req.Goal, Acknowledgment: req.Acknowledgment,
		RequestedBudget: cloneBudgetPolicy(req.Budget), Revision: 1, CreatedAt: now}
	task.RequestDigest, err = conversationTaskDigest(task)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.GetConversationTask(ctx, req.Scope, id)
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
		return &ConversationTaskResult{Task: existing, WorkRun: work, SourceRun: source, Replayed: true}, nil
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
		return nil, err
	}
	if source.Budget != nil {
		hostedBudget, budgetErr := projectHostedRunBudget(source, "")
		if budgetErr != nil {
			return nil, budgetErr
		}
		if err := validateHostedChildBudgetFloor(budget, hostedBudget); err != nil {
			return nil, err
		}
	}
	workRequest := CreateAgentRunRequest{Scope: req.Scope, Kind: RunKindAgentWork, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
		ConcurrencyKey: "task:" + id, Goal: req.Goal, Source: RunSourceChat, Priority: source.Priority, Deadline: source.Deadline, Budget: budget, Policy: cloneMap(source.Policy),
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
		Visibility: ActivityVisibilityScope, CorrelationID: id, ConversationRefs: []string{conversation.ID}, Payload: map[string]interface{}{"taskId": id, "workRunId": workID, "sourceTurnId": turn.ID}, CreatedAt: now}
	return s.store.CreateConversationTask(ctx, ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated,
		ExpectedSourceRevision: source.Revision, WorkerID: req.WorkerID, Event: event})
}

func validateConversationTaskSource(current *AgentRun, task *ConversationTask, revision int64, worker string) error {
	if current == nil {
		return ErrRunNotFound
	}
	if current.Revision != revision {
		return ErrRevisionConflict
	}
	if current.Status != AgentRunStatusRunning || current.LeaseOwner != worker || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(time.Now().UTC()) {
		return ErrLeaseLost
	}
	if current.Scope != task.Scope || current.ID != task.SourceRunID || current.Kind != RunKindConversation || current.Source != RunSourceChat ||
		current.ParentRunID != "" || current.RootRunID != current.ID || current.Owner != task.Owner || current.AssignedAgentID != task.TargetAgentID ||
		current.LastAppliedTurn != task.SourceTurnNumber || current.Context[conversationRunContextConversationID] != task.ConversationID ||
		current.Context[conversationRunContextTriggerID] != task.SourceMessageID {
		return ErrInvalidConversationTask
	}
	return nil
}

func validateConversationTaskCreateRecord(record ConversationTaskCreateRecord) error {
	if record.Task == nil || record.WorkRun == nil || record.SourceRun == nil || record.Event == nil || record.ExpectedSourceRevision < 1 || !validOpaqueIdentifier(record.WorkerID, 256) {
		return ErrInvalidConversationTask
	}
	if err := record.Task.Validate(); err != nil {
		return err
	}
	if err := record.WorkRun.Validate(); err != nil {
		return err
	}
	if err := record.SourceRun.Validate(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	t, w, source := record.Task, record.WorkRun, record.SourceRun
	if t.ID != conversationTaskID(t.Scope, t.SourceRunID, t.TaskKey) || w.ID != t.WorkRunID || w.Scope != t.Scope || w.Owner != t.Owner ||
		w.AssignedAgentID != t.TargetAgentID || w.Goal != t.Goal || w.Kind != RunKindAgentWork || w.Source != RunSourceChat ||
		w.ParentRunID != "" || w.RootRunID != w.ID || w.ConcurrencyKey != "task:"+t.ID || w.Status != AgentRunStatusQueued || w.ObjectiveID != "" || w.Entrypoint != "" ||
		w.Context[ConversationTaskContextKey] != t.ID || w.Context[conversationRunContextConversationID] != t.ConversationID || w.Context[conversationRunContextTriggerID] != t.SourceMessageID ||
		w.Context[runReportingContextRootRunID] != w.ID || source.ID != t.SourceRunID || source.Scope != t.Scope || source.Owner != t.Owner ||
		source.Revision != record.ExpectedSourceRevision+1 || source.Status != AgentRunStatusCompleted || source.Output["summary"] != t.Acknowledgment ||
		source.Output["conversationTaskWorkRunId"] != w.ID || source.LeaseOwner != "" || source.LeaseExpiresAt != nil || source.CompletedAt == nil ||
		record.Event.Scope != t.Scope || record.Event.RunID != source.ID || record.Event.CorrelationID != t.ID {
		return ErrInvalidConversationTask
	}
	ids, ok := source.Output["conversationTaskIds"].([]string)
	if !ok {
		if raw, valid := source.Output["conversationTaskIds"].([]interface{}); valid && len(raw) == 1 {
			if text, valid := raw[0].(string); valid {
				ids = []string{text}
				ok = true
			}
		}
	}
	if !ok || len(ids) != 1 || ids[0] != t.ID {
		return ErrInvalidConversationTask
	}
	return nil
}

// Only the completion and its exact task budget allocation may change during
// admission. This guards low-level persistence callers as well as factories.
func validateConversationTaskSourceUpdate(current *AgentRun, record ConversationTaskCreateRecord) error {
	if err := validateConversationTaskSource(current, record.Task, record.ExpectedSourceRevision, record.WorkerID); err != nil {
		return err
	}
	expected := cloneAgentRun(current)
	if err := addRunBudgetAllocation(expected, record.WorkRun.ID, record.WorkRun.Budget); err != nil {
		return err
	}
	proposed := record.SourceRun
	expected.Context[runReportingContextRootRunID] = current.ID
	expected.Context[runReportingContextMilestones] = []interface{}{"completed", "failed"}
	expected.Status = proposed.Status
	expected.Output = cloneMap(proposed.Output)
	expected.Error = proposed.Error
	expected.WakeCondition = proposed.WakeCondition
	expected.CompletedAt = proposed.CompletedAt
	expected.LeaseOwner = proposed.LeaseOwner
	expected.LeaseExpiresAt = proposed.LeaseExpiresAt
	expected.Revision = proposed.Revision
	expected.UpdatedAt = proposed.UpdatedAt
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(proposed)
	if string(a) != string(b) {
		return ErrConversationTaskConflict
	}
	if !reflect.DeepEqual(record.WorkRun.Deadline, current.Deadline) {
		return ErrConversationTaskConflict
	}
	return nil
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
		messageID := stableConversationID(task.Scope, "run-reporting-terminal:"+work.ID+":"+string(work.Status), "message")
		message, getErr := s.store.GetChannelMessage(ctx, task.Scope, task.ConversationID, messageID)
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
