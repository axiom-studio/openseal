package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func conversationTaskForReport(ctx context.Context, store ConversationStore, run *AgentRun) (*ConversationTask, bool, error) {
	if run == nil {
		return nil, false, nil
	}
	taskID, _ := run.Context[ConversationTaskContextKey].(string)
	sourceTaskID := ""
	switch ids := run.Output["conversationTaskIds"].(type) {
	case []string:
		if len(ids) == 1 {
			sourceTaskID = ids[0]
		}
	case []interface{}:
		if len(ids) == 1 {
			sourceTaskID, _ = ids[0].(string)
		}
	}
	if taskID == "" && sourceTaskID == "" {
		return nil, false, nil
	}
	tasks, ok := store.(ConversationTaskStore)
	if !ok {
		return nil, false, ErrInvalidConversationTask
	}
	var task *ConversationTask
	var err error
	sourceReport := taskID == "" && sourceTaskID != ""
	if sourceReport {
		task, err = tasks.GetConversationTask(ctx, run.Scope, sourceTaskID)
	} else {
		task, err = tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	}
	if err != nil {
		return nil, false, err
	}
	if task == nil || task.Validate() != nil || task.Scope != run.Scope || task.Owner != run.Owner || task.Mode != ConversationTaskModeContinuation && task.TargetAgentID != run.AssignedAgentID ||
		run.Context[conversationRunContextConversationID] != task.ConversationID || run.Context[conversationRunContextTriggerID] != task.SourceMessageID {
		return nil, false, ErrInvalidConversationTask
	}
	if task.Mode == ConversationTaskModeContinuation {
		proof, ok := store.(ConversationTaskProofStore)
		if !ok {
			return nil, false, ErrInvalidConversationTask
		}
		valid, err := agentrun.VerifyConversationTaskWorkRun(ctx, proof, task, run)
		if err != nil {
			return nil, false, err
		}
		if !valid {
			return nil, false, ErrInvalidConversationTask
		}
		return task, false, nil
	}
	if sourceReport {
		if run.ID != task.SourceRunID || run.Kind != RunKindConversation || run.Source != RunSourceChat || run.Output["summary"] != task.Acknowledgment || run.Output["conversationTaskWorkRunId"] != task.WorkRunID {
			return nil, false, ErrInvalidConversationTask
		}
	} else if taskID != task.ID || !ConversationTaskMatchesWorkRun(task, run) {
		return nil, false, ErrInvalidConversationTask
	}
	return task, sourceReport, nil
}
