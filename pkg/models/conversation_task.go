package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

const ConversationTaskContextKey = "openseal.conversationTaskId"

type ConversationTaskMode string

const (
	// Empty mode preserves the existing independently admitted work contract.
	ConversationTaskModeIndependent  ConversationTaskMode = ""
	ConversationTaskModeContinuation ConversationTaskMode = "continuation"
)

var (
	ErrInvalidConversationTask  = errors.New("invalid conversation task")
	ErrConversationTaskConflict = errors.New("conversation task request conflicts with accepted work")
)

// ConversationTask is immutable provenance for independently admitted work.
// Lifecycle state remains authoritative on WorkRunID; a source chat never owns
// or cancels that run through the parent/child lifecycle.
type ConversationTask struct {
	ID                 string                  `json:"id"`
	Mode               ConversationTaskMode    `json:"mode,omitempty"`
	ForegroundDeadline *time.Time              `json:"foregroundDeadline,omitempty"`
	Scope              Scope                   `json:"scope"`
	Owner              ObjectiveOwner          `json:"owner"`
	ConversationID     string                  `json:"conversationId"`
	SourceMessageID    string                  `json:"sourceMessageId"`
	ThreadRootID       string                  `json:"threadRootId"`
	SourceRunID        string                  `json:"sourceRunId"`
	SourceTurnID       string                  `json:"sourceTurnId"`
	SourceTurnNumber   int64                   `json:"sourceTurnNumber"`
	SourceToolEntryID  string                  `json:"sourceToolEntryId,omitempty"`
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
	for _, value := range []string{t.ID, t.ConversationID, t.SourceMessageID, t.ThreadRootID, t.SourceRunID, t.WorkRunID, t.TaskKey} {
		if !IsValidOpaqueIdentifier(value, 128) {
			return ErrInvalidConversationTask
		}
	}
	if t.Mode != ConversationTaskModeIndependent && t.Mode != ConversationTaskModeContinuation {
		return ErrInvalidConversationTask
	}
	if t.Mode == ConversationTaskModeContinuation {
		if t.ForegroundDeadline == nil || t.ForegroundDeadline.IsZero() || t.CreatedAt.Before(*t.ForegroundDeadline) || t.SourceRunID != t.WorkRunID || (t.SourceTurnID == "") != (t.SourceTurnNumber == 0) || t.SourceTurnNumber < 0 ||
			t.SourceTurnID != "" && !IsValidOpaqueIdentifier(t.SourceTurnID, 128) ||
			t.TargetAgentID == "" && t.Owner.Type != OwnerTypeTeam {
			return ErrInvalidConversationTask
		}
	} else if t.ForegroundDeadline != nil || t.SourceRunID == t.WorkRunID || !IsValidOpaqueIdentifier(t.SourceTurnID, 128) || t.SourceTurnNumber < 1 || !IsValidOpaqueIdentifier(t.TargetAgentID, 128) {
		return ErrInvalidConversationTask
	}
	if t.TargetAgentID != "" && !IsValidOpaqueIdentifier(t.TargetAgentID, 128) {
		return ErrInvalidConversationTask
	}
	if t.Revision != 1 || t.CreatedAt.IsZero() ||
		t.AuthenticatedActor.Type != ConversationParticipantUser || t.AuthenticatedActor.Validate() != nil ||
		strings.TrimSpace(t.Goal) == "" || len(t.Goal) > 16384 || strings.TrimSpace(t.Acknowledgment) == "" || len(t.Acknowledgment) > 600 {
		return ErrInvalidConversationTask
	}
	if t.RequestedBudget != nil {
		if err := t.RequestedBudget.Validate(); err != nil {
			return err
		}
	}
	digest, err := ConversationTaskDigest(t)
	if err != nil || t.RequestDigest != digest {
		return ErrInvalidConversationTask
	}
	return nil
}

// ConversationTaskMatchesWorkRun checks the exact execution identity of an
// already loaded canonical Task. A caller-provided context marker alone never
// establishes this proof; callers must retrieve the ledger first. An initially
// unassigned Team continuation additionally needs the persisted selected-round
// attribution proof when the kernel later assigns its Agent.
func ConversationTaskMatchesWorkRun(task *ConversationTask, run *AgentRun) bool {
	if task == nil || run == nil || task.Validate() != nil || task.ID != ConversationTaskID(task.Scope, task.SourceRunID, task.TaskKey) ||
		run.Scope != task.Scope || run.ID != task.WorkRunID || run.Owner != task.Owner ||
		run.Source != RunSourceChat || run.ParentRunID != "" || run.RootRunID != run.ID || run.ConcurrencyKey != "task:"+task.ID ||
		run.Context[ConversationTaskContextKey] != task.ID || run.Context[ConversationRunConversationIDContextKey] != task.ConversationID ||
		run.Context[ConversationRunTriggerIDContextKey] != task.SourceMessageID {
		return false
	}
	if run.AssignedAgentID != task.TargetAgentID && !(task.Mode == ConversationTaskModeContinuation && task.Owner.Type == OwnerTypeTeam &&
		task.TargetAgentID == "" && IsValidOpaqueIdentifier(run.AssignedAgentID, 128)) {
		return false
	}
	if task.Mode == ConversationTaskModeContinuation {
		thread, _ := run.Context["threadRootMessageId"].(string)
		return run.Kind == RunKindConversation && task.SourceRunID == run.ID && task.ForegroundDeadline.Equal(run.CreatedAt.Add(ConversationTaskForegroundTimeout)) &&
			(thread == task.ThreadRootID || thread == "" && task.ThreadRootID == task.SourceMessageID)
	}
	return run.Kind == RunKindAgentWork && run.Goal == task.Goal
}

