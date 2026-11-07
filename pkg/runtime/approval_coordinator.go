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
	Decision         ApprovalDecision
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

type approvalResolutionKind int

const (
	approvalResolutionPrincipal approvalResolutionKind = iota
	approvalResolutionTimeout
	approvalResolutionMode
	approvalResolutionSuperseded
)

// ApprovalSupersededPrincipal is the kernel principal recorded when a chat
// message from the person replaces a pending approval.
var ApprovalSupersededPrincipal = ApprovalPrincipal{Type: "system", ID: "kernel"}

// ApprovalSupersededReason is the decision reason of a superseded approval.
const ApprovalSupersededReason = "The person replied with a message instead of approving; the action was not carried out."

// ApprovalModePrincipal is the system principal recorded when a conversation
// approval mode change resolves a pending approval.
var ApprovalModePrincipal = ApprovalPrincipal{Type: "system", ID: "approval-mode"}

// ErrApprovalExpired reports that an approval can no longer be approved
// because its deadline elapsed; only the timeout rule can resolve it.
var ErrApprovalExpired = errors.New("approval has expired")

func (c *ApprovalCoordinator) Resolve(ctx context.Context, req ResolveApprovalRequest) (*ApprovalResolutionResult, error) {
	return c.resolve(ctx, req, approvalResolutionPrincipal)
}

// ResolveByApprovalMode approves a pending checkpoint because its
// conversation's approval mode now allows the action without review. The
// decision id is stable per approval, so concurrent or repeated sweeps converge
// on one decision; revision checks make a racing human decision win cleanly.
// The caller is responsible for proving the mode allows the action.
func (c *ApprovalCoordinator) ResolveByApprovalMode(ctx context.Context, scope Scope, approvalID string, expectedRevision int64, mode ConversationApprovalMode, correlationID string) (*ApprovalResolutionResult, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: invalid approval mode %q", ErrInvalidConversation, mode)
	}
	return c.resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approvalID, ExpectedRevision: expectedRevision,
		DecisionID: "approval-mode:" + approvalID, Decision: ApprovalDecisionApprove, Principal: ApprovalModePrincipal,
		Reason: "approval mode " + string(mode), CorrelationID: correlationID,
	}, approvalResolutionMode)
}

// ResolveSupersededByMessage closes a pending approval because the person
// answered it with a chat message. The Run resumes with that message as a
// follow-up and revises the proposal. The decision id is stable per approval,
// so repeated or concurrent messages converge on one decision; a racing human
// decision wins through the revision check. The caller proves the approval is
// one a message may replace.
func (c *ApprovalCoordinator) ResolveSupersededByMessage(ctx context.Context, scope Scope, approvalID string, expectedRevision int64, messageID string) (*ApprovalResolutionResult, error) {
	if strings.TrimSpace(messageID) == "" {
		return nil, errors.New("superseding message is required")
	}
	return c.resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approvalID, ExpectedRevision: expectedRevision,
		DecisionID: "superseded:" + approvalID, Principal: ApprovalSupersededPrincipal,
		Reason: ApprovalSupersededReason, CorrelationID: messageID,
	}, approvalResolutionSuperseded)
}

// ResolveTimeout applies only the timeout rule durably captured on the
// approval. It cannot resolve an approval early and does not accept a caller-
// supplied decision, preventing a worker from widening reviewed authority.
func (c *ApprovalCoordinator) ResolveTimeout(ctx context.Context, scope Scope, approvalID string, expectedRevision int64, correlationID string) (*ApprovalResolutionResult, error) {
	return c.resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approvalID, ExpectedRevision: expectedRevision,
		DecisionID: "approval-timeout:" + approvalID, Principal: ApprovalPrincipal{Type: "system", ID: "approval-timeout-worker"},
		Reason: "Approval deadline elapsed", CorrelationID: correlationID,
	}, approvalResolutionTimeout)
}

// approvalResolveAttempts bounds how often resolve re-reads and recomputes a
// loop call's decision after its Run row moved under it (a heartbeat, a park
// or another decision of the same Run).
const approvalResolveAttempts = 4

