package runtime

import (
	"github.com/axiom-studio/openseal/pkg/skill"
)

// DeclinedRoutineBriefReply is the calm reply after the person declines a
// routine brief. Declining is a decision, not a failure: the Run completes.
const DeclinedRoutineBriefReply = "Okay, I didn't set up the routine."

// SupersededApprovalInstruction tells the Agent how to continue after the
// person answered any other pending approval with a chat message. The action
// was not carried out; the message never approves it.
const SupersededApprovalInstruction = "The person replied with a message instead of approving your proposed action, so it was not carried out and its approval is closed (continuationCheckpoint.lastAction holds its exact arguments). Their message is in followUpMessages; a message never approves an action. Do what their message asks first. If the original request still stands, propose the action again through the normal governed action path, with any change they asked for, once whatever they asked first is done or in the same Turn when nothing blocks it. If they no longer want it, do not propose it again; reply briefly. Never propose a payment again unless their message explicitly asks for it."

// SupersededRoutineBriefInstruction tells the Agent how to continue after the
// person answered a pending routine brief with a chat message.
const SupersededRoutineBriefInstruction = "The person replied to your routine brief with a message instead of approving it, so that brief is closed and was not created (continuationCheckpoint.lastAction holds its exact arguments). Their message is in followUpMessages. Propose openseal.runbooks.create_task again in this Turn with their change applied (for example a different time or timezone), keeping the title, goal, schedule, brief and grants they did not ask to change. If their message instead says they no longer want the routine, do not propose it again; reply briefly. If it asks something unrelated, answer it and propose the unchanged brief again."

// DeclinedRoutineBriefInstruction applies when a declined brief resumes a Run
// that also has newer messages, so the model answers instead of the kernel.
const DeclinedRoutineBriefInstruction = "The person declined your routine brief (continuationCheckpoint.lastAction); the routine was not created. This is their decision, not a failure: do not apologize, retry or propose the routine again unless a newer message explicitly asks for it. Answer any other request in followUpMessages; otherwise reply only: " + DeclinedRoutineBriefReply

// RoutineTimezoneInstruction keeps routine times in the person's timezone.
// The runtime does not know where the person is, so the Agent must not guess.
const RoutineTimezoneInstruction = "A routine runs in the IANA timezone you pass to openseal.runbooks.create_task. The person's timezone is not supplied to you; never assume one (not Europe/London, UTC or the server's). Use the timezone they stated or that this conversation clearly establishes, for example a city or zone they named (IST is Asia/Kolkata). When timing depends on a timezone you do not know, ask one short question before proposing. Always name the time and timezone in brief.summary, for example \"every day at 8:00 AM India time\"."

// RoutineDeliveryInstruction tells the Agent where a routine's results go
// and how a brief grants posting anywhere else (create_task 1.7.0).
const RoutineDeliveryInstruction = "Each run of a routine posts its result automatically as a new message in the chat it was created from (for a Slack conversation, a new top-level message there), so a routine that reports here, including one asked to post in this same channel, needs no send grant. To post to another channel, grant that sending action in brief.grants with toDestinations listing each channel exactly as the runs will pass it (its channel ID, or #name), and name the channel in the label, for example \"Post to #ai-news\". Posts to any other channel still ask first."

// routineBindingToolGrantInstruction tells the Agent how a routine brief
// grants a connected tool, or is empty when no binding tool is offered. A
// connected tool is granted by its own action name; the action it runs
// through is never grantable.
func routineBindingToolGrantInstruction(actions []skill.ModelAction) string {
	for _, action := range actions {
		if action.BindingTool != nil {
			return "To let a routine use a connected tool without asking, list that tool in brief.grants by its own exact action name, for example " + action.Name +
				". Grant only the specific tools each run needs; " + action.SkillID + "." + action.Action + " itself cannot be granted."
		}
	}
	return ""
}

func routineBriefAction(skillID, action string) bool {
	return skillID == RunbookManagementSkillID && action == RunbookActionCreateTask
}

// declinedRoutineBrief reports a routine brief the person declined.
func declinedRoutineBrief(skillID, action, approvalStatus string) bool {
	return routineBriefAction(skillID, action) && approvalStatus == string(ApprovalStatusRejected)
}
