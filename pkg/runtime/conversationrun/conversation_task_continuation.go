package conversationrun

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Promote changes only the conversation Run's presentation and execution lane.
// The store commits the canonical Task and its acknowledgment together; callers
// must not announce a handoff until this operation returns a committed result.
func (s *ConversationTaskService) Promote(ctx context.Context, req models.ConversationTaskPromotionRequest) (*models.ConversationTaskResult, error) {
	if s == nil || s.store == nil {
		return nil, models.ErrInvalidConversationTask
	}
	store, ok := s.store.(models.ConversationTaskPromotionStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidOpaqueIdentifier(req.RunID, 128) || req.MinimumAge < 0 {
		return nil, models.ErrInvalidConversationTask
	}
	if req.Now.IsZero() {
		req.Now = s.now().UTC()
	}
	if req.MinimumAge == 0 {
		req.MinimumAge = models.ConversationTaskForegroundTimeout
	}
	return store.PromoteConversationTask(ctx, req)
}

type conversationTaskContinuationCursor struct {
	createdAt time.Time
	id        string
}

// ConversationTaskContinuationReconciler visits one indexed page per scope and
// pass. Its cursor is a scheduling hint: all eligibility and promotion decisions
// are repeated atomically against durable state, including after a restart.
type ConversationTaskContinuationReconciler struct {
	store   models.ConversationTaskContinuationStore
	mu      sync.Mutex
	cursors map[models.Scope]conversationTaskContinuationCursor
}

func NewConversationTaskContinuationReconciler(store models.ConversationTaskContinuationStore) (*ConversationTaskContinuationReconciler, error) {
	if store == nil {
		return nil, errors.New("conversation task continuation storage is required")
	}
	return &ConversationTaskContinuationReconciler{store: store, cursors: make(map[models.Scope]conversationTaskContinuationCursor)}, nil
}

func (r *ConversationTaskContinuationReconciler) ReconcileScope(ctx context.Context, scope models.Scope, now time.Time) ([]*models.ConversationTaskResult, error) {
	if r == nil || r.store == nil || now.IsZero() {
		return nil, models.ErrInvalidConversationTask
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	const pageSize = 100
	filter := models.ConversationTaskDueFilter{Scope: scope, BeforeCreatedAt: now.Add(-models.ConversationTaskForegroundTimeout), Limit: pageSize}
	if cursor := r.cursors[scope]; cursor.id != "" {
		filter.AfterCreatedAt = &cursor.createdAt
		filter.AfterID = cursor.id
	}
	runs, err := r.store.ListDueConversationTaskRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	results := make([]*models.ConversationTaskResult, 0, len(runs))
	for _, run := range runs {
		if run == nil {
			continue
		}
		result, err := r.store.PromoteConversationTask(ctx, models.ConversationTaskPromotionRequest{
			Scope: scope, RunID: run.ID, Now: now, MinimumAge: models.ConversationTaskForegroundTimeout,
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
func canonicalConversationTaskContinuation(ctx context.Context, store interface{}, run *models.AgentRun) (*models.ConversationTask, error) {
	if run == nil {
		return nil, models.ErrInvalidAgentRun
	}
	taskID, _ := run.Context[models.ConversationTaskContextKey].(string)
	if taskID == "" {
		return nil, nil
	}
	tasks, ok := store.(models.ConversationTaskStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	task, err := tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.Mode != models.ConversationTaskModeContinuation {
		return nil, models.ErrInvalidConversationTask
	}
	proof, ok := store.(models.ConversationTaskProofStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	verified, err := agentrun.VerifyConversationTaskWorkRun(ctx, proof, task, run)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, models.ErrInvalidConversationTask
	}
	return task, nil
}
