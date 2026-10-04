package runtime

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// TurnTaskProposal asks the kernel to admit independent work from a foreground
// conversation. The model describes intent only; the kernel owns actor,
// conversation, target deployment, and execution identity.
type TurnTaskProposal struct {
	TaskKey        string        `json:"taskKey"`
	Goal           string        `json:"goal"`
	Acknowledgment string        `json:"acknowledgment"`
	Budget         *BudgetPolicy `json:"budget,omitempty"`
}

func (p *TurnTaskProposal) Validate() error {
	if p == nil || !validOpaqueIdentifier(p.TaskKey, 128) ||
		strings.TrimSpace(p.Goal) == "" || len(p.Goal) > 16384 ||
		strings.TrimSpace(p.Acknowledgment) == "" || len(p.Acknowledgment) > 600 {
		return errors.New("task proposal requires a bounded task key, goal, and acknowledgment")
	}
	if p.Budget != nil {
		return p.Budget.Validate()
	}
	return nil
}

func cloneTurnTaskProposal(proposal *TurnTaskProposal) *TurnTaskProposal {
	if proposal == nil {
		return nil
	}
	copy := *proposal
	copy.Budget = cloneBudgetPolicy(proposal.Budget)
	return &copy
}

// ConversationTaskSnapshot is the small model-visible projection of one
// durable task, without source authority or credentials.
type ConversationTaskSnapshot struct {
	TaskID            string               `json:"taskId"`
	Mode              ConversationTaskMode `json:"mode,omitempty"`
	WorkRunID         string               `json:"workRunId"`
	Goal              string               `json:"goal"`
	Status            AgentRunStatus       `json:"status"`
	Revision          int64                `json:"revision"`
	AvailableControls []string             `json:"availableControls,omitempty"`
	CreatedAt         time.Time            `json:"createdAt"`
}

// HostedConversationTaskContext is supplied by the trusted host after resolving
// canonical conversation state. CanStart is a model affordance; admission
// independently checks the persisted foreground Run and exact conversation.
type HostedConversationTaskContext struct {
	ConversationID string                     `json:"conversationId"`
	ThreadRootID   string                     `json:"threadRootId,omitempty"`
	CanStart       bool                       `json:"canStart"`
	Tasks          []ConversationTaskSnapshot `json:"tasks,omitempty"`
}

func (value *HostedConversationTaskContext) Validate() error {
	if value == nil {
		return nil
	}
	if !validOpaqueIdentifier(value.ConversationID, 128) || value.ThreadRootID != "" && !validOpaqueIdentifier(value.ThreadRootID, 128) || len(value.Tasks) > 10 {
		return errors.New("hosted conversation task context requires an exact conversation and at most ten task snapshots")
	}
	seen := make(map[string]bool, len(value.Tasks))
	for _, snapshot := range value.Tasks {
		if !validOpaqueIdentifier(snapshot.TaskID, 128) || !validOpaqueIdentifier(snapshot.WorkRunID, 128) ||
			(snapshot.Mode != ConversationTaskModeIndependent && snapshot.Mode != ConversationTaskModeContinuation) ||
			strings.TrimSpace(snapshot.Goal) == "" || !utf8.ValidString(snapshot.Goal) || utf8.RuneCountInString(snapshot.Goal) > 512 ||
			!validConversationTaskSnapshotStatus(snapshot.Status) || snapshot.Revision < 1 || snapshot.CreatedAt.IsZero() || seen[snapshot.TaskID] {
			return errors.New("hosted conversation task snapshot is invalid or exceeds its bounded goal summary")
		}
		seen[snapshot.TaskID] = true
		seenControls := make(map[string]bool, len(snapshot.AvailableControls))
		for _, control := range snapshot.AvailableControls {
			if control != RunActionCancel && control != RunActionPause && control != RunActionResume || seenControls[control] {
				return errors.New("hosted conversation task snapshot contains an invalid run control")
			}
			seenControls[control] = true
		}
	}
	return nil
}

func cloneHostedConversationTaskContext(value *HostedConversationTaskContext) *HostedConversationTaskContext {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Tasks = append([]ConversationTaskSnapshot(nil), value.Tasks...)
	for index := range copy.Tasks {
		copy.Tasks[index].AvailableControls = append([]string(nil), value.Tasks[index].AvailableControls...)
	}
	return &copy
}

func validateTaskProposalOutput(proposal *TurnTaskProposal, status AgentRunStatus, wake *WakeCondition, summary string, output map[string]interface{}, runError string) error {
	if err := proposal.Validate(); err != nil {
		return err
	}
	if status != AgentRunStatusRunning || wake != nil {
		return errors.New("a proposed task must leave the foreground Run running until durable admission")
	}
	if strings.TrimSpace(summary) != "" || len(output) != 0 || strings.TrimSpace(runError) != "" {
		return errors.New("a proposed task acknowledgment must appear only in proposedTask until durable admission")
	}
	return nil
}

func validateHostedTaskAuthority(run *AgentRun, taskContext *HostedConversationTaskContext) error {
	if run == nil || run.Kind != RunKindConversation || strings.TrimSpace(run.ParentRunID) != "" ||
		run.Context[ConversationTaskContextKey] != nil || taskContext == nil || !taskContext.CanStart || !validOpaqueIdentifier(taskContext.ConversationID, 256) {
		return errors.New("independent task admission is not available to this hosted Turn")
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	threadRootID, _ := run.Context["threadRootMessageId"].(string)
	if strings.TrimSpace(conversationID) != taskContext.ConversationID || !validOpaqueIdentifier(triggerID, 256) ||
		strings.TrimSpace(threadRootID) != strings.TrimSpace(taskContext.ThreadRootID) {
		return errors.New("hosted task context does not match the foreground conversation")
	}
	return nil
}

func hostedTaskForegroundSource(input TurnExecutionContext) *AgentRun {
	if input.ForegroundConversation == nil {
		return input.Run
	}
	source := input.ForegroundConversation
	if input.Run == nil || source.ID != input.Run.ID || source.Scope != input.Run.Scope || source.Owner != input.Run.Owner ||
		source.ParentRunID != input.Run.ParentRunID {
		return nil
	}
	for _, key := range []string{conversationRunContextConversationID, conversationRunContextTriggerID, "threadRootMessageId"} {
		sourceValue, sourceString := source.Context[key].(string)
		runValue, runString := input.Run.Context[key].(string)
		if sourceString != runString || sourceValue != runValue || !sourceString && (source.Context[key] != nil || input.Run.Context[key] != nil) {
			return nil
		}
	}
	return source
}

func validConversationTaskSnapshotStatus(status AgentRunStatus) bool {
	switch status {
	case AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusPaused, AgentRunStatusSleeping,
		AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent, AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent,
		AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled:
		return true
	default:
		return false
	}
}
