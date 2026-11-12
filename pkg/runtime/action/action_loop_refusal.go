package action

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The repeat-guard codes that give a governed loop call a refused
// action_calls row (section 08 §5.7). same_step_duplicate and run_stopping
// refusals get no row.
const (
	RefusedLoopCallRepeat              = "repeat_refused"
	RefusedLoopCallDuplicateSuppressed = "duplicate_suppressed"
	RefusedLoopCallRepeatLimit         = RunFailureCodeRepeatLimit
)

// ActivityEventActionRefused is the event of a refused governed loop call.
const ActivityEventActionRefused = "action.refused"

// RefusedLoopCallRequest describes one governed tool call of a step that the
// kernel's repeat guard refused before dispatch.
type RefusedLoopCallRequest struct {
	Scope           models.Scope
	Run             *models.AgentRun
	StepID          string // ActionCall.TurnID (D1)
	DeploymentID    string
	SkillID         string
	SkillVersion    string
	Action          string
	BindingID       string
	BindingRevision int64
	Risk            skill.RiskLevel
	SideEffect      skill.SideEffect
	// InputSchema is the action's input schema; sensitive arguments are
	// stripped from the stored row by it, as for any proposal.
	InputSchema map[string]interface{}
	Arguments   map[string]interface{}
	Code        string // repeat_refused | duplicate_suppressed | repeat_limit
	// Message is the model-facing refusal text, without appended result blocks.
	Message string
	// DuplicateOfActionCallID is the prior counted entry's ActionCallID.
	DuplicateOfActionCallID string
	PriorStep               int64
	// Count is the call's position in the counted sequence (3 = 3rd identical).
	Count int
	Lease *models.AgentRunLeaseGuard
}

func (r RefusedLoopCallRequest) validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if r.Run == nil || r.Run.Scope != r.Scope || strings.TrimSpace(r.Run.ID) == "" {
		return errors.New("a refused loop call requires its Run in scope")
	}
	switch r.Code {
	case RefusedLoopCallRepeat, RefusedLoopCallDuplicateSuppressed, RefusedLoopCallRepeatLimit:
	default:
		return errors.New("refused loop call code is invalid")
	}
	if strings.TrimSpace(r.StepID) == "" || strings.TrimSpace(r.Message) == "" || strings.TrimSpace(r.DuplicateOfActionCallID) == "" {
		return errors.New("a refused loop call requires its step, message and prior action call")
	}
	if r.PriorStep < 0 || r.Count < 1 {
		return errors.New("refused loop call step and count are invalid")
	}
	return nil
}

// NewRefusedLoopCall builds the refused row and its activity event. It does
// not resolve policy, approval, budget or credentials: the call never runs.
func (c *ActionCoordinator) NewRefusedLoopCall(req RefusedLoopCallRequest) (*models.ActionCall, *models.ActivityEvent, error) {
	if c == nil {
		return nil, nil, errors.New("action coordinator is required")
	}
	if err := req.validate(); err != nil {
		return nil, nil, err
	}
	now := c.now().UTC()
	run := req.Run
	call := &models.ActionCall{
		ID: c.newID(), Scope: req.Scope, RunID: run.ID, TurnID: req.StepID, DeploymentID: req.DeploymentID,
		BindingID: req.BindingID, BindingRevision: req.BindingRevision, SkillID: req.SkillID, SkillVersion: req.SkillVersion,
		Action: req.Action, Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, Status: models.ActionCallStatusRefused,
		Risk: req.Risk, SideEffect: req.SideEffect, Arguments: persistedActionArguments(req.Arguments, req.InputSchema),
		DuplicateOfActionCallID: req.DuplicateOfActionCallID, ErrorCode: req.Code, Error: req.Message,
		ErrorDetails: map[string]string{"step": strconv.FormatInt(req.PriorStep, 10), "count": strconv.Itoa(req.Count)},
		MaxAttempts:  1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	if err := call.Validate(); err != nil {
		return nil, nil, err
	}
	summary := "Skipped a repeated call"
	if req.Code == RefusedLoopCallDuplicateSuppressed {
		summary = "Already done; not repeated"
	}
	actor := models.ActivityActor{Type: "worker"}
	if req.Lease != nil {
		actor.ID = req.Lease.WorkerID
	}
	event := &models.ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: ActivityEventActionRefused, Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: req.StepID, TeamID: models.TeamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: actor, Summary: summary, Visibility: models.ActivityVisibilityScope, CreatedAt: now,
		Payload: map[string]interface{}{
			"actionCallId": call.ID, "duplicateOfActionCallId": call.DuplicateOfActionCallID,
			"skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "errorCode": call.ErrorCode,
		},
	}
	return call, event, nil
}

// RecordRefusedLoopCall stores the refused row and its event under the Run
// lease (the loop fence of CreateActionProposal), without changing the Run.
// The executor then names the row on the tool entry's terminal transition
// (ToolEntryTransitionRequest.ActionCallID), which requires it to exist.
func (c *ActionCoordinator) RecordRefusedLoopCall(ctx context.Context, req RefusedLoopCallRequest) (*models.ActionProposalResult, error) {
	if req.Lease == nil {
		return nil, errors.New("a refused loop call requires its Run lease")
	}
	call, event, err := c.NewRefusedLoopCall(req)
	if err != nil {
		return nil, err
	}
	return c.actions.CreateActionProposal(ctx, models.ActionProposalRecord{
		Call: call, ExpectedRunRevision: req.Run.Revision, Lease: req.Lease, Event: event,
	})
}

const RunFailureCodeRepeatLimit = "repeat_limit"
