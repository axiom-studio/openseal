package runtime

import (
	"context"
	"errors"
)

// DeclinedRoutineBriefReply is the calm reply after the person declines a
// routine brief. Declining is a decision, not a failure: the Run completes.
const DeclinedRoutineBriefReply = "Okay, I didn't set up the routine."

// SupersededRoutineBriefInstruction tells the Agent how to continue after the
// person answered a pending routine brief with a chat message.
const SupersededRoutineBriefInstruction = "The person replied to your routine brief with a message instead of approving it, so that brief is closed and was not created (continuationCheckpoint.lastAction holds its exact arguments). Their message is in followUpMessages. Propose openseal.runbooks.create_task again in this Turn with their change applied (for example a different time or timezone), keeping the title, goal, schedule, brief and grants they did not ask to change. If their message instead says they no longer want the routine, do not propose it again; reply briefly. If it asks something unrelated, answer it and propose the unchanged brief again."

// DeclinedRoutineBriefInstruction applies when a declined brief resumes a Run
// that also has newer messages, so the model answers instead of the kernel.
const DeclinedRoutineBriefInstruction = "The person declined your routine brief (continuationCheckpoint.lastAction); the routine was not created. This is their decision, not a failure: do not apologize, retry or propose the routine again unless a newer message explicitly asks for it. Answer any other request in followUpMessages; otherwise reply only: " + DeclinedRoutineBriefReply

// RoutineTimezoneInstruction keeps routine times in the person's timezone.
// The runtime does not know where the person is, so the Agent must not guess.
const RoutineTimezoneInstruction = "A routine runs in the IANA timezone you pass to openseal.runbooks.create_task. The person's timezone is not supplied to you; never assume one (not Europe/London, UTC or the server's). Use the timezone they stated or that this conversation clearly establishes, for example a city or zone they named (IST is Asia/Kolkata). When timing depends on a timezone you do not know, ask one short question before proposing. Always name the time and timezone in brief.summary, for example \"every day at 8:00 AM India time\"."

func routineBriefAction(skillID, action string) bool {
	return skillID == RunbookManagementSkillID && action == RunbookActionCreateTask
}

// declinedRoutineBrief reports a routine brief the person declined.
func declinedRoutineBrief(skillID, action, approvalStatus string) bool {
	return routineBriefAction(skillID, action) && approvalStatus == string(ApprovalStatusRejected)
}

// supersedeRoutineBriefForMessage closes the pending routine brief that the
// Run is waiting on when the person answers it with a chat message, which is
// already delivered to the Run as a follow-up. Only a create_task approval of
// this Run, still pending and created before the message, qualifies; any other
// approval keeps waiting for its decision. A decision that won the race (the
// person pressed Accept or Decline) is kept: the message then reaches the Run
// as an ordinary follow-up when it resumes. It returns the resolution, or nil
// when nothing was superseded.
func (s *ConversationRunScheduler) supersedeRoutineBriefForMessage(ctx context.Context, run *AgentRun, message *ChannelMessage) (*ApprovalResolutionResult, error) {
	if run == nil || message == nil || run.Status != AgentRunStatusWaitingForApproval || run.WakeCondition == nil ||
		run.WakeCondition.Type != "approval" || run.WakeCondition.Reference == "" || !conversationRunHasFollowUp(run, message.ID) {
		return nil, nil
	}
	approval, err := s.runs.store.GetApproval(ctx, run.Scope, run.WakeCondition.Reference)
	if errors.Is(err, ErrApprovalNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if approval == nil || approval.Status != ApprovalStatusPending || approval.RunID != run.ID || !approval.CreatedAt.Before(message.CreatedAt) {
		return nil, nil
	}
	call, err := s.runs.store.GetActionCall(ctx, run.Scope, approval.ActionCallID)
	if errors.Is(err, ErrActionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if call == nil || !routineBriefAction(call.SkillID, call.Action) || call.Status != ActionCallStatusWaitingApproval || call.ApprovalID != approval.ID {
		return nil, nil
	}
	result, err := s.approvals.ResolveSupersededByMessage(ctx, run.Scope, approval.ID, approval.Revision, message.ID)
	if errors.Is(err, ErrApprovalResolved) || errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidRunTransition) || errors.Is(err, ErrApprovalExpired) {
		return nil, nil
	}
	if err != nil || result == nil || !result.Resolved {
		return nil, err
	}
	return result, nil
}
