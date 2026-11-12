package routine

import (
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// RoutineTimezoneInstruction keeps routine times in the person's timezone.
// The runtime does not know where the person is, so the Agent must not guess.
const RoutineTimezoneInstruction = "A routine runs in the IANA timezone you pass to openseal.runbooks.create_task. The person's timezone is not supplied to you; never assume one (not Europe/London, UTC or the server's). Use the timezone they stated or that this conversation clearly establishes, for example a city or zone they named (IST is Asia/Kolkata). When timing depends on a timezone you do not know, ask one short question before proposing. Always name the time and timezone in brief.summary, for example \"every day at 8:00 AM India time\"."

// RoutineDeliveryInstruction tells the Agent where a routine's results go
// and how a brief grants posting anywhere else (create_task 1.7.0).
const RoutineDeliveryInstruction = "Each run of a routine posts its result automatically as a new message in the chat it was created from (for a Slack conversation, a new top-level message there), so a routine that reports here, including one asked to post in this same channel, needs no send grant. To post to another channel, grant that sending action in brief.grants with toDestinations listing each channel exactly as the runs will pass it (its channel ID, or #name), and name the channel in the label, for example \"Post to #ai-news\". Posts to any other channel still ask first."

// BindingToolGrantInstruction tells the Agent how a routine brief
// grants a connected tool, or is empty when no binding tool is offered. A
// connected tool is granted by its own action name; the action it runs
// through is never grantable.
func BindingToolGrantInstruction(actions []skill.ModelAction) string {
	for _, action := range actions {
		if action.BindingTool != nil {
			return "To let a routine use a connected tool without asking, list that tool in brief.grants by its own exact action name, for example " + action.Name +
				". Grant only the specific tools each run needs; " + action.SkillID + "." + action.Action + " itself cannot be granted."
		}
	}
	return ""
}

func routineBriefAction(skillID, action string) bool {
	return skillID == models.RunbookManagementSkillID && action == runtimeaction.RunbookActionCreateTask
}

// declinedRoutineBrief reports a routine brief the person declined.
func declinedRoutineBrief(skillID, action, approvalStatus string) bool {
	return routineBriefAction(skillID, action) && approvalStatus == string(models.ApprovalStatusRejected)
}
