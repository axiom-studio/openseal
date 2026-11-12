package models

import (
	"context"
	"errors"
	"strings"
)

func ValidateConversationTaskForegroundReserve(reserve, maximum int) error {
	if reserve < 0 || reserve > 0 && (maximum <= 0 || reserve >= maximum) {
		return errors.New("conversation task foreground reserve must be nonnegative and smaller than the agent capacity")
	}
	return nil
}

// These lookups must share the claim's store lock or transaction. Neither a
// caller's context hint nor a root id alone grants task admission authority.
type ConversationTaskAdmissionLookup struct {
	Task         func(context.Context, Scope, string) (*ConversationTask, error)
	Run          func(context.Context, Scope, string) (*AgentRun, error)
	Conversation func(context.Context, Scope, string) (*Conversation, error)
	Message      func(context.Context, Scope, string, string) (*ChannelMessage, error)
	Request      func(context.Context, Scope, string) (*AgentRequest, error)
	Round        func(context.Context, Scope, string, string) (*ParticipationRound, error)
}

func PrepareConversationTaskAdmission(ctx context.Context, claim *AgentRunClaim, runs []*AgentRun, lookup ConversationTaskAdmissionLookup) error {
	claim.conversationTaskRuns = nil
	claim.InvalidConversationTaskRuns = nil
	if claim.ConversationTaskForegroundReserve == 0 {
		claim.ActiveConversationTaskRuns = nil
		return nil
	}
	if claim.ActiveConversationTaskRuns == nil {
		claim.ActiveConversationTaskRuns = make(map[string]bool)
	}
	claim.conversationTaskRuns = make(map[string]bool)
	claim.InvalidConversationTaskRuns = make(map[string]bool)
	tasks := make(map[string]*ConversationTask)
	checked := make(map[string]bool)
	persistedRuns := make(map[string]*AgentRun)
	for _, run := range runs {
		if run != nil && run.Scope == claim.Scope {
			persistedRuns[run.ID] = run
		}
	}
	getRun := func(id string) (*AgentRun, error) {
		if run, ok := persistedRuns[id]; ok {
			return run, nil
		}
		run, err := lookup.Run(ctx, claim.Scope, id)
		if errors.Is(err, ErrRunNotFound) {
			err = nil
		}
		if err == nil {
			persistedRuns[id] = run
		}
		return run, err
	}
	for _, candidate := range runs {
		if candidate == nil || candidate.Scope != claim.Scope {
			continue
		}
		if candidate.Status == AgentRunStatusRunning && candidate.LeaseExpiresAt != nil && candidate.LeaseExpiresAt.After(claim.Now) {
			if _, known := claim.ActiveConversationTaskRuns[candidate.ID]; known {
				continue // PostgreSQL diagnostics batch this metadata in SQL.
			}
		} else if !agentRunEligible(candidate, *claim) {
			continue
		}
		rootID := candidate.RootRunID
		if rootID == "" {
			rootID = candidate.ID
		}
		if !checked[rootID] {
			task, err := lookup.Task(ctx, claim.Scope, rootID)
			if err != nil {
				return err
			}
			tasks[rootID], checked[rootID] = task, true
		}
		task := tasks[rootID]
		_, hint := candidate.Context[ConversationTaskContextKey]
		taskLike := hint || strings.HasPrefix(candidate.ConcurrencyKey, "task:") || task != nil
		if candidate.Status == AgentRunStatusRunning && candidate.LeaseExpiresAt != nil && candidate.LeaseExpiresAt.After(claim.Now) {
			claim.ActiveConversationTaskRuns[candidate.ID] = taskLike
			continue // Counting a reserved marker can only restrict admission.
		}
		if !taskLike {
			continue
		}
		valid := task != nil && task.Validate() == nil && task.Scope == claim.Scope && task.WorkRunID == rootID &&
			task.ID == ConversationTaskID(task.Scope, task.SourceRunID, task.TaskKey)
		if valid {
			root, err := getRun(rootID)
			if err != nil {
				return err
			}
			valid = IsValidConversationTaskAdmissionRoot(root, task)
		}
		if valid {
			source, err := getRun(task.SourceRunID)
			if err != nil {
				return err
			}
			conversation, err := lookup.Conversation(ctx, claim.Scope, task.ConversationID)
			if err != nil {
				return err
			}
			message, err := lookup.Message(ctx, claim.Scope, task.ConversationID, task.SourceMessageID)
			if err != nil {
				return err
			}
			var round *ParticipationRound
			if ConversationTaskContinuationNeedsAssignmentProof(task, source) && lookup.Round != nil {
				round, err = lookup.Round(ctx, claim.Scope, task.ConversationID, ConversationTaskLedgerParticipationRoundKey(task))
				if err != nil {
					return err
				}
			}
			valid = IsValidConversationTaskAdmissionSourceWithRound(source, conversation, message, task, round)
		}
		if valid {
			var err error
			valid, err = ConversationTaskAdmissionLineage(ctx, claim.Scope, task, candidate, getRun, lookup.Request)
			if err != nil {
				return err
			}
		}
		if valid {
			claim.conversationTaskRuns[candidate.ID] = true
		} else {
			claim.InvalidConversationTaskRuns[candidate.ID] = true
		}
	}
	return nil
}

