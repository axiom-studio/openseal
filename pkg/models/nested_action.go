package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ActionOrigin records who requested an ActionCall.
type ActionOrigin string

// ActionOriginVM marks a nested call made during a step by code running in the
// chat's VM (for example `seal call` inside workspace.run_command). Its TurnID
// is the step (agent_turns row) whose run_command made the call.
const ActionOriginVM ActionOrigin = "vm"

var (
	// ErrNestedTurnNotActive reports that the step is not the Run's running
	// step hosted by this process.
	ErrNestedTurnNotActive = errors.New("step is not the run's running step")
	// ErrNestedActionLimit reports that a per-step nested call bound is reached.
	ErrNestedActionLimit = errors.New("nested action call limit reached for this step")
)

// NestedActionFence is checked by the store in the transaction that creates a
// nested ActionCall: the Run is running under WorkerID's live lease, the step
// is its running step under the same lease, and the step's bounds hold.
type NestedActionFence struct {
	WorkerID      string
	Now           time.Time
	MaxCalls      int
	MaxConcurrent int
}

// NestedActionTurnState is what a store observed inside its transaction.
type NestedActionTurnState struct {
	RunStatus       AgentRunStatus
	RunLeaseOwner   string
	RunLeaseExpiry  *time.Time
	Turn            *AgentTurn
	TotalCalls      int
	UnfinishedCalls int
}

func (f *NestedActionFence) Admit(call *ActionCall, state NestedActionTurnState) error {
	turn := state.Turn
	if state.RunStatus != AgentRunStatusRunning || state.RunLeaseOwner != f.WorkerID || state.RunLeaseExpiry == nil || !state.RunLeaseExpiry.After(f.Now) ||
		turn == nil || turn.RunID != call.RunID || turn.Status != AgentTurnStatusRunning || turn.LeaseOwner != f.WorkerID ||
		turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(f.Now) {
		return ErrNestedTurnNotActive
	}
	if state.TotalCalls >= f.MaxCalls {
		return fmt.Errorf("%w: at most %d calls", ErrNestedActionLimit, f.MaxCalls)
	}
	if state.UnfinishedCalls >= f.MaxConcurrent {
		return fmt.Errorf("%w: at most %d unfinished calls at once", ErrNestedActionLimit, f.MaxConcurrent)
	}
	return nil
}

func validateNestedActionFence(proposal ActionProposalRecord) error {
	fence := proposal.Nested
	if fence == nil {
		return nil
	}
	if proposal.Run != nil || proposal.Call.Origin != ActionOriginVM || strings.TrimSpace(fence.WorkerID) == "" || fence.Now.IsZero() ||
		fence.MaxCalls < 1 || fence.MaxConcurrent < 1 {
		return errors.New("nested action proposal requires a vm call, no Run update, a worker, a time, and bounds")
	}
	return nil
}

// NestedActionUnfinishedStatuses lists the statuses that count against the
// concurrency bound.
var NestedActionUnfinishedStatuses = []ActionCallStatus{ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusRunning}

// ActionExecution says who performs an ActionCall once it may run.
type ActionExecution string

// ActionExecutionHost marks a nested call the host performs itself, such as
// git over HTTPS for code in the chat's VM. The call is proposed, authorized,
// approved and recorded exactly like the offered Skill action it names, but no
// action worker dispatches it: the host claims it, takes a credential lease
// for it, does the work and reports the outcome.
const ActionExecutionHost ActionExecution = "host"

const (
	// NestedHostActionReadyGrace is how long a ready host call waits for its
	// host before an action worker may recover it (and cancel or fail it,
	// never dispatch it).
	NestedHostActionReadyGrace = 15 * time.Minute
)
