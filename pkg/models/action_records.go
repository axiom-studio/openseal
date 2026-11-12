package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func ValidateActionProposalRecord(proposal ActionProposalRecord) error {
	if err := proposal.Call.Validate(); err != nil {
		return err
	}
	if err := validateNestedActionFence(proposal); err != nil {
		return err
	}
	if proposal.Nested == nil {
		// Only the run loop proposes outside a nested fence. It holds the
		// Run lease and changes the Run only to reserve budget.
		if proposal.Call.Origin != ActionOriginLoop {
			return errors.New("a nested action call requires its step fence")
		}
		if proposal.Lease == nil || strings.TrimSpace(proposal.Lease.WorkerID) == "" || proposal.Lease.Now.IsZero() {
			return errors.New("a loop action proposal requires its Run lease")
		}
		if proposal.Run != nil {
			if err := proposal.Run.Validate(); err != nil {
				return err
			}
			if proposal.Call.Scope != proposal.Run.Scope || proposal.Call.RunID != proposal.Run.ID {
				return ErrInvalidScope
			}
		}
	}
	if err := proposal.Event.Validate(); err != nil {
		return err
	}
	if proposal.Call.Scope != proposal.Event.Scope || proposal.Call.RunID != proposal.Event.RunID {
		return ErrInvalidScope
	}
	if proposal.Approval != nil {
		if err := proposal.Approval.Validate(); err != nil {
			return err
		}
		if proposal.Approval.Scope != proposal.Call.Scope || proposal.Approval.RunID != proposal.Call.RunID ||
			proposal.Approval.ActionCallID != proposal.Call.ID || proposal.Call.ApprovalID != proposal.Approval.ID {
			return ErrInvalidScope
		}
	}
	return nil
}

func ValidateActionSubmissionBinding(call *ActionCall, binding *skill.Binding) error {
	if call == nil || binding == nil || binding.Disabled || call.BindingID == "" ||
		binding.Scope.Kind != call.Scope.Kind || binding.Scope.ID != call.Scope.ID ||
		binding.DeploymentID != call.DeploymentID || binding.ID != call.BindingID ||
		binding.Revision != call.BindingRevision || binding.SkillID != call.SkillID || binding.SkillVersion != call.SkillVersion {
		return skill.ErrBindingUnavailable
	}
	return nil
}

// A nested call's approval is resolved without a Run update (Run nil). A loop
// call's approval updates its Run only when the Run is parked on it.
func ValidateApprovalResolutionRecord(resolution ApprovalResolutionRecord) error {
	if err := resolution.Approval.Validate(); err != nil {
		return err
	}
	if err := resolution.Call.Validate(); err != nil {
		return err
	}
	if resolution.Run != nil && resolution.Call.Origin == ActionOriginVM {
		return errors.New("a nested action call's approval resolves without a Run update")
	}
	if resolution.Run != nil {
		if err := resolution.Run.Validate(); err != nil {
			return err
		}
		if resolution.Approval.Scope != resolution.Run.Scope || resolution.Approval.RunID != resolution.Run.ID {
			return ErrInvalidScope
		}
	}
	if err := resolution.Event.Validate(); err != nil {
		return err
	}
	if resolution.Approval.Status == ApprovalStatusPending || strings.TrimSpace(resolution.Approval.DecisionID) == "" || resolution.Approval.DecisionBy == nil || resolution.Approval.DecidedAt == nil {
		return errors.New("approval resolution requires a terminal decision, id, principal, and decision time")
	}
	if resolution.Approval.Scope != resolution.Call.Scope || resolution.Approval.Scope != resolution.Event.Scope ||
		resolution.Approval.RunID != resolution.Call.RunID || resolution.Approval.RunID != resolution.Event.RunID ||
		resolution.Approval.ActionCallID != resolution.Call.ID || resolution.Call.ApprovalID != resolution.Approval.ID {
		return ErrInvalidScope
	}
	return nil
}

func ValidateActionExecutionRecord(execution ActionExecutionRecord) error {
	if err := execution.Call.Validate(); err != nil {
		return err
	}
	if err := execution.Event.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(execution.WorkerID) == "" || execution.Now.IsZero() || execution.ExpectedRunRevision <= 0 {
		return errors.New("action execution worker, current time, and observed Run revision are required")
	}
	if execution.Call.Status != ActionCallStatusReady && execution.Call.Status != ActionCallStatusSucceeded && execution.Call.Status != ActionCallStatusFailed && execution.Call.Status != ActionCallStatusCanceled {
		return errors.New("persisted action execution must retry or finish")
	}
	if execution.Call.Scope != execution.Event.Scope || execution.Call.RunID != execution.Event.RunID {
		return ErrInvalidScope
	}
	if execution.Run != nil {
		if err := execution.Run.Validate(); err != nil {
			return err
		}
		if execution.Run.Scope != execution.Call.Scope || execution.Run.ID != execution.Call.RunID || execution.Call.Status == ActionCallStatusReady {
			return ErrInvalidScope
		}
	}
	return nil
}

func CloneActionCall(value *ActionCall) *ActionCall {
	if value == nil {
		return nil
	}
	var result ActionCall
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func CloneApprovalCheckpoint(value *ApprovalCheckpoint) *ApprovalCheckpoint {
	if value == nil {
		return nil
	}
	var result ApprovalCheckpoint
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

// runOwnsActionDependency reports whether run waits on call as its action
// dependency, active or paused. Stores use it to keep a paused dependency
// unclaimed; loop calls are never a Run's dependency.
func runOwnsActionDependency(run *AgentRun, call *ActionCall) bool {
	if run == nil || call == nil || call.Origin == ActionOriginLoop || run.Scope != call.Scope || run.ID != call.RunID {
		return false
	}
	condition := run.WakeCondition
	if run.Status == AgentRunStatusPaused && run.PausedFrom == AgentRunStatusWaitingForDependency {
		condition = run.PausedWakeCondition
	} else if run.Status != AgentRunStatusWaitingForDependency {
		return false
	}
	return condition != nil && condition.Type == "action" && condition.Reference == call.ID
}

func RunHasPausedActionDependency(run *AgentRun, call *ActionCall) bool {
	return run != nil && run.Status == AgentRunStatusPaused && runOwnsActionDependency(run, call)
}

// ApprovalNotificationStore is the durable intersection needed to project a
// canonical approval into an external conversation. No provider-specific
// fields or credentials cross this boundary.
type ApprovalNotificationStore interface {
	ExternalConversationStore
	CallbackRegistrationStore
	ActionStore
	PortfolioStore
}

const CapabilityInvocationContextKey = "capabilityInvocation"

// ActionCallReader reads the authoritative ActionCall behind a governed tool
// entry. Deterministic producers read governed output from it, never from the
// (possibly bounded) tool result content.
type ActionCallReader interface {
	GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error)
}