// resolve decides an approval. Every loop decision writes its Run under the
// Run row lock, the first lock the stores take: a Run parked on the approval
// is queued (or, when paused, set to resume queued); a Run that still holds
// the approval in its running step only gets a revision bump, so a racing
// park of that step fails its revision check and re-plans. A nested (vm)
// decision never touches its Run.
func (c *ApprovalCoordinator) resolve(ctx context.Context, req ResolveApprovalRequest, kind approvalResolutionKind) (*ApprovalResolutionResult, error) {
	if c == nil || c.portfolio == nil || c.actions == nil || kind == approvalResolutionPrincipal && c.authorize == nil {
		return nil, errors.New("approval coordinator is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.ApprovalID) == "" || req.ExpectedRevision < 1 || strings.TrimSpace(req.DecisionID) == "" || strings.TrimSpace(req.Principal.Type) == "" || strings.TrimSpace(req.Principal.ID) == "" {
		return nil, errors.New("approval, expected revision, decision id, and principal are required")
	}
	for attempt := 1; ; attempt++ {
		record, result, err := c.planResolution(ctx, req, kind)
		if err != nil || result != nil {
			return result, err
		}
		resolved, err := c.actions.ResolveApproval(ctx, *record)
		if errors.Is(err, ErrRevisionConflict) && record.Call.Origin == ActionOriginLoop && attempt < approvalResolveAttempts {
			continue
		}
		return resolved, err
	}
}

// planResolution reads the approval, its call and its Run and computes the
// resolution record. It returns a result instead when the same decision was
// already applied.
func (c *ApprovalCoordinator) planResolution(ctx context.Context, req ResolveApprovalRequest, kind approvalResolutionKind) (*ApprovalResolutionRecord, *ApprovalResolutionResult, error) {
	timeout := kind == approvalResolutionTimeout
	approval, err := c.actions.GetApproval(ctx, req.Scope, req.ApprovalID)
	if err != nil {
		return nil, nil, err
	}
	call, err := c.actions.GetActionCall(ctx, req.Scope, approval.ActionCallID)
	if err != nil {
		return nil, nil, err
	}
	run, err := c.portfolio.GetAgentRun(ctx, req.Scope, approval.RunID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil {
		return nil, nil, ErrRunNotFound
	}
	if approval.Status != ApprovalStatusPending {
		if approval.DecisionID == req.DecisionID {
			return nil, &ApprovalResolutionResult{Approval: approval, Call: call, Run: run, Resolved: false}, nil
		}
		return nil, nil, ErrApprovalResolved
	}
	if approval.Revision != req.ExpectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	loop := call.Origin == ActionOriginLoop
	nested := call.Origin == ActionOriginVM
	if !loop && !nested {
		return nil, nil, fmt.Errorf("action call %s has no loop or vm origin", call.ID)
	}
	now := c.now().UTC()
	expired := !now.Before(approval.ExpiresAt)
	if !timeout && !expired && kind != approvalResolutionSuperseded {
		if err := req.Decision.Validate(); err != nil {
			return nil, nil, err
		}
		if req.Decision == ApprovalDecisionRequestChanges && strings.TrimSpace(req.Reason) == "" {
			return nil, nil, errors.New("reviewer guidance is required when requesting changes")
		}
	}
	if timeout && !expired {
		return nil, nil, errors.New("approval timeout deadline has not elapsed")
	}
	if (kind == approvalResolutionMode || kind == approvalResolutionSuperseded) && expired {
		return nil, nil, ErrApprovalExpired
	}
	callWaiting := call.Status == ActionCallStatusWaitingApproval && call.ApprovalID == approval.ID
	// parked: the Run waits on this approval, directly or under a pause.
	parked := run.Status == AgentRunStatusWaitingForApproval && run.WakeCondition != nil &&
		run.WakeCondition.Type == "approval" && run.WakeCondition.Reference == approval.ID
	pausedParked := run.Status == AgentRunStatusPaused && run.PausedFrom == AgentRunStatusWaitingForApproval &&
		run.PausedWakeCondition != nil && run.PausedWakeCondition.Type == "approval" && run.PausedWakeCondition.Reference == approval.ID
	// held: the Run proposed the call and has not parked on it (its step still
	// runs, or it parked on another approval). The decision is accepted and
	// the Run picks it up when it resumes the tool entry.
	held := loop && !parked && !pausedParked && callWaiting && !isTerminalAgentRunStatus(run.Status)
	// A nested call waits on its approval while its Run keeps running.
	decidable := callWaiting && (nested || held || parked || pausedParked)
	if !expired && !decidable {
		if !callWaiting {
			return nil, nil, fmt.Errorf("%w: action is not waiting on this approval", ErrApprovalResolved)
		}
		return nil, nil, fmt.Errorf("%w: run is not waiting on this approval", ErrInvalidRunTransition)
	}
	status := ApprovalStatusRejected
	eventType := "approval.rejected"
	callStatus := ActionCallStatusDenied
	if expired {
		// A manifest review floor never auto-approves, whatever the
		// checkpoint's timeout rule says.
		if timeout && approval.TimeoutDecision == ApprovalTimeoutApprove && decidable && approval.ProposedAction["review"] != "always" {
			status = ApprovalStatusApproved
			eventType = "approval.auto_approved"
			callStatus = ActionCallStatusReady
		} else {
			status = ApprovalStatusExpired
			eventType = "approval.expired"
			if !callWaiting {
				callStatus = call.Status
			}
		}
	} else {
		if kind == approvalResolutionPrincipal {
			if err := c.authorize.AuthorizeApproval(ctx, req.Principal, cloneApprovalCheckpoint(approval)); err != nil {
				return nil, nil, fmt.Errorf("authorize approval: %w", err)
			}
		}
		switch {
		case kind == approvalResolutionSuperseded:
			status = ApprovalStatusSuperseded
			eventType = "approval.superseded"
		case req.Decision == ApprovalDecisionApprove:
			status = ApprovalStatusApproved
			eventType = "approval.approved"
			callStatus = ActionCallStatusReady
		case req.Decision == ApprovalDecisionRequestChanges:
			status = ApprovalStatusChangesRequested
			eventType = "approval.changes_requested"
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
	if status == ApprovalStatusSuperseded {
		updatedApproval.SupersededByMessageID = req.CorrelationID
	}
	updatedCall := cloneActionCall(call)
	updatedCall.Status = callStatus
	updatedCall.AvailableAt = now
	updatedCall.UpdatedAt = now
	updatedCall.Revision++
	denied := callStatus == ActionCallStatusDenied && callWaiting
	if denied {
		updatedCall.Error = string(status)
		if req.Reason != "" {
			updatedCall.Error += ": " + req.Reason
		}
		updatedCall.CompletedAt = &now
	}
	event := &ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: call.TurnID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: ActivityActor{Type: req.Principal.Type, ID: req.Principal.ID},
		Summary: fmt.Sprintf("Approval %s for %s.%s", status, call.SkillID, call.Action), Visibility: ActivityVisibilityScope, CorrelationID: req.CorrelationID,
		CausationID: approval.ID, CreatedAt: now,
		Payload: map[string]interface{}{"approvalId": approval.ID, "actionCallId": call.ID, "status": status, "decisionId": req.DecisionID, "decisionReason": strings.TrimSpace(req.Reason)},
	}
	addActionOrigin(event.Payload, call)
	record := &ApprovalResolutionRecord{
		Approval: updatedApproval, ExpectedApprovalRevision: approval.Revision,
		Call: updatedCall, ExpectedCallRevision: call.Revision, Event: event,
	}
	// A nested decision, or a late expiry of a call whose Run ended or moved
	// on, resolves without the Run.
	if nested || !callWaiting || !(held || parked || pausedParked) {
		return record, nil, nil
	}
	updatedRun := cloneAgentRun(run)
	if denied && updatedRun.Budget != nil {
		if _, reserved := updatedRun.BudgetReservations[actionBudgetReservationID(call.ID)]; reserved {
			if err := releaseRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID)); err != nil {
				return nil, nil, err
			}
		}
	}
	switch {
	case parked:
		updatedRun.Status = AgentRunStatusQueued
		updatedRun.WakeCondition = nil
		updatedRun.AvailableAt = now
		updatedRun.QueueEnteredAt = now
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
		updatedRun.LastWakeSignalID = req.DecisionID
	case pausedParked:
		// The operator's pause stands; resume queues the Run, which then
		// reads the decided tool entry.
		updatedRun.PausedFrom = AgentRunStatusQueued
		updatedRun.PausedWakeCondition = nil
		updatedRun.LastWakeSignalID = req.DecisionID
	}
	updatedRun.UpdatedAt = now
	updatedRun.Revision++
	record.Run = updatedRun
	record.ExpectedRunRevision = run.Revision
	return record, nil, nil
}
