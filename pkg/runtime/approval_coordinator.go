package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/google/uuid"
)

type ApprovalAuthorizer interface {
	AuthorizeApproval(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error
}

// AgentBuilderApprovalAuthorizer is the host authorizer that may approve
// agent-builder cards (create_agent, update_agent, create_team; D11) and
// openseal.skills binding cards aimed at another Agent (targetAgentId). Only a
// coordinator built with one accepts such an approve, and only from a "user"
// principal. A targeted skills card is not an agent-builder card: its approve
// is checked by AuthorizeApproval alone, which the host uses for the person's
// right on the target Agent. approval modes, the timeout rule, external channel replies,
// callbacks, notification links and every other in-kernel resolver use an
// authorizer without this method and are refused. Reject, changes_requested,
// supersede, expire and cancel stay allowed everywhere.
type AgentBuilderApprovalAuthorizer interface {
	ApprovalAuthorizer
	AuthorizeAgentBuilderApproval(ctx context.Context, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error
}

// ErrAgentBuilderApprovalForbidden reports an approve of an agent-builder card
// that did not come from a person through the host approval route (D11).
var ErrAgentBuilderApprovalForbidden = errors.New("this approval can only be given by a person in the app")

// agentBuilderTeamOperation is the create_team operation of an openseal.teams
// card, the only Team card under the agent-builder rule (D11).
const agentBuilderTeamOperation = "create_team"

// isAgentBuilderApproval reports whether approval reviews an agent-builder
// action: any agent_profile card, or a team card for create_team. Team role and
// profile amendments keep their own approval paths.
func isAgentBuilderApproval(approval *models.ApprovalCheckpoint) bool {
	if approval == nil {
		return false
	}
	resourceType, _ := approval.ProposedAction["resourceType"].(string)
	operation, _ := approval.ProposedAction["operation"].(string)
	return resourceType == models.AgentProfileResourceType || resourceType == "team" && operation == agentBuilderTeamOperation
}

// isTargetedSkillBindingApproval reports whether approval reviews an
// openseal.skills upsert_binding or disable_binding aimed at another Agent
// (the validator's preview names its targetAgent).
func isTargetedSkillBindingApproval(approval *models.ApprovalCheckpoint) bool {
	if approval == nil {
		return false
	}
	resourceType, _ := approval.ProposedAction["resourceType"].(string)
	operation, _ := approval.ProposedAction["operation"].(string)
	return resourceType == "skill_binding" && (operation == skillbinding.SkillActionUpsertBinding || operation == skillbinding.SkillActionDisableBinding) &&
		approval.ProposedAction["targetAgent"] != nil
}

// approvedOnlyByPerson reports whether an approve of approval must come from
// a person through the host approval route: an agent-builder card or a
// targeted skills card. No approval mode, timeout rule, external reply or
// callback approves one.
func approvedOnlyByPerson(approval *models.ApprovalCheckpoint) bool {
	return isAgentBuilderApproval(approval) || isTargetedSkillBindingApproval(approval)
}

// authorizeAgentBuilderApproval applies D11 to an approve of an agent-builder
// or targeted skills card: a person's decision through a host
// AgentBuilderApprovalAuthorizer, on top of the coordinator's ordinary
// authorization.
func (c *ApprovalCoordinator) authorizeAgentBuilderApproval(ctx context.Context, kind approvalResolutionKind, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
	host, ok := c.authorize.(AgentBuilderApprovalAuthorizer)
	if kind != approvalResolutionPrincipal || principal.Type != "user" || !ok {
		return fmt.Errorf("authorize approval: %w", ErrAgentBuilderApprovalForbidden)
	}
	if err := host.AuthorizeApproval(ctx, principal, models.CloneApprovalCheckpoint(approval)); err != nil {
		return fmt.Errorf("authorize approval: %w", err)
	}
	if !isAgentBuilderApproval(approval) {
		return nil
	}
	if err := host.AuthorizeAgentBuilderApproval(ctx, principal, models.CloneApprovalCheckpoint(approval)); err != nil {
		return fmt.Errorf("authorize approval: %w", err)
	}
	return nil
}

type ApprovalAuthorizerFunc func(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error

func (f ApprovalAuthorizerFunc) AuthorizeApproval(ctx context.Context, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
	return f(ctx, principal, approval)
}

type ResolveApprovalRequest struct {
	Scope            models.Scope
	ApprovalID       string
	ExpectedRevision int64
	DecisionID       string
	Decision         models.ApprovalDecision
	Principal        models.ApprovalPrincipal
	Reason           string
	CorrelationID    string
}

type ApprovalCoordinator struct {
	portfolio models.PortfolioStore
	actions   models.ActionStore
	authorize ApprovalAuthorizer
	now       func() time.Time
	newID     func() string
}

func NewApprovalCoordinator(portfolio models.PortfolioStore, actions models.ActionStore, authorizer ApprovalAuthorizer, opts ...models.ServiceOption) *ApprovalCoordinator {
	return &ApprovalCoordinator{portfolio: portfolio, actions: actions, authorize: authorizer, now: models.ClockFrom(opts), newID: uuid.NewString}
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
var ApprovalSupersededPrincipal = models.ApprovalPrincipal{Type: "system", ID: "kernel"}

// ApprovalSupersededReason is the decision reason of a superseded approval.
const ApprovalSupersededReason = "The person replied with a message instead of approving; the action was not carried out."

// ApprovalModePrincipal is the system principal recorded when a conversation
// approval mode change resolves a pending approval.
var ApprovalModePrincipal = models.ApprovalPrincipal{Type: "system", ID: "approval-mode"}

// ErrApprovalExpired reports that an approval can no longer be approved
// because its deadline elapsed; only the timeout rule can resolve it.
var ErrApprovalExpired = errors.New("approval has expired")

func (c *ApprovalCoordinator) Resolve(ctx context.Context, req ResolveApprovalRequest) (*models.ApprovalResolutionResult, error) {
	return c.resolve(ctx, req, approvalResolutionPrincipal)
}

// ResolveByApprovalMode approves a pending checkpoint because its
// conversation's approval mode now allows the action without review. The
// decision id is stable per approval, so concurrent or repeated sweeps converge
// on one decision; revision checks make a racing human decision win cleanly.
// The caller is responsible for proving the mode allows the action.
func (c *ApprovalCoordinator) ResolveByApprovalMode(ctx context.Context, scope models.Scope, approvalID string, expectedRevision int64, mode models.ConversationApprovalMode, correlationID string) (*models.ApprovalResolutionResult, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: invalid approval mode %q", models.ErrInvalidConversation, mode)
	}
	return c.resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approvalID, ExpectedRevision: expectedRevision,
		DecisionID: "approval-mode:" + approvalID, Decision: models.ApprovalDecisionApprove, Principal: ApprovalModePrincipal,
		Reason: "approval mode " + string(mode), CorrelationID: correlationID,
	}, approvalResolutionMode)
}

// ResolveSupersededByMessage closes a pending approval because the person
// answered it with a chat message. The Run resumes with that message as a
// follow-up and revises the proposal. The decision id is stable per approval,
// so repeated or concurrent messages converge on one decision; a racing human
// decision wins through the revision check. The caller proves the approval is
// one a message may replace.
func (c *ApprovalCoordinator) ResolveSupersededByMessage(ctx context.Context, scope models.Scope, approvalID string, expectedRevision int64, messageID string) (*models.ApprovalResolutionResult, error) {
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
func (c *ApprovalCoordinator) ResolveTimeout(ctx context.Context, scope models.Scope, approvalID string, expectedRevision int64, correlationID string) (*models.ApprovalResolutionResult, error) {
	return c.resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approvalID, ExpectedRevision: expectedRevision,
		DecisionID: "approval-timeout:" + approvalID, Principal: models.ApprovalPrincipal{Type: "system", ID: "approval-timeout-worker"},
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
func (c *ApprovalCoordinator) resolve(ctx context.Context, req ResolveApprovalRequest, kind approvalResolutionKind) (*models.ApprovalResolutionResult, error) {
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
		if errors.Is(err, models.ErrRevisionConflict) && record.Call.Origin == models.ActionOriginLoop && attempt < approvalResolveAttempts {
			continue
		}
		return resolved, err
	}
}

// planResolution reads the approval, its call and its Run and computes the
// resolution record. It returns a result instead when the same decision was
// already applied.
func (c *ApprovalCoordinator) planResolution(ctx context.Context, req ResolveApprovalRequest, kind approvalResolutionKind) (*models.ApprovalResolutionRecord, *models.ApprovalResolutionResult, error) {
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
		return nil, nil, models.ErrRunNotFound
	}
	if approval.Status != models.ApprovalStatusPending {
		if approval.DecisionID == req.DecisionID {
			return nil, &models.ApprovalResolutionResult{Approval: approval, Call: call, Run: run, Resolved: false}, nil
		}
		return nil, nil, models.ErrApprovalResolved
	}
	if approval.Revision != req.ExpectedRevision {
		return nil, nil, models.ErrRevisionConflict
	}
	loop := call.Origin == models.ActionOriginLoop
	nested := call.Origin == models.ActionOriginVM
	if !loop && !nested {
		return nil, nil, fmt.Errorf("action call %s has no loop or vm origin", call.ID)
	}
	now := c.now().UTC()
	expired := !now.Before(approval.ExpiresAt)
	if !timeout && !expired && kind != approvalResolutionSuperseded {
		if err := req.Decision.Validate(); err != nil {
			return nil, nil, err
		}
		if req.Decision == models.ApprovalDecisionRequestChanges && strings.TrimSpace(req.Reason) == "" {
			return nil, nil, errors.New("reviewer guidance is required when requesting changes")
		}
	}
	if timeout && !expired {
		return nil, nil, errors.New("approval timeout deadline has not elapsed")
	}
	if (kind == approvalResolutionMode || kind == approvalResolutionSuperseded) && expired {
		return nil, nil, ErrApprovalExpired
	}
	callWaiting := call.Status == models.ActionCallStatusWaitingApproval && call.ApprovalID == approval.ID
	// parked: the Run waits on this approval, directly or under a pause.
	parked := run.Status == models.AgentRunStatusWaitingForApproval && run.WakeCondition != nil &&
		run.WakeCondition.Type == "approval" && run.WakeCondition.Reference == approval.ID
	pausedParked := run.Status == models.AgentRunStatusPaused && run.PausedFrom == models.AgentRunStatusWaitingForApproval &&
		run.PausedWakeCondition != nil && run.PausedWakeCondition.Type == "approval" && run.PausedWakeCondition.Reference == approval.ID
	// held: the Run proposed the call and has not parked on it (its step still
	// runs, or it parked on another approval). The decision is accepted and
	// the Run picks it up when it resumes the tool entry.
	held := loop && !parked && !pausedParked && callWaiting && !models.IsTerminalAgentRunStatus(run.Status)
	// A nested call waits on its approval while its Run keeps running.
	decidable := callWaiting && (nested || held || parked || pausedParked)
	if !expired && !decidable {
		if !callWaiting {
			return nil, nil, fmt.Errorf("%w: action is not waiting on this approval", models.ErrApprovalResolved)
		}
		return nil, nil, fmt.Errorf("%w: run is not waiting on this approval", models.ErrInvalidRunTransition)
	}
	status := models.ApprovalStatusRejected
	eventType := "approval.rejected"
	callStatus := models.ActionCallStatusDenied
	if expired {
		// A manifest review floor never auto-approves, whatever the
		// checkpoint's timeout rule says, and neither does an agent-builder
		// card or a targeted skills card (D11): only a person approves those.
		if timeout && approval.TimeoutDecision == models.ApprovalTimeoutApprove && decidable && approval.ProposedAction["review"] != "always" &&
			!approvedOnlyByPerson(approval) {
			status = models.ApprovalStatusApproved
			eventType = "approval.auto_approved"
			callStatus = models.ActionCallStatusReady
		} else {
			status = models.ApprovalStatusExpired
			eventType = "approval.expired"
			if !callWaiting {
				callStatus = call.Status
			}
		}
	} else {
		builderApprove := kind != approvalResolutionSuperseded && req.Decision == models.ApprovalDecisionApprove && approvedOnlyByPerson(approval)
		if builderApprove {
			if err := c.authorizeAgentBuilderApproval(ctx, kind, req.Principal, approval); err != nil {
				return nil, nil, err
			}
		} else if kind == approvalResolutionPrincipal {
			if err := c.authorize.AuthorizeApproval(ctx, req.Principal, models.CloneApprovalCheckpoint(approval)); err != nil {
				return nil, nil, fmt.Errorf("authorize approval: %w", err)
			}
		}
		switch {
		case kind == approvalResolutionSuperseded:
			status = models.ApprovalStatusSuperseded
			eventType = "approval.superseded"
		case req.Decision == models.ApprovalDecisionApprove:
			status = models.ApprovalStatusApproved
			eventType = "approval.approved"
			callStatus = models.ActionCallStatusReady
		case req.Decision == models.ApprovalDecisionRequestChanges:
			status = models.ApprovalStatusChangesRequested
			eventType = "approval.changes_requested"
		}
	}
	updatedApproval := models.CloneApprovalCheckpoint(approval)
	updatedApproval.Status = status
	updatedApproval.DecisionID = req.DecisionID
	updatedApproval.DecisionBy = &models.ApprovalPrincipal{Type: req.Principal.Type, ID: req.Principal.ID}
	updatedApproval.DecisionReason = req.Reason
	updatedApproval.DecidedAt = &now
	updatedApproval.UpdatedAt = now
	updatedApproval.Revision++
	if status == models.ApprovalStatusSuperseded {
		updatedApproval.SupersededByMessageID = req.CorrelationID
	}
	updatedCall := models.CloneActionCall(call)
	updatedCall.Status = callStatus
	updatedCall.AvailableAt = now
	updatedCall.UpdatedAt = now
	updatedCall.Revision++
	denied := callStatus == models.ActionCallStatusDenied && callWaiting
	if denied {
		updatedCall.Error = string(status)
		if req.Reason != "" {
			updatedCall.Error += ": " + req.Reason
		}
		updatedCall.CompletedAt = &now
	}
	event := &models.ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: eventType, Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: call.TurnID, TeamID: models.TeamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: models.ActivityActor{Type: req.Principal.Type, ID: req.Principal.ID},
		Summary: fmt.Sprintf("Approval %s for %s.%s", status, call.SkillID, call.Action), Visibility: models.ActivityVisibilityScope, CorrelationID: req.CorrelationID,
		CausationID: approval.ID, CreatedAt: now,
		Payload: map[string]interface{}{"approvalId": approval.ID, "actionCallId": call.ID, "status": status, "decisionId": req.DecisionID, "decisionReason": strings.TrimSpace(req.Reason)},
	}
	addActionOrigin(event.Payload, call)
	record := &models.ApprovalResolutionRecord{
		Approval: updatedApproval, ExpectedApprovalRevision: approval.Revision,
		Call: updatedCall, ExpectedCallRevision: call.Revision, Event: event,
	}
	// A nested decision, or a late expiry of a call whose Run ended or moved
	// on, resolves without the Run.
	if nested || !callWaiting || !(held || parked || pausedParked) {
		return record, nil, nil
	}
	updatedRun := models.CloneAgentRun(run)
	if denied && updatedRun.Budget != nil {
		if _, reserved := updatedRun.BudgetReservations[agentrun.ActionBudgetReservationID(call.ID)]; reserved {
			if err := models.ReleaseRunBudgetReservation(updatedRun, agentrun.ActionBudgetReservationID(call.ID)); err != nil {
				return nil, nil, err
			}
		}
	}
	switch {
	case parked:
		updatedRun.Status = models.AgentRunStatusQueued
		updatedRun.WakeCondition = nil
		updatedRun.AvailableAt = now
		updatedRun.QueueEnteredAt = now
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
		updatedRun.LastWakeSignalID = req.DecisionID
	case pausedParked:
		// The operator's pause stands; resume queues the Run, which then
		// reads the decided tool entry.
		updatedRun.PausedFrom = models.AgentRunStatusQueued
		updatedRun.PausedWakeCondition = nil
		updatedRun.LastWakeSignalID = req.DecisionID
	}
	updatedRun.UpdatedAt = now
	updatedRun.Revision++
	record.Run = updatedRun
	record.ExpectedRunRevision = run.Revision
	return record, nil, nil
}
