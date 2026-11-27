package modelstep

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// ControlToolOffered is the authority rule for each tool. Decision Runs are
// offered only their own final tool, whatever else is set.
func ControlToolOffered(name models.ControlToolName, a agentrun.ControlAuthority) bool {
	mode := a.EffectiveFinalMode()
	if mode == agentrun.FinalModeDecision {
		return name == models.ControlToolRequestDecision
	}
	switch name {
	case models.ControlToolRequestDecision:
		return false
	case models.ControlToolReportProgress:
		return a.CanReportProgress
	case models.ControlToolWait:
		return a.CanWait
	case models.ControlToolAskUser:
		return a.CanAskUser
	case models.ControlToolReact:
		return a.CanReact
	case models.ControlToolStartTask:
		return a.TaskStart != nil && a.TaskStart.CanStart && !a.VoiceCallStarted
	case models.ControlToolStartSubagents, models.ControlToolWaitSubagents, models.ControlToolReadSubagent,
		models.ControlToolMessageSubagent, models.ControlToolStopSubagents:
		return a.ForkAllowed
	case models.ControlToolRunOperation:
		return len(a.Operations) > 0
	case models.ControlToolDelegate, models.ControlToolDelegateReply:
		return len(a.DelegateTargets) > 0
	case models.ControlToolRunbookFork, models.ControlToolRunbookDelegate:
		return false // kernel-only: runbook StepProducer calls carry a directive
	}
	return false
}
