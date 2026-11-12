package runtime

// controlToolOffered is the authority rule for each tool. Decision and review
// Runs are offered only their own final tool, whatever else is set.
func controlToolOffered(name ControlToolName, a ControlAuthority) bool {
	mode := a.finalMode()
	if mode == FinalModeDecision {
		return name == ControlToolRequestDecision
	}
	if mode == FinalModeReview {
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
