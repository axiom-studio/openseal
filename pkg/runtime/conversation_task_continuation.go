package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

// Promote changes only the conversation Run's presentation and execution lane.
// The store commits the canonical Task and its acknowledgment together; callers
// must not announce a handoff until this operation returns a committed result.
func (s *ConversationTaskService) Promote(ctx context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalidConversationTask
	}
	store, ok := s.store.(ConversationTaskPromotionStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.RunID, 128) || req.MinimumAge < 0 {
		return nil, ErrInvalidConversationTask
	}
	if req.Now.IsZero() {
		req.Now = s.now().UTC()
	}
	if req.MinimumAge == 0 {
		req.MinimumAge = ConversationTaskForegroundTimeout
	}
	return store.PromoteConversationTask(ctx, req)
}

type ConversationTaskContinuationStore interface {
	ConversationTaskPromotionStore
	ConversationTaskDueStore
}

type conversationTaskContinuationCursor struct {
	createdAt time.Time
	id        string
}

// ConversationTaskContinuationReconciler visits one indexed page per scope and
// pass. Its cursor is a scheduling hint: all eligibility and promotion decisions
// are repeated atomically against durable state, including after a restart.
type ConversationTaskContinuationReconciler struct {
	store   ConversationTaskContinuationStore
	mu      sync.Mutex
	cursors map[Scope]conversationTaskContinuationCursor
}

func NewConversationTaskContinuationReconciler(store ConversationTaskContinuationStore) (*ConversationTaskContinuationReconciler, error) {
	if store == nil {
		return nil, errors.New("conversation task continuation storage is required")
	}
	return &ConversationTaskContinuationReconciler{store: store, cursors: make(map[Scope]conversationTaskContinuationCursor)}, nil
}

func (r *ConversationTaskContinuationReconciler) ReconcileScope(ctx context.Context, scope Scope, now time.Time) ([]*ConversationTaskResult, error) {
	if r == nil || r.store == nil || now.IsZero() {
		return nil, ErrInvalidConversationTask
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	const pageSize = 100
	filter := ConversationTaskDueFilter{Scope: scope, BeforeCreatedAt: now.Add(-ConversationTaskForegroundTimeout), Limit: pageSize}
	if cursor := r.cursors[scope]; cursor.id != "" {
		filter.AfterCreatedAt = &cursor.createdAt
		filter.AfterID = cursor.id
	}
	runs, err := r.store.ListDueConversationTaskRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	results := make([]*ConversationTaskResult, 0, len(runs))
	for _, run := range runs {
		if run == nil {
			continue
		}
		result, err := r.store.PromoteConversationTask(ctx, ConversationTaskPromotionRequest{
			Scope: scope, RunID: run.ID, Now: now, MinimumAge: ConversationTaskForegroundTimeout,
		})
		if err != nil {
			return results, err
		}
		if result != nil {
			results = append(results, result)
		}
		r.cursors[scope] = conversationTaskContinuationCursor{createdAt: run.CreatedAt, id: run.ID}
	}
	if len(runs) < pageSize {
		delete(r.cursors, scope)
	}
	return results, nil
}

// canonicalConversationTaskContinuation proves a task lane using the ledger,
// never a Run context marker supplied by an adapter or a model.
func canonicalConversationTaskContinuation(ctx context.Context, store interface{}, run *AgentRun) (*ConversationTask, error) {
	if run == nil {
		return nil, ErrInvalidAgentRun
	}
	taskID, _ := run.Context[ConversationTaskContextKey].(string)
	if taskID == "" {
		return nil, nil
	}
	tasks, ok := store.(ConversationTaskStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	task, err := tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.Mode != ConversationTaskModeContinuation {
		return nil, ErrInvalidConversationTask
	}
	proof, ok := store.(ConversationTaskProofStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	verified, err := VerifyConversationTaskWorkRun(ctx, proof, task, run)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, ErrInvalidConversationTask
	}
	return task, nil
}

// conversationTaskCoordinationRebase admits message drift for already accepted
// continuation work while retaining the original participant proposals. Every
// configuration revision remains a conflict, even if settings were reverted.
func conversationTaskCoordinationRebase(ctx context.Context, store ConversationStore, before, current *Conversation, runID, triggerID string) (*ConversationTask, error) {
	if before == nil || current == nil || runID == "" || current.Revision < before.Revision ||
		current.LastSequence < before.LastSequence || current.Revision-before.Revision != current.LastSequence-before.LastSequence {
		return nil, nil
	}
	previousSettings, currentSettings := cloneConversation(before), cloneConversation(current)
	previousSettings.Revision, currentSettings.Revision = 0, 0
	previousSettings.LastSequence, currentSettings.LastSequence = 0, 0
	previousSettings.UpdatedAt, currentSettings.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(previousSettings, currentSettings) {
		return nil, nil
	}
	runs, ok := store.(PortfolioStore)
	if !ok {
		return nil, nil
	}
	run, err := runs.GetAgentRun(ctx, current.Scope, runID)
	if err != nil || run == nil {
		return nil, err
	}
	task, err := canonicalConversationTaskContinuation(ctx, store, run)
	if err != nil || task == nil {
		return nil, err
	}
	if isTerminalAgentRunStatus(run.Status) || task.ConversationID != current.ID || task.SourceMessageID != triggerID || task.Owner != current.Owner {
		return nil, nil
	}
	ack, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, "conversation-task-handoff:"+task.ID)
	if err != nil {
		return nil, err
	}
	if ack == nil || ack.ID != ConversationTaskAcknowledgmentMessageID(task) || ack.ReplyToMessageID != task.SourceMessageID ||
		ack.ThreadRootID != task.ThreadRootID || ack.Content != task.Acknowledgment || ack.Sequence > current.LastSequence {
		return nil, nil
	}
	return task, nil
}
