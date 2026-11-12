package runtime

import (
	"context"
	"errors"
)

// VerifyConversationTaskWorkRun binds a loaded Task to its canonical work,
// original actor/message/thread and any later governed Team assignment. The
// structural matcher alone never grants execution or viewer authority.
func VerifyConversationTaskWorkRun(ctx context.Context, store ConversationTaskProofStore, task *ConversationTask, run *AgentRun) (bool, error) {
	if store == nil || !ConversationTaskMatchesWorkRun(task, run) {
		return false, nil
	}
	persisted, err := store.FindConversationTaskByWorkRunID(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return false, err
	}
	if persisted == nil || !sameConversationTask(persisted, task) {
		return false, nil
	}
	canonical, err := store.GetAgentRun(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return false, err
	}
	if !ConversationTaskMatchesWorkRun(task, canonical) || canonical.AssignedAgentID != run.AssignedAgentID {
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
	var round *ParticipationRound
	if conversationTaskContinuationNeedsAssignmentProof(task, source) {
		result, err := store.FindParticipationRoundByIdempotencyKey(ctx, task.Scope, task.ConversationID, conversationTaskLedgerParticipationRoundKey(task))
		if err != nil {
			return false, err
		}
		if result != nil {
			round = result.Round
		}
	}
	return validConversationTaskAdmissionSourceWithRound(source, conversation, message, task, round), nil
}

// Scheduling provenance classifies legitimate original delegated work while
// keeping each child's existing owner, deployment and grants authoritative.
// It is deliberately separate from the same-Agent task fork authority.
func persistedConversationTaskSchedulingOrigin(ctx context.Context, store PortfolioStore, run *AgentRun, getRequest func(context.Context, Scope, string) (*AgentRequest, error)) (*ConversationTask, error) {
	tasks, ok := store.(ConversationTaskStore)
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
	proof, ok := store.(ConversationTaskProofStore)
	if !ok {
		return nil, ErrInvalidConversationTask
	}
	valid, err := VerifyConversationTaskWorkRun(ctx, proof, task, root)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidConversationTask
	}
	valid, err = conversationTaskAdmissionLineage(ctx, run.Scope, task, run, func(id string) (*AgentRun, error) {
		return store.GetAgentRun(ctx, run.Scope, id)
	}, getRequest)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidConversationTask
	}
	return task, nil
}

var (
	ErrConversationTaskNotFound = errors.New("conversation task not found")
)

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
