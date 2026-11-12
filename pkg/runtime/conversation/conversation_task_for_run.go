package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TaskForReport(ctx context.Context, store models.ConversationStore, run *models.AgentRun) (*models.ConversationTask, bool, error) {
	if run == nil {
		return nil, false, nil
	}
	taskID, _ := run.Context[models.ConversationTaskContextKey].(string)
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
	tasks, ok := store.(models.ConversationTaskStore)
	if !ok {
		return nil, false, models.ErrInvalidConversationTask
	}
	var task *models.ConversationTask
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
	if task == nil || task.Validate() != nil || task.Scope != run.Scope || task.Owner != run.Owner || task.Mode != models.ConversationTaskModeContinuation && task.TargetAgentID != run.AssignedAgentID ||
		run.Context[models.ConversationRunConversationIDContextKey] != task.ConversationID || run.Context[models.ConversationRunTriggerIDContextKey] != task.SourceMessageID {
		return nil, false, models.ErrInvalidConversationTask
	}
	if task.Mode == models.ConversationTaskModeContinuation {
		proof, ok := store.(models.ConversationTaskProofStore)
		if !ok {
			return nil, false, models.ErrInvalidConversationTask
		}
		valid, err := agentrun.VerifyConversationTaskWorkRun(ctx, proof, task, run)
		if err != nil {
			return nil, false, err
		}
		if !valid {
			return nil, false, models.ErrInvalidConversationTask
		}
		return task, false, nil
	}
	if sourceReport {
		if run.ID != task.SourceRunID || run.Kind != models.RunKindConversation || run.Source != models.RunSourceChat || run.Output["summary"] != task.Acknowledgment || run.Output["conversationTaskWorkRunId"] != task.WorkRunID {
			return nil, false, models.ErrInvalidConversationTask
		}
	} else if taskID != task.ID || !models.ConversationTaskMatchesWorkRun(task, run) {
		return nil, false, models.ErrInvalidConversationTask
	}
	return task, sourceReport, nil
}
