package runtime

import (
	"context"
	"strings"
)

func conversationTaskForReport(ctx context.Context, store ConversationStore, run *AgentRun) (*ConversationTask, bool, error) {
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
	sourceReport := sourceTaskID != ""
	if sourceReport {
		task, err = tasks.GetConversationTask(ctx, run.Scope, sourceTaskID)
	} else {
		task, err = tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	}
	if err != nil {
		return nil, false, err
	}
	if task == nil || task.Validate() != nil || task.Scope != run.Scope || task.Owner != run.Owner || task.TargetAgentID != run.AssignedAgentID ||
		run.Context[conversationRunContextConversationID] != task.ConversationID || run.Context[conversationRunContextTriggerID] != task.SourceMessageID {
		return nil, false, ErrInvalidConversationTask
	}
	if sourceReport {
		if run.ID != task.SourceRunID || run.Kind != RunKindConversation || run.Source != RunSourceChat || run.Output["summary"] != task.Acknowledgment || run.Output["conversationTaskWorkRunId"] != task.WorkRunID {
			return nil, false, ErrInvalidConversationTask
		}
	} else if taskID != task.ID || run.ID != task.WorkRunID || run.Kind != RunKindAgentWork || run.ParentRunID != "" || run.RootRunID != run.ID || run.ConcurrencyKey != "task:"+task.ID {
		return nil, false, ErrInvalidConversationTask
	}
	return task, sourceReport, nil
}

func conversationTaskReportContent(task *ConversationTask, source bool, run *AgentRun, fallback string) string {
	if source {
		return task.Acknowledgment
	}
	if run.Status == AgentRunStatusCompleted {
		for _, key := range []string{"report", "summary"} {
			if answer, ok := run.Output[key].(string); ok && strings.TrimSpace(answer) != "" {
				return strings.TrimSpace(answer)
			}
		}
	}
	return fallback
}