func ConversationTaskAdmissionLineage(ctx context.Context, scope Scope, task *ConversationTask, candidate *AgentRun, getRun func(string) (*AgentRun, error), getRequest func(context.Context, Scope, string) (*AgentRequest, error)) (bool, error) {
	seen := make(map[string]bool)
	current := candidate
	for depth := 0; depth < 64 && current != nil; depth++ {
		if seen[current.ID] || current.Scope != task.Scope || current.RootRunID != task.WorkRunID {
			return false, nil
		}
		seen[current.ID] = true
		if hinted, exists := current.Context[ConversationTaskContextKey]; exists && hinted != task.ID {
			return false, nil
		}
		if current.ID == task.WorkRunID {
			return ConversationTaskMatchesWorkRun(task, current), nil
		}
		if current.ParentRunID == "" {
			return false, nil
		}
		parent, err := getRun(current.ParentRunID)
		if err != nil {
			return false, err
		}
		if task.Mode == ConversationTaskModeIndependent {
			if current.Owner != task.Owner || current.AssignedAgentID != task.TargetAgentID || NormalizeRunKind(current.Kind) != RunKindAgentWork || current.Source != RunSourceFork {
				return false, nil
			}
		} else if !validConversationTaskContinuationChild(current, parent) {
			if getRequest == nil || current.Source != RunSourceRequest && current.Source != RunSourceHandoff && current.Source != RunSourceEscalation {
				return false, nil
			}
			collaboration, _ := current.Context["collaboration"].(map[string]interface{})
			requestID, _ := collaboration["requestId"].(string)
			if !IsValidOpaqueIdentifier(requestID, 128) {
				return false, nil
			}
			request, err := getRequest(ctx, scope, requestID)
			if err != nil {
				return false, err
			}
			if !validConversationTaskContinuationDelegation(current, parent, request) {
				return false, nil
			}
		}
		current = parent
	}
	return false, nil
}

func validConversationTaskContinuationChild(child, parent *AgentRun) bool {
	return child != nil && parent != nil && child.Scope == parent.Scope && child.ParentRunID == parent.ID && child.RootRunID == parent.RootRunID &&
		child.Source == RunSourceFork && (child.Kind == RunKindAgentWork || child.Kind == RunKindConversation) &&
		child.Owner == parent.Owner && child.AssignedAgentID == parent.AssignedAgentID
}

func validConversationTaskContinuationDelegation(child, parent *AgentRun, request *AgentRequest) bool {
	if child == nil || parent == nil || request == nil || request.Validate() != nil || child.Scope != parent.Scope || request.Scope != child.Scope ||
		child.ParentRunID != parent.ID || child.RootRunID != parent.RootRunID || request.SourceRunID != parent.ID || request.ChildRunID != child.ID ||
		request.AssignedAgentID != child.AssignedAgentID || request.Goal != child.Goal || !RequesterControlsRun(request.Requester, parent) ||
		child.Kind != RunKindAgentWork && child.Kind != RunKindConversation {
		return false
	}
	expectedSource := RunSourceRequest
	if request.Kind == AgentRequestKindHandoff {
		expectedSource = RunSourceHandoff
	} else if request.Kind == AgentRequestKindEscalation {
		expectedSource = RunSourceEscalation
	}
	return child.Source == expectedSource && child.Owner == CollaborationChildOwner(parent, request)
}

