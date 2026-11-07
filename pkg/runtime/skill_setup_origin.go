package runtime

import (
	"context"
	"errors"
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
		ok, err := independentTaskStartedByStep(ctx, store, task)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", denied
		}
	}
	return task.ConversationID, task.SourceMessageID, nil
}

// independentTaskStartedByStep proves an independent task was admitted from
// its source Run's completed step through that step's completed
// conversation_start_task tool entry (section 04 §7.6): the entry's decoded
// goal and acknowledgment are the task's, and the task key derives from the
// entry id. Tasks without a source tool entry are unproven.
func independentTaskStartedByStep(ctx context.Context, store KernelStore, task *ConversationTask) (bool, error) {
	sessions, ok := store.(AgentSessionStore)
	if !ok || task.SourceToolEntryID == "" || task.SourceTurnID == "" {
		return false, nil
	}
	step, err := store.GetAgentTurn(ctx, task.Scope, task.SourceTurnID)
	if err != nil {
		return false, err
	}
	if step == nil || step.ID != task.SourceTurnID || step.Scope != task.Scope || step.RunID != task.SourceRunID ||
		step.Sequence != task.SourceTurnNumber || step.Status != AgentTurnStatusCompleted {
		return false, nil
	}
	entry, err := optionalSessionEntry(sessions.GetSessionEntry(ctx, task.Scope, task.SourceToolEntryID))
	if err != nil {
		return false, err
	}
	if entry == nil || entry.ID != task.SourceToolEntryID || entry.Type != SessionEntryTool || entry.Tool == nil ||
		entry.RunID != task.SourceRunID || entry.StepID != task.SourceTurnID || entry.Step != task.SourceTurnNumber ||
		entry.Tool.Name != string(ControlToolStartTask) || entry.State != ToolEntryCompleted || startTaskKey(entry.ID) != task.TaskKey {
		return false, nil
	}
	args := startTaskArguments(entry)
	if args == nil || args.Goal != task.Goal || args.Acknowledgment != task.Acknowledgment {
		return false, nil
	}
	return true, nil
}