func ConversationTaskDigest(task *ConversationTask) (string, error) {
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

func CloneConversationTask(task *ConversationTask) *ConversationTask {
	if task == nil {
		return nil
	}
	copy := *task
	if task.ForegroundDeadline != nil {
		deadline := *task.ForegroundDeadline
		copy.ForegroundDeadline = &deadline
	}
	copy.RequestedBudget = CloneBudgetPolicy(task.RequestedBudget)
	return &copy
}

func SameConversationTask(a, b *ConversationTask) bool {
	return a != nil && b != nil && a.ID == b.ID && a.Scope == b.Scope && a.RequestDigest == b.RequestDigest
}

type ConversationTaskResult struct {
	Task                    *ConversationTask `json:"task"`
	WorkRun                 *AgentRun         `json:"workRun"`
	SourceRun               *AgentRun         `json:"sourceRun,omitempty"`
	Replayed                bool              `json:"replayed,omitempty"`
	AcknowledgmentMessageID string            `json:"acknowledgmentMessageId,omitempty"`
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
	if !IsValidOpaqueIdentifier(f.ConversationID, 128) || f.ThreadRootID != "" && !IsValidOpaqueIdentifier(f.ThreadRootID, 128) ||
		f.Limit < 1 || f.Limit > 100 || (f.BeforeCreatedAt == nil) != (f.BeforeID == "") ||
		f.BeforeID != "" && (!IsValidOpaqueIdentifier(f.BeforeID, 128) || f.BeforeCreatedAt.IsZero()) {
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
	// ToolTransition completes the source conversation_start_task tool entry
	// (pending|running -> completed, ConversationTaskStartedToolResult) and
	// WorkSession forks the work Run's session from the chat session just
	// before that step's assistant entry. Both or neither; both exactly when
	// Task.SourceToolEntryID is set. Applied in the create transaction.
	ToolTransition *ToolEntryTransitionRequest
	WorkSession    *CreateRunAgentSessionTxRequest
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

func ConversationTaskID(scope Scope, sourceRunID, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:conversation-task:"+scope.Key()+":"+sourceRunID+":"+key)).String()
}

func ValidateConversationTaskSource(current *AgentRun, task *ConversationTask, revision int64, worker string) error {
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
		current.LastAppliedTurn != task.SourceTurnNumber || current.Context[ConversationRunConversationIDContextKey] != task.ConversationID ||
		current.Context[ConversationRunTriggerIDContextKey] != task.SourceMessageID {
		return ErrInvalidConversationTask
	}
	return nil
}

func ValidateConversationTaskCreateRecord(record ConversationTaskCreateRecord) error {
	if record.Task == nil || record.WorkRun == nil || record.SourceRun == nil || record.Event == nil || record.ExpectedSourceRevision < 1 || !IsValidOpaqueIdentifier(record.WorkerID, 256) {
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
	if t.ID != ConversationTaskID(t.Scope, t.SourceRunID, t.TaskKey) || w.ID != t.WorkRunID || w.Scope != t.Scope || w.Owner != t.Owner ||
		w.AssignedAgentID != t.TargetAgentID || w.Goal != t.Goal || w.Kind != RunKindAgentWork || w.Source != RunSourceChat ||
		w.ParentRunID != "" || w.RootRunID != w.ID || w.ConcurrencyKey != "task:"+t.ID || w.Status != AgentRunStatusQueued || w.ObjectiveID != "" || w.Entrypoint != "" ||
		w.Context[ConversationTaskContextKey] != t.ID || w.Context[ConversationRunConversationIDContextKey] != t.ConversationID || w.Context[ConversationRunTriggerIDContextKey] != t.SourceMessageID ||
		w.Context[RunReportingRootRunIDContextKey] != w.ID || source.ID != t.SourceRunID || source.Scope != t.Scope || source.Owner != t.Owner ||
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
	return validateConversationTaskToolRecord(record)
}

// Only the completion and its exact task budget allocation may change during
// admission. This guards low-level persistence callers as well as factories.
func ValidateConversationTaskSourceUpdate(current *AgentRun, record ConversationTaskCreateRecord) error {
	if err := ValidateConversationTaskSource(current, record.Task, record.ExpectedSourceRevision, record.WorkerID); err != nil {
		return err
	}
	expected := CloneAgentRun(current)
	if err := AddRunBudgetAllocation(expected, record.WorkRun.ID, record.WorkRun.Budget); err != nil {
		return err
	}
	proposed := record.SourceRun
	expected.Context[RunReportingRootRunIDContextKey] = current.ID
	expected.Context[RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	expected.Status = proposed.Status
	expected.Output = maps.Clone(proposed.Output)
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

type ConversationTaskContinuationStore interface {
	ConversationTaskPromotionStore
	ConversationTaskDueStore
}
