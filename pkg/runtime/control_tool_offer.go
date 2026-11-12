package runtime

import "github.com/axiom-studio/openseal/pkg/runtime/agentrun"

// controlToolOffered is the authority rule for each tool. Decision and review
// Runs are offered only their own final tool, whatever else is set.
func controlToolOffered(name ControlToolName, a agentrun.ControlAuthority) bool {
	mode := a.EffectiveFinalMode()
	if mode == agentrun.FinalModeDecision {
		return name == ControlToolRequestDecision
	}
	if mode == agentrun.FinalModeReview {
		return name == ControlToolCompletionReview
	}
	switch name {
	case ControlToolFinalAnswer:
		return true
	case ControlToolRequestDecision, ControlToolCompletionReview:
		return false
	case ControlToolReportProgress:
		return a.CanReportProgress
	case ControlToolWait:
		return a.CanWait
	case ControlToolAskUser:
		return a.CanAskUser
	case ControlToolReact:
		return a.CanReact
	case ControlToolStartTask:
		return a.TaskStart != nil && a.TaskStart.CanStart && !a.VoiceCallStarted
	case ControlToolFork:
		return a.ForkAllowed
	case ControlToolRunOperation:
		return len(a.Operations) > 0
	case ControlToolDelegate, ControlToolDelegateReply:
		return len(a.DelegateTargets) > 0
	case ControlToolRunbookFork, ControlToolRunbookDelegate:
		return false // kernel-only: runbook StepProducer calls carry a directive
	}
	return false
}
