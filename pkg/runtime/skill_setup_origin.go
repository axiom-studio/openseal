package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

// Task context is a lookup hint. Only the canonical immutable task aggregate,
// source message/actor/thread, settled source turn and exact work lineage can
// route an account setup back to its originating Agent conversation.
func (d *SkillBindingActionDispatcher) skillSetupConversationOrigin(ctx context.Context, run *AgentRun, deploymentID string) (string, string, error) {
	denied := errors.New("Skill setup requires a verified Agent conversation origin")
	if run == nil {
		return "", "", denied
	}
	_, taskHint := run.Context[ConversationTaskContextKey]
	if run.Kind == RunKindConversation && !taskHint && !strings.HasPrefix(run.ConcurrencyKey, "task:") {
		conversationID, _ := run.Context[conversationRunContextConversationID].(string)
		messageID, _ := run.Context[conversationRunContextTriggerID].(string)
		if conversationID == "" || messageID == "" {
			return "", "", denied
		}
		return conversationID, messageID, nil
	}
	proof, ok := d.store.(ConversationTaskProofStore)
	if !ok {
		return "", "", denied
	}
	canonical, err := d.store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return "", "", err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.ID != run.ID || canonical.AssignedAgentID != deploymentID || canonical.Owner != run.Owner {
		return "", "", denied
	}
	task, err := persistedTaskWorkOrigin(ctx, d.store, canonical)
	if err != nil {
		return "", "", err
	}
	if task == nil {
		return "", "", denied
	}
	root, err := d.store.GetAgentRun(ctx, task.Scope, task.WorkRunID)
	if err != nil {
		return "", "", err
	}
	verified, err := VerifyConversationTaskWorkRun(ctx, proof, task, root)
	if err != nil {
		return "", "", err
	}
	if !verified || task.TargetAgentID != deploymentID || task.Owner.Type != OwnerTypeAgent || task.Owner.ID != deploymentID {
		return "", "", denied
	}
	conversation, err := proof.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return "", "", err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive || conversation.Owner != task.Owner {
		return "", "", denied
	}
	if task.Mode == ConversationTaskModeIndependent {
		turn, err := d.store.GetAgentTurn(ctx, task.Scope, task.SourceTurnID)
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