func IsValidConversationTaskAdmissionRoot(root *AgentRun, task *ConversationTask) bool {
	return ConversationTaskMatchesWorkRun(task, root) && (task.Mode == ConversationTaskModeContinuation || root.ObjectiveID == "")
}

func IsValidConversationTaskAdmissionSource(source *AgentRun, conversation *Conversation, message *ChannelMessage, task *ConversationTask) bool {
	return IsValidConversationTaskAdmissionSourceWithRound(source, conversation, message, task, nil)
}

func IsValidConversationTaskAdmissionSourceWithRound(source *AgentRun, conversation *Conversation, message *ChannelMessage, task *ConversationTask, round *ParticipationRound) bool {
	if source == nil || conversation == nil || message == nil || source.Scope != task.Scope || source.ID != task.SourceRunID ||
		source.Kind != RunKindConversation || source.Source != RunSourceChat || source.ParentRunID != "" || source.RootRunID != source.ID ||
		source.Owner != task.Owner || task.Mode != ConversationTaskModeContinuation && source.AssignedAgentID != task.TargetAgentID || source.Context[ConversationRunConversationIDContextKey] != task.ConversationID ||
		source.Context[ConversationRunTriggerIDContextKey] != task.SourceMessageID || conversation.Scope != task.Scope || conversation.ID != task.ConversationID ||
		conversation.Owner != task.Owner || message.Scope != task.Scope || message.ID != task.SourceMessageID || message.ConversationID != task.ConversationID {
		return false
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	thread := message.ThreadRootID
	if thread == "" {
		thread = message.ID
	}
	if actor != task.AuthenticatedActor || thread != task.ThreadRootID {
		return false
	}
	if task.Mode == ConversationTaskModeContinuation {
		storedThread, _ := source.Context["threadRootMessageId"].(string)
		// A Team channel's conversation Run only routes; it never acts for
		// an assigned member, so such a continuation cannot be proven.
		if ConversationTaskContinuationNeedsAssignmentProof(task, source) {
			return false
		}
		return ConversationTaskMatchesWorkRun(task, source) && storedThread == ExternalConversationThreadRoot(conversation, message) &&
			!task.CreatedAt.Before(*task.ForegroundDeadline)
	}
	return true
}

func ConversationTaskContinuationNeedsAssignmentProof(task *ConversationTask, run *AgentRun) bool {
	return task != nil && run != nil && task.Mode == ConversationTaskModeContinuation && task.Owner.Type == OwnerTypeTeam &&
		task.TargetAgentID == "" && run.AssignedAgentID != ""
}

func ConversationTaskLedgerParticipationRoundKey(task *ConversationTask) string {
	return "participation-round:" + SHA256DigestString(task.Scope.Kind+"\x00"+task.Scope.ID+"\x00"+task.ConversationID+"\x00"+task.SourceMessageID)
}

type ConversationTaskProofStore interface {
	GetAgentRun(context.Context, Scope, string) (*AgentRun, error)
	FindConversationTaskByWorkRunID(context.Context, Scope, string) (*ConversationTask, error)
	GetConversation(context.Context, Scope, string) (*Conversation, error)
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
	FindParticipationRoundByIdempotencyKey(context.Context, Scope, string, string) (*ParticipationRoundResult, error)
}

// ValidateConversationTaskStartAuthority reports whether run is a foreground
// conversation Run allowed to start independent work in taskContext's
// conversation and thread.
func ValidateConversationTaskStartAuthority(run *AgentRun, taskContext *HostedConversationTaskContext) error {
	if run == nil || run.Kind != RunKindConversation || strings.TrimSpace(run.ParentRunID) != "" ||
		run.Context[ConversationTaskContextKey] != nil || taskContext == nil || !taskContext.CanStart || !IsValidOpaqueIdentifier(taskContext.ConversationID, 256) {
		return errors.New("independent task admission is not available to this Run")
	}
	conversationID, _ := run.Context[ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[ConversationRunTriggerIDContextKey].(string)
	threadRootID, _ := run.Context["threadRootMessageId"].(string)
	if strings.TrimSpace(conversationID) != taskContext.ConversationID || !IsValidOpaqueIdentifier(triggerID, 256) ||
		strings.TrimSpace(threadRootID) != strings.TrimSpace(taskContext.ThreadRootID) {
		return errors.New("task context does not match the foreground conversation")
	}
	return nil
}
