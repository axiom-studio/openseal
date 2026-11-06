package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

// Task context is a lookup hint. Only the canonical immutable task aggregate,
// source message/actor/thread, settled source turn and exact work lineage can
// route an account setup back to its originating conversation. The run's
// acting member is member and its umbrella must own the conversation.
func (d *SkillBindingActionDispatcher) skillSetupConversationOrigin(ctx context.Context, run *AgentRun, member string, umbrella ObjectiveOwner) (string, string, error) {
	return skillSetupConversationOrigin(ctx, d.store, run, member, umbrella)
}

// skillSetupConversationOrigin returns the conversation and trigger message a
// run's in-chat card belongs to, or an error when the origin is unproven.
func skillSetupConversationOrigin(ctx context.Context, store KernelStore, run *AgentRun, member string, umbrella ObjectiveOwner) (string, string, error) {
	denied := errors.New("Skill setup requires a verified conversation origin")
	if run == nil || run.Owner != umbrella {
		return "", "", denied
	}
	_, taskHint := run.Context[ConversationTaskContextKey]
	if run.Kind == RunKindConversation && !taskHint && !strings.HasPrefix(run.ConcurrencyKey, "task:") {
		conversationID, _ := run.Context[conversationRunContextConversationID].(string)
		messageID, _ := run.Context[conversationRunContextTriggerID].(string)
		if conversationID == "" || messageID == "" {
			return "", "", denied
		}
		// A chat Run carries its conversation's owner, checked above.
		return conversationID, messageID, nil
	}
	proof, ok := store.(ConversationTaskProofStore)
	if !ok {
		return "", "", denied
	}
	canonical, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return "", "", err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.ID != run.ID || canonical.AssignedAgentID != member || canonical.Owner != run.Owner {
		return "", "", denied
	}
	task, err := persistedTaskWorkOrigin(ctx, store, canonical)
	if err != nil {
		return "", "", err
	}
	if task == nil {
		return "", "", denied
	}
	root, err := store.GetAgentRun(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return "", "", err
	}
	verified, err := VerifyConversationTaskWorkRun(ctx, proof, task, root)
	if err != nil {
		return "", "", err
	}
	if !verified || task.TargetAgentID != member || task.Owner != umbrella {
		return "", "", denied
	}
	conversation, err := proof.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return "", "", err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive || conversation.Owner != task.Owner {
		return "", "", denied
	}
	onRoster, err := AgentOnChatRoster(ctx, teamRosterReader(store), conversation.Scope, conversation.Owner, member)
	if err != nil {
		return "", "", err
	}
	if !onRoster {
		return "", "", denied
	}
	if task.Mode == ConversationTaskModeIndependent {
		turn, err := store.GetAgentTurn(ctx, task.Scope, task.SourceTurnID)
		if err != nil {
			return "", "", err
		}
		if turn == nil || turn.ID != task.SourceTurnID || turn.Scope != task.Scope || turn.RunID != task.SourceRunID || turn.Sequence != task.SourceTurnNumber || turn.Status != AgentTurnStatusCompleted ||
			turn.RequestedTask == nil || turn.RequestedTask.TaskKey != task.TaskKey || turn.RequestedTask.Goal != task.Goal ||
			turn.RequestedTask.Acknowledgment != task.Acknowledgment || !reflect.DeepEqual(turn.RequestedTask.Budget, task.RequestedBudget) {
			return "", "", denied
		}
	}
	return task.ConversationID, task.SourceMessageID, nil
}
