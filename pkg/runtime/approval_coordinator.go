package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ApprovalAuthorizer interface {
	AuthorizeApproval(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error
}

type ApprovalAuthorizerFunc func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error

func (f ApprovalAuthorizerFunc) AuthorizeApproval(ctx context.Context, principal ApprovalPrincipal, approval *ApprovalCheckpoint) error {
	return f(ctx, principal, approval)
}

type ResolveApprovalRequest struct {
	Scope            Scope
	ApprovalID       string
	ExpectedRevision int64
	DecisionID       string
	Approve          bool
	Principal        ApprovalPrincipal
	Reason           string
	CorrelationID    string
}

type ApprovalCoordinator struct {
	portfolio PortfolioStore
	actions   ActionStore
	authorize ApprovalAuthorizer
	now       func() time.Time
	newID     func() string
}

func NewApprovalCoordinator(portfolio PortfolioStore, actions ActionStore, authorizer ApprovalAuthorizer) *ApprovalCoordinator {
	return &ApprovalCoordinator{portfolio: portfolio, actions: actions, authorize: authorizer, now: time.Now, newID: uuid.NewString}
}

func (c *ApprovalCoordinator) Resolve(ctx context.Context, req ResolveApprovalRequest) (*ApprovalResolutionResult, error) {
	if c == nil || c.portfolio == nil || c.actions == nil || c.authorize == nil {
		return nil, errors.New("approval coordinator is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.ApprovalID) == "" || req.ExpectedRevision < 1 || strings.TrimSpace(req.DecisionID) == "" || strings.TrimSpace(req.Principal.Type) == "" || strings.TrimSpace(req.Principal.ID) == "" {
		return nil, errors.New("approval, expected revision, decision id, and principal are required")
	}
	approval, err := c.actions.GetApproval(ctx, req.Scope, req.ApprovalID)
	if err != nil {
		return nil, err
	}
	call, err := c.actions.GetActionCall(ctx, req.Scope, approval.ActionCallID)
	if err != nil {
		return nil, err
	}
	run, err := c.portfolio.GetAgentRun(ctx, req.Scope, approval.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if approval.Status != ApprovalStatusPending {
		if approval.DecisionID == req.DecisionID {
			return &ApprovalResolutionResult{Approval: approval, Call: call, Run: run, Resolved: false}, nil
		}
		return nil, ErrApprovalResolved
	}
	if approval.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if run.Status != AgentRunStatusWaitingForApproval || run.WakeCondition == nil || run.WakeCondition.Type != "approval" || run.WakeCondition.Reference != approval.ID {
		return nil, fmt.Errorf("%w: run is not waiting on this approval", ErrInvalidRunTransition)
	}
	if call.Status != ActionCallStatusWaitingApproval || call.ApprovalID != approval.ID {
		return nil, fmt.Errorf("%w: action is not waiting on this approval", ErrApprovalResolved)
	}
	now := c.now().UTC()
	status := ApprovalStatusRejected
	eventType := "approval.rejected"
	callStatus := ActionCallStatusDenied
	if !now.Before(approval.ExpiresAt) {
		status = ApprovalStatusExpired
		eventType = "approval.expired"
	} else {
		if err := c.authorize.AuthorizeApproval(ctx, req.Principal, cloneApprovalCheckpoint(approval)); err != nil {
			return nil, fmt.Errorf("authorize approval: %w", err)
		}
		if req.Approve {
			status = ApprovalStatusApproved
			eventType = "approval.approved"
			callStatus = ActionCallStatusReady
		}
	}
	updatedApproval := cloneApprovalCheckpoint(approval)
	updatedApproval.Status = status
	updatedApproval.DecisionID = req.DecisionID
	updatedApproval.DecisionBy = &ApprovalPrincipal{Type: req.Principal.Type, ID: req.Principal.ID}
	updatedApproval.DecisionReason = req.Reason
	updatedApproval.DecidedAt = &now
	updatedApproval.UpdatedAt = now
	updatedApproval.Revision++
	updatedCall := cloneActionCall(call)
	updatedCall.Status = callStatus
	updatedCall.AvailableAt = now
	updatedCall.UpdatedAt = now
	updatedCall.Revision++
	if callStatus == ActionCallStatusDenied {
		updatedCall.Error = string(status)
		if req.Reason != "" {
			updatedCall.Error += ": " + req.Reason
		}
	}
	updatedRun := cloneAgentRun(run)
	if callStatus == ActionCallStatusDenied && updatedRun.Budget != nil {
		if err := releaseRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID)); err != nil {
			return nil, err
		}
	}
	if callStatus == ActionCallStatusReady {
		updatedRun.Status = AgentRunStatusWaitingForDependency
		updatedRun.WakeCondition = &WakeCondition{Type: "action", Reference: call.ID}
	} else {
		updatedRun.Status = AgentRunStatusQueued
		updatedRun.WakeCondition = nil
		updatedRun.AvailableAt = now
		updatedRun.QueueEnteredAt = now
	}
	updatedRun.LeaseOwner = ""
	updatedRun.LeaseExpiresAt = nil
	updatedRun.LastWakeSignalID = req.DecisionID
	updatedRun.UpdatedAt = now
	updatedRun.Revision++
	summary := fmt.Sprintf("Approval %s for %s.%s", status, call.SkillID, call.Action)
	event := &ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: call.TurnID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: ActivityActor{Type: req.Principal.Type, ID: req.Principal.ID},
		Summary: summary, Visibility: ActivityVisibilityScope, CorrelationID: req.CorrelationID,
		CausationID: approval.ID, CreatedAt: now,
		Payload: map[string]interface{}{"approvalId": approval.ID, "actionCallId": call.ID, "status": status, "decisionId": req.DecisionID},
	}
	return c.actions.ResolveApproval(ctx, ApprovalResolutionRecord{
		Approval: updatedApproval, ExpectedApprovalRevision: approval.Revision,
		Call: updatedCall, ExpectedCallRevision: call.Revision,
		Run: updatedRun, ExpectedRunRevision: run.Revision, Event: event,
	})
}
