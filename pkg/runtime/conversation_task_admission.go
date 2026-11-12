package runtime

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
)

// VerifyConversationTaskWorkRun binds a loaded Task to its canonical work,
// original actor/message/thread and any later governed Team assignment. The
// structural matcher alone never grants execution or viewer authority.
func VerifyConversationTaskWorkRun(ctx context.Context, store models.ConversationTaskProofStore, task *models.ConversationTask, run *models.AgentRun) (bool, error) {
	if store == nil || !models.ConversationTaskMatchesWorkRun(task, run) {
		return false, nil
	}
	persisted, err := store.FindConversationTaskByWorkRunID(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return false, err
	}
	if persisted == nil || !models.SameConversationTask(persisted, task) {
		return false, nil
	}
	canonical, err := store.GetAgentRun(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return false, err
	}
	if !models.ConversationTaskMatchesWorkRun(task, canonical) || canonical.AssignedAgentID != run.AssignedAgentID {
		return false, nil
	}
	source := canonical
	if task.SourceRunID != canonical.ID {
		source, err = store.GetAgentRun(ctx, task.Scope, task.SourceRunID)
		if err != nil {
			return false, err
		}
	}
	conversation, err := store.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return false, err
	}
	message, err := store.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return false, err
	}
	var round *models.ParticipationRound
	if models.ConversationTaskContinuationNeedsAssignmentProof(task, source) {
		result, err := store.FindParticipationRoundByIdempotencyKey(ctx, task.Scope, task.ConversationID, models.ConversationTaskLedgerParticipationRoundKey(task))
		if err != nil {
			return false, err
		}
		if result != nil {
			round = result.Round
		}
	}
	return models.IsValidConversationTaskAdmissionSourceWithRound(source, conversation, message, task, round), nil
}

// Scheduling provenance classifies legitimate original delegated work while
// keeping each child's existing owner, deployment and grants authoritative.
// It is deliberately separate from the same-Agent task fork authority.
func persistedConversationTaskSchedulingOrigin(ctx context.Context, store models.PortfolioStore, run *models.AgentRun, getRequest func(context.Context, models.Scope, string) (*models.AgentRequest, error)) (*models.ConversationTask, error) {
	tasks, ok := store.(models.ConversationTaskStore)
	if !ok || run == nil {
		return nil, nil
	}
	rootID := run.RootRunID
	if rootID == "" {
		rootID = run.ID
	}
	task, err := tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, rootID)
	if errors.Is(err, ErrConversationTaskNotFound) {
		return nil, nil
	}
	if err != nil || task == nil {
		return nil, err
	}
	root, err := store.GetAgentRun(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return nil, err
	}
	proof, ok := store.(models.ConversationTaskProofStore)
	if !ok {
		return nil, models.ErrInvalidConversationTask
	}
	valid, err := VerifyConversationTaskWorkRun(ctx, proof, task, root)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, models.ErrInvalidConversationTask
	}
	valid, err = models.ConversationTaskAdmissionLineage(ctx, run.Scope, task, run, func(id string) (*models.AgentRun, error) {
		return store.GetAgentRun(ctx, run.Scope, id)
	}, getRequest)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, models.ErrInvalidConversationTask
	}
	return task, nil
}

var (
	ErrConversationTaskNotFound = errors.New("conversation task not found")
)

// PersistedTaskOrigin resolves an exact independent work identity. Caller
// context is only a hint; hosting applications must verify canonical source
// conversation/message/actor and their existing authorization policy.
func PersistedTaskOrigin(ctx context.Context, store models.ConversationTaskStore, scope models.Scope, workRunID string) (*models.ConversationTask, error) {
	if store == nil {
		return nil, models.ErrInvalidConversationTask
	}
	task, err := store.FindConversationTaskByWorkRunID(ctx, scope, workRunID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrConversationTaskNotFound
	}
	if task.Scope != scope || task.WorkRunID != workRunID || task.Validate() != nil {
		return nil, models.ErrInvalidConversationTask
	}
	return task, nil
}
