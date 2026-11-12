package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type ActionDisposition string

const (
	ActionDispositionAllow           ActionDisposition = "allow"
	ActionDispositionDeny            ActionDisposition = "deny"
	ActionDispositionRequireApproval ActionDisposition = "require_approval"
)

type ActionPolicyInput struct {
	Run               *AgentRun
	Bound             *skill.BoundAction
	Arguments         map[string]interface{}
	ExternalOperation *ExternalOperationIdentity
	Actor             ActivityActor
	Summary           string
	// ConversationID is the canonical conversation the Run acts for (its own
	// context, else its root Run's); empty when it acts for none.
	ConversationID string
	// ApprovalMode is that conversation's approval mode. It is empty when the
	// Run acts for no conversation, which policies treat as manual.
	ApprovalMode ConversationApprovalMode
	// RoutineGrant is the grant from the routine brief the user approved that
	// covers this exact action (including its owner-only recipients), when
	// the Run is an occurrence of that routine. Policies allow a covered
	// action in every approval mode; a review: always action is never covered.
	RoutineGrant *RoutineGrant
}

type ActionPolicyDecision struct {
	Disposition          ActionDisposition
	Reason               string
	EligibleApprovers    []ApprovalPrincipal
	ApprovalTTL          time.Duration
	ApprovalTimeout      ApprovalTimeoutDecision
	ApprovalDestinations []ApprovalDestination
	// Review is a host review floor for this one call, such as a person's
	// "always ask me" setting. "always" makes the call exactly like a
	// manifest review: always action: it is persisted on the call and its
	// approval card, an Allow becomes RequireApproval, no approval mode
	// waives it and its checkpoint never auto-approves on timeout.
	Review skill.ActionReview `json:"review,omitempty"`
}

type ActionPolicyEvaluator interface {
	EvaluateAction(context.Context, ActionPolicyInput) (ActionPolicyDecision, error)
}

type ActionPolicyEvaluatorFunc func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error)

func (f ActionPolicyEvaluatorFunc) EvaluateAction(ctx context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
	return f(ctx, input)
}

type ActionCatalog interface {
	Resolve(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundAction, error)
	ValidateInput(context.Context, *skill.BoundAction, map[string]interface{}) error
}

// ActionProposalValidator applies deterministic kernel rules that cannot be
// expressed by a JSON schema. Validators run before policy evaluation and
// persistence, so an invalid or out-of-scope model proposal never becomes an
// approval request. A validator may return a typed, secret-safe approval
// preview; the last non-nil preview replaces the generic Skill invocation.
type ActionProposalValidator interface {
	ValidateActionProposal(context.Context, ActionProposalValidationInput) (map[string]interface{}, error)
}

// ActionProposalArgumentResolver lets a deterministic kernel validator add
// arguments that belong to the runtime rather than the model. Resolvers run
// before JSON Schema validation; the resolved arguments are then used for
// validation, policy, approval previews, persistence, and execution. This is
// intentionally separate from model input so concurrency tokens and similar
// implementation details never need to become user-facing prompt fields.
type ActionProposalArgumentResolver interface {
	ResolveActionProposalArguments(context.Context, ActionProposalValidationInput) (map[string]interface{}, bool, error)
}

type ProposeActionRequest struct {
	Scope Scope
	RunID string
	// TurnID is the step that proposes the call (its agent_turns row).
	TurnID string
	// WorkerID is the Run lease owner.
	WorkerID          string
	DeploymentID      string
	BindingID         string
	BindingRevision   int64
	SkillID           string
	SkillVersion      string
	Action            string
	Arguments         map[string]interface{}
	PreparedRuntime   *skill.PreparedRuntime
	IdempotencyKey    string
	Summary           string
	Actor             ActivityActor
	EvidenceRefs      []string
	ExternalOperation *ExternalOperationIdentity
	ReviewContext     *ApprovalReviewContext
	CorrelationID     string
	CausationID       string
	// Origin is ActionOriginLoop for a governed tool call of a run loop step
	// (Execution ActionExecutionLoop) or ActionOriginVM for a nested call
	// (see ProposeNested), whose OriginRef groups it under its command.
	// Neither changes the Run apart from a loop call's budget reservation.
	Origin    ActionOrigin
	OriginRef string
	// Execution ActionExecutionHost marks a nested call its host performs
	// itself (see ProposeNestedHost); ActionExecutionLoop marks a loop call.
	Execution ActionExecution
}

type ActionCoordinator struct {
	portfolio     PortfolioStore
	actions       ActionStore
	conversations conversationReader
	routines      routineGrantStore
	owners        RoutineOwnerAddressResolver
	catalog       ActionCatalog
	policy        ActionPolicyEvaluator
	validators    []ActionProposalValidator
	now           func() time.Time
	newID         func() string
	nested        nestedActionTurns
	// nestedMaxCalls and nestedMaxConcurrent bound nested calls per step.
	nestedMaxCalls, nestedMaxConcurrent int
}

func NewActionCoordinator(portfolio PortfolioStore, actions ActionStore, catalog ActionCatalog, policy ActionPolicyEvaluator, validators ...ActionProposalValidator) *ActionCoordinator {
	portable := []ActionProposalValidator{KernelResolvedActionArgumentResolver{}, BindingActionArgumentResolver{}, EmbedSessionActionAuthorityValidator{}, RequiredActionEvidenceValidator{}, ObservationRefActionProposalValidator{}}
	portable = append(portable, validators...)
	coordinator := &ActionCoordinator{
		portfolio: portfolio, actions: actions, catalog: catalog, policy: policy, validators: portable, now: time.Now, newID: uuid.NewString,
		nestedMaxCalls: MaxNestedActionCallsPerTurn, nestedMaxConcurrent: MaxConcurrentNestedActionCalls,
	}
	// Conversation approval modes are read from the same kernel store when it
	// keeps conversations; a store without them has no modes (manual).
	if conversations, ok := portfolio.(conversationReader); ok {
		coordinator.conversations = conversations
	} else if conversations, ok := actions.(conversationReader); ok {
		coordinator.conversations = conversations
	}
	if routines, ok := portfolio.(routineGrantStore); ok {
		coordinator.routines = routines
	} else if routines, ok := actions.(routineGrantStore); ok {
		coordinator.routines = routines
	}
	return coordinator
}

// SetRoutineOwnerAddresses lets owner-only routine grants check recipients.
func (c *ActionCoordinator) SetRoutineOwnerAddresses(resolver RoutineOwnerAddressResolver) {
	if c != nil {
		c.owners = resolver
	}
}

// Propose governs one action call of a running Run: catalog, validators,
// external operation identity, approval mode, routine grant and policy, then
// records the call with its approval and activity. A loop call never changes
// the Run apart from its budget reservation; a nested call never changes it.
// A *ActionProposalRejection is a reason the model can correct; any other
// error stops the caller's claim.
func (c *ActionCoordinator) Propose(ctx context.Context, req ProposeActionRequest) (*ActionProposalResult, error) {
	if c == nil || c.portfolio == nil || c.actions == nil || c.catalog == nil || c.policy == nil {
		return nil, errors.New("action coordinator is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.DeploymentID) == "" || strings.TrimSpace(req.SkillID) == "" || strings.TrimSpace(req.SkillVersion) == "" || strings.TrimSpace(req.Action) == "" {
		return nil, errors.New("run, deployment, skill, version, and action are required")
	}
	loop := req.Origin == ActionOriginLoop
	if err := actionSkillRuntimeMaintenanceError(ctx, c.actions, req.Scope, req.SkillID); err != nil {
		return nil, err
	}
	run, err := c.portfolio.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.Status != AgentRunStatusRunning {
		if loop {
			return nil, ErrLeaseLost
		}
		return nil, fmt.Errorf("action can only be proposed by a running run, got %s", run.Status)
	}
	switch req.Origin {
	case ActionOriginVM:
		if req.Execution != "" && req.Execution != ActionExecutionHost {
			return nil, errors.New("a nested action is performed by an action worker or its host")
		}
		if strings.TrimSpace(req.TurnID) == "" || strings.TrimSpace(req.WorkerID) == "" || req.ExternalOperation != nil {
			return nil, errors.New("a nested action requires its step and worker and takes no external operation")
		}
	case ActionOriginLoop:
		if req.Execution != ActionExecutionLoop || strings.TrimSpace(req.TurnID) == "" || strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.OriginRef) != "" {
			return nil, errors.New("a loop action requires its step, the Run lease owner, loop execution and no origin reference")
		}
		if strings.TrimSpace(req.IdempotencyKey) == "" {
			return nil, errors.New("a loop action requires its tool call's idempotency key")
		}
	default:
		return nil, errors.New("action origin is invalid")
	}
	var lease *AgentRunLeaseGuard
	now := c.now().UTC()
	if loop || run.LeaseOwner != "" {
		if run.LeaseOwner != req.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
			return nil, ErrLeaseLost
		}
		lease = &AgentRunLeaseGuard{WorkerID: req.WorkerID, Now: now}
	}
	selection := []skill.BindingReference(nil)
	if req.BindingID != "" || req.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: req.BindingID, Revision: req.BindingRevision})
	}
	bound, err := c.catalog.Resolve(skill.WithRunActor(ctx, RunSkillActor(run)), skill.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}, req.DeploymentID, req.SkillID, req.SkillVersion, req.Action, selection...)
	if err != nil {
		// Maintenance may have started while the catalog was being read. Return
		// its durable continuation instead of a transient stale-version error.
		if maintenanceErr := actionSkillRuntimeMaintenanceError(ctx, c.actions, req.Scope, req.SkillID); maintenanceErr != nil {
			return nil, maintenanceErr
		}
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, rejectProposal(ProposalRejectionCapabilityUnavailable, err)
	}
	if canonical := canonicalActionReview(bound.Definition, bound.Action); canonical.Review != bound.Action.Review {
		reviewed := *bound
		reviewed.Action = canonical
		bound = &reviewed
	}
	history := newRunActionHistory(c.actions, req.Scope, run.ID)
	arguments := maps.Clone(req.Arguments)
	for _, validator := range c.validators {
		resolver, ok := validator.(ActionProposalArgumentResolver)
		if !ok || resolver == nil {
			continue
		}
		resolved, handled, resolveErr := resolver.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
			Run: cloneAgentRun(run), Bound: bound, Arguments: maps.Clone(arguments), History: history, IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		})
		if resolveErr != nil {
			return nil, proposalValidationError(ctx, ProposalRejectionToolArgumentsInvalid, resolveErr)
		}
		if handled {
			arguments = maps.Clone(resolved)
		}
	}
	if err := c.catalog.ValidateInput(ctx, bound, arguments); err != nil {
		return nil, proposalValidationError(ctx, ProposalRejectionToolArgumentsInvalid, err)
	}
	var proposedAction map[string]interface{}
	for _, validator := range c.validators {
		if validator == nil {
			continue
		}
		preview, validateErr := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: cloneAgentRun(run), Bound: bound, Arguments: maps.Clone(arguments), History: history, IdempotencyKey: strings.TrimSpace(req.IdempotencyKey)})
		if validateErr != nil {
			return nil, proposalValidationError(ctx, ProposalRejectionNotAdmitted, validateErr)
		}
		if preview != nil {
			proposedAction = maps.Clone(preview)
		}
	}
	if bound.Action.Idempotency == skill.IdempotencyRequired && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, errors.New("skill action requires an idempotency key")
	}
	externalOperationPolicy := effectiveExternalOperationPolicy(bound.Action.SideEffect, bound.Action.ExternalOperationPolicy)
	if externalOperationPolicy == skill.ExternalOperationRequired && req.ExternalOperation == nil {
		return nil, rejectProposal(ProposalRejectionExternalIdentityInvalid, errors.New("skill action requires an external operation identity"))
	}
	if externalOperationPolicy == skill.ExternalOperationForbidden && req.ExternalOperation != nil {
		return nil, rejectProposal(ProposalRejectionExternalIdentityInvalid, errors.New("skill action forbids an external operation identity"))
	}
	externalOperationDigest, err := computeExternalOperationDigest(req.Scope, run.Owner, run.ObjectiveID, req.ExternalOperation, req.IdempotencyKey)
	if err != nil {
		return nil, rejectProposal(ProposalRejectionExternalIdentityInvalid, err)
	}
	if externalOperationDigest != "" {
		prior, lookupErr := c.actions.GetActionCallByExternalOperation(ctx, req.Scope, externalOperationDigest)
		if lookupErr != nil && !errors.Is(lookupErr, ErrActionNotFound) {
			return nil, lookupErr
		}
		// The prior is this same call when the tool call is replayed; the
		// store returns it by its idempotency key.
		if prior != nil && !replaysActionCall(prior, run, req.IdempotencyKey) {
			if externalOperationReceiptComplete(prior.Status) {
				return c.suppressDuplicateExternalOperation(ctx, req, run, bound, arguments, externalOperationDigest, prior, lease, now)
			}
			return nil, &ExternalOperationConflictError{Prior: prior}
		}
	}
	conversationID, approvalMode, err := RunConversationApprovalMode(ctx, c.portfolio, c.conversations, run)
	if err != nil {
		return nil, fmt.Errorf("resolve conversation approval mode: %w", err)
	}
	routineGrant, err := resolveRoutineGrant(ctx, c.portfolio, c.routines, c.owners, run, bound, arguments)
	if err != nil {
		return nil, fmt.Errorf("resolve routine grant: %w", err)
	}
	decision, err := c.policy.EvaluateAction(ctx, ActionPolicyInput{
		Run: cloneAgentRun(run), Bound: bound, Arguments: maps.Clone(arguments), ExternalOperation: req.ExternalOperation, Actor: req.Actor, Summary: req.Summary,
		ConversationID: conversationID, ApprovalMode: approvalMode, RoutineGrant: routineGrant,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate action policy: %w", err)
	}
	if !validActionDisposition(decision.Disposition) {
		return nil, errors.New("action policy returned an invalid disposition")
	}
	// A manifest review floor is a kernel invariant, not a policy preference:
	// no policy, standing grant, or approval mode may let the action proceed
	// unreviewed, and its checkpoint never auto-approves on timeout.
	// A builder's skill change aimed at another Agent is reviewed the same
	// way: it changes that Agent, so the asking person approves it.
	targeted := isTargetedSkillBindingAction(bound, arguments)
	if bound.Action.AlwaysReview() || isAgentBuilderApprovalAction(bound) || targeted {
		if decision.Disposition == ActionDispositionAllow {
			return nil, fmt.Errorf("action policy allowed %s.%s, which always requires explicit review", req.SkillID, req.Action)
		}
		if decision.ApprovalTimeout == ApprovalTimeoutApprove {
			decision.ApprovalTimeout = ApprovalTimeoutExpire
		}
	}
	// A host review floor raises this one call to review: always. The policy
	// asked for review itself, so its own Allow becomes a request.
	review := bound.Action.Review
	switch decision.Review {
	case "":
	case skill.ActionReviewAlways:
		review = skill.ActionReviewAlways
		if decision.Disposition == ActionDispositionAllow {
			decision.Disposition = ActionDispositionRequireApproval
		}
		if decision.Disposition == ActionDispositionRequireApproval {
			decision.ApprovalTimeout = ApprovalTimeoutExpire
		}
	default:
		return nil, fmt.Errorf("action policy returned unsupported review %q", decision.Review)
	}
	// An action that acts as one person's own account is approved only by
	// that person, whatever the policy named.
	if decision.Disposition == ActionDispositionRequireApproval && run.ActingUser != "" && boundUsesPersonCredential(bound) {
		decision.EligibleApprovers = []ApprovalPrincipal{{Type: "user", ID: run.ActingUser}}
	}
	// An agent or team card is decided by the person who asked the builder
	// for it (D9), whatever role the host policy named.
	if decision.Disposition == ActionDispositionRequireApproval && run.ActingUser != "" && (isAgentBuilderApprovalAction(bound) || targeted) {
		decision.EligibleApprovers = []ApprovalPrincipal{{Type: "user", ID: run.ActingUser}}
	}
	if decision.Disposition == ActionDispositionRequireApproval && len(decision.EligibleApprovers) == 0 {
		return nil, errors.New("approval policy must identify at least one eligible approver")
	}
	// A person reads the summary on the approval card. The kernel never
	// writes one for the model.
	if loop && decision.Disposition == ActionDispositionRequireApproval && strings.TrimSpace(req.Summary) == "" {
		return nil, &ActionProposalRejection{Code: ProposalRejectionReviewSummaryRequired, Message: proposalRejectionReviewSummaryRequiredMessage}
	}

	callID := c.newID()
	call := &ActionCall{
		ID: callID, Scope: req.Scope, RunID: run.ID, TurnID: req.TurnID, DeploymentID: req.DeploymentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
		SkillID: req.SkillID, SkillVersion: req.SkillVersion, Action: req.Action, Origin: req.Origin, OriginRef: strings.TrimSpace(req.OriginRef), Execution: req.Execution,
		Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Review: review,
		Arguments:         persistedActionArguments(arguments, bound.Action.InputSchema),
		ResolvedArguments: bindingResolvedArgumentProvenance(bound),
		PreparedRuntime:   clonePreparedRuntime(req.PreparedRuntime),
		CredentialRefs:    boundCredentialReferences(bound), EvidenceRefs: append([]string(nil), req.EvidenceRefs...), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		ExternalOperationDigest: externalOperationDigest,
		MaxAttempts:             1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	if !loop {
		return c.createNestedProposal(ctx, req, run, bound, arguments, proposedAction, call, decision, lease, now)
	}
	result, err := c.createLoopProposal(ctx, req, run, bound, arguments, proposedAction, call, decision, lease, now)
	var conflict *ExternalOperationConflictError
	if errors.As(err, &conflict) && conflict.Prior != nil && externalOperationReceiptComplete(conflict.Prior.Status) && !replaysActionCall(conflict.Prior, run, req.IdempotencyKey) {
		return c.suppressDuplicateExternalOperation(ctx, req, run, bound, arguments, externalOperationDigest, conflict.Prior, lease, now)
	}
	return result, err
}

// proposalValidationError is err as a model-visible rejection with code,
// unless the proposal's context ended.
func proposalValidationError(ctx context.Context, code string, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return rejectProposal(code, err)
}

// replaysActionCall reports whether prior is the call a replay of the same
// tool call (same Run, same idempotency key) already recorded.
func replaysActionCall(prior *ActionCall, run *AgentRun, idempotencyKey string) bool {
	key := strings.TrimSpace(idempotencyKey)
	return prior != nil && run != nil && key != "" && prior.RunID == run.ID && prior.IdempotencyKey == key
}

// createLoopProposal records a loop call with its disposition, approval and
// activity. The Run is changed only to reserve the call's budget; an approval
// parks the Run later, at step end.
func (c *ActionCoordinator) createLoopProposal(ctx context.Context, req ProposeActionRequest, run *AgentRun,
	bound *skill.BoundAction, arguments, proposedAction map[string]interface{}, call *ActionCall,
	decision ActionPolicyDecision, lease *AgentRunLeaseGuard, now time.Time) (*ActionProposalResult, error) {
	if lease == nil {
		return nil, ErrLeaseLost
	}
	var updatedRun *AgentRun
	budgetExhausted := false
	if run.Budget != nil && decision.Disposition != ActionDispositionDeny {
		reserved := cloneAgentRun(run)
		reserved.Revision++
		reserved.UpdatedAt = now
		err := reserveRunBudget(reserved, BudgetReservation{
			ID: actionBudgetReservationID(call.ID), Usage: BudgetUsage{Actions: 1}, CreatedAt: now,
		})
		switch {
		case errors.Is(err, ErrBudgetExhausted):
			budgetExhausted = true
			decision.Disposition = ActionDispositionDeny
			decision.Reason = actionBudgetExhaustedReason
		case err != nil:
			return nil, err
		default:
			updatedRun = reserved
		}
	}
	eventType := "action.proposed"
	summary := strings.TrimSpace(req.Summary)
	eventSummary := summary
	if eventSummary == "" {
		eventSummary = fmt.Sprintf("Proposed %s.%s", req.SkillID, req.Action)
	}
	var approval *ApprovalCheckpoint
	switch decision.Disposition {
	case ActionDispositionAllow:
		call.Status = ActionCallStatusReady
	case ActionDispositionDeny:
		call.Status = ActionCallStatusDenied
		call.Error = decision.Reason
		call.CompletedAt = &now
		eventType = "action.denied"
		if budgetExhausted {
			eventType = "budget.exhausted"
			eventSummary = "The Run's action budget is exhausted, so the action was not run"
		}
	case ActionDispositionRequireApproval:
		approval = c.newApprovalCheckpoint(ctx, req, run, bound, arguments, proposedAction, call, decision, summary, now)
		eventType = "action.approval_requested"
	}
	record := ActionProposalRecord{
		Call: call, Approval: approval, Run: updatedRun, ExpectedRunRevision: run.Revision, Lease: lease,
		Event:               c.proposalEvent(req, run, call, decision, eventType, eventSummary, now),
		RequireBindingFence: usesDurableActionBindings(c.catalog), HostBinding: hostActionBinding(bound),
	}
	result, err := c.actions.CreateActionProposal(ctx, record)
	if err != nil {
		return nil, err
	}
	if result != nil {
		if result.Created {
			result.BudgetExhausted = budgetExhausted
		} else if existing := result.Call; existing != nil {
			// A replay of a budget denial must still map to the budget pause.
			result.BudgetExhausted = existing.Status == ActionCallStatusDenied && existing.Error == actionBudgetExhaustedReason
		}
	}
	return result, nil
}

// actionBudgetExhaustedReason is the denial reason of a loop call refused for
// action budget; replays recognize the denial by it.
const actionBudgetExhaustedReason = "run action budget is exhausted"

// createNestedProposal records a nested call with the same disposition,
// approval and activity a model proposal would get, without touching the Run.
// The store fences it against the running step and its bounds.
func (c *ActionCoordinator) createNestedProposal(ctx context.Context, req ProposeActionRequest, run *AgentRun, bound *skill.BoundAction, arguments, proposedAction map[string]interface{}, call *ActionCall, decision ActionPolicyDecision, lease *AgentRunLeaseGuard, now time.Time) (*ActionProposalResult, error) {
	if lease == nil {
		return nil, ErrNestedTurnNotActive
	}
	eventType := "action.proposed"
	summary := req.Summary
	if summary == "" {
		summary = fmt.Sprintf("Proposed %s.%s", req.SkillID, req.Action)
	}
	var approval *ApprovalCheckpoint
	switch decision.Disposition {
	case ActionDispositionAllow:
		call.Status = ActionCallStatusReady
	case ActionDispositionDeny:
		call.Status = ActionCallStatusDenied
		call.Error = decision.Reason
		call.CompletedAt = &now
		eventType = "action.denied"
	case ActionDispositionRequireApproval:
		approval = c.newApprovalCheckpoint(ctx, req, run, bound, arguments, proposedAction, call, decision, summary, now)
		eventType = "action.approval_requested"
	}
	return c.actions.CreateActionProposal(ctx, ActionProposalRecord{
		Call: call, Approval: approval, Lease: lease, Event: c.proposalEvent(req, run, call, decision, eventType, summary, now),
		RequireBindingFence: usesDurableActionBindings(c.catalog), HostBinding: hostActionBinding(bound),
		Nested: &NestedActionFence{WorkerID: lease.WorkerID, Now: now, MaxCalls: c.nestedMaxCalls, MaxConcurrent: c.nestedMaxConcurrent},
	})
}

// agentBuilderApprovalTTL is how long an agent or team card stays open. The
// card is the builder's draft, so it lasts as long as a draft would rather
// than the 24 hour default.
const agentBuilderApprovalTTL = 30 * 24 * time.Hour

// isAgentBuilderApprovalAction reports whether bound is a builder action
// whose approval card creates or changes an Agent or a Team: create_agent,
// update_agent and create_team.
func isAgentBuilderApprovalAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil {
		return false
	}
	switch bound.Definition.ID {
	case AgentManagementSkillID:
		return bound.Action.Name == AgentActionCreateAgent || bound.Action.Name == AgentActionUpdateAgent
	case TeamManagementSkillID:
		return bound.Action.Name == TeamActionCreateTeam
	}
	return false
}

// newApprovalCheckpoint puts call in review and returns its pending approval.
func (c *ActionCoordinator) newApprovalCheckpoint(ctx context.Context, req ProposeActionRequest, run *AgentRun, bound *skill.BoundAction, arguments, proposedAction map[string]interface{}, call *ActionCall, decision ActionPolicyDecision, summary string, now time.Time) *ApprovalCheckpoint {
	approvalID := c.newID()
	ttl := decision.ApprovalTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	timeout := decision.ApprovalTimeout
	destinations := append([]ApprovalDestination(nil), decision.ApprovalDestinations...)
	if isAgentBuilderApprovalAction(bound) {
		// An agent or team card is the draft itself: it stays open as long
		// as a draft would, is decided only in the app, and lapses rather
		// than approves when nobody answers.
		ttl, timeout, destinations = agentBuilderApprovalTTL, ApprovalTimeoutExpire, nil
	} else if isTargetedSkillBindingAction(bound, arguments) {
		// A skill change aimed at another Agent is decided only in the app
		// and lapses rather than approves when nobody answers.
		timeout, destinations = ApprovalTimeoutExpire, nil
	}
	call.Status = ActionCallStatusWaitingApproval
	call.ApprovalID = approvalID
	approval := &ApprovalCheckpoint{
		ID: approvalID, Scope: req.Scope, RunID: run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: bound.Action.Risk, Summary: summary, PolicyReason: decision.Reason, ProposedAction: proposedAction,
		EvidenceRefs: append([]string(nil), req.EvidenceRefs...), EligibleApprovers: append([]ApprovalPrincipal(nil), decision.EligibleApprovers...),
		Destinations: destinations,
		ExpiresAt:    now.Add(ttl), TimeoutDecision: timeout,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if approval.ProposedAction == nil {
		approval.ProposedAction = approvalPreview(bound, arguments)
	}
	if call.Review == skill.ActionReviewAlways {
		// The reviewer must see exactly what they consent to (for a payment:
		// amount, currency, merchant) even when a validator supplied its own
		// preview. Sensitive inputs remain redacted. The call carries the
		// manifest's review floor and any the host policy added.
		if _, ok := approval.ProposedAction["arguments"]; !ok {
			approval.ProposedAction["arguments"] = sanitizeApprovalValue(arguments, bound.Action.InputSchema)
		}
		approval.ProposedAction["review"] = string(call.Review)
	}
	approval.ProposedAction = c.enrichApprovalPreview(ctx, approval.ProposedAction, run, req.EvidenceRefs, req.ExternalOperation)
	if req.ReviewContext != nil {
		approval.ProposedAction["reviewContext"] = approvalReviewContextMap(req.ReviewContext)
	} else if review := intentApprovalReviewContext(approval.ProposedAction, req.ExternalOperation); review != nil {
		approval.ProposedAction["reviewContext"] = approvalReviewContextMap(review)
	}
	return approval
}

// proposalEvent is the activity recorded with a proposal. A nested call's
// event carries its origin so a feed can group it under its command.
func (c *ActionCoordinator) proposalEvent(req ProposeActionRequest, run *AgentRun, call *ActionCall, decision ActionPolicyDecision, eventType, summary string, now time.Time) *ActivityEvent {
	actor := req.Actor
	if actor.Type == "" {
		actor = ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: req.TurnID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: actor, Summary: summary, Visibility: ActivityVisibilityScope,
		Payload:       map[string]interface{}{"actionCallId": call.ID, "approvalId": call.ApprovalID, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "risk": call.Risk},
		CorrelationID: req.CorrelationID, CausationID: req.CausationID, CreatedAt: now,
	}
	addActionOrigin(event.Payload, call)
	if call.PreparedRuntime != nil {
		event.Payload["preparedRuntime"] = clonePreparedRuntime(call.PreparedRuntime)
	}
	if decision.Reason != "" && decision.Disposition != ActionDispositionAllow {
		event.Payload["policyReason"] = decision.Reason
	}
	return event
}

// addActionOrigin marks the activity of a nested or loop call with its origin,
// grouping reference and execution.
func addActionOrigin(payload map[string]interface{}, call *ActionCall) {
	if call == nil || call.Origin == "" {
		return
	}
	payload["origin"] = string(call.Origin)
	if call.OriginRef != "" {
		payload["originRef"] = call.OriginRef
	}
	if call.Execution != "" {
		payload["execution"] = string(call.Execution)
	}
}

// suppressDuplicateExternalOperation records a succeeded receipt call that
// points at a completed prior external operation instead of running it
// again. The Run is not changed.
func (c *ActionCoordinator) suppressDuplicateExternalOperation(ctx context.Context, req ProposeActionRequest, run *AgentRun, bound *skill.BoundAction, arguments map[string]interface{}, digest string, prior *ActionCall, lease *AgentRunLeaseGuard, now time.Time) (*ActionProposalResult, error) {
	if lease == nil {
		return nil, ErrLeaseLost
	}
	call := &ActionCall{
		ID: c.newID(), Scope: req.Scope, RunID: run.ID, TurnID: req.TurnID, DeploymentID: req.DeploymentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision, SkillID: req.SkillID, SkillVersion: req.SkillVersion,
		Action: req.Action, Origin: req.Origin, Execution: req.Execution, Status: ActionCallStatusSucceeded, Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect,
		Arguments: persistedActionArguments(arguments, bound.Action.InputSchema), PreparedRuntime: clonePreparedRuntime(req.PreparedRuntime),
		ResolvedArguments: bindingResolvedArgumentProvenance(bound),
		EvidenceRefs:      append([]string(nil), req.EvidenceRefs...), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		ExternalOperationDigest: digest, DuplicateOfActionCallID: prior.ID,
		Output:      map[string]interface{}{"duplicateSuppressed": true, "priorActionCallId": prior.ID, "priorRunId": prior.RunID},
		MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	actor := req.Actor
	if actor.Type == "" {
		actor = ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: "action.duplicate_suppressed", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: req.TurnID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: actor, Summary: "Reused a prior external-operation receipt; no external action was executed",
		Visibility: ActivityVisibilityScope, CorrelationID: req.CorrelationID, CausationID: req.CausationID, CreatedAt: now,
		Payload: map[string]interface{}{"actionCallId": call.ID, "priorActionCallId": prior.ID, "priorRunId": prior.RunID,
			"externalOperationDigest": digest, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action},
	}
	addActionOrigin(event.Payload, call)
	return c.actions.CreateActionProposal(ctx, ActionProposalRecord{Call: call, ExpectedRunRevision: run.Revision, Lease: lease, Event: event, RequireBindingFence: usesDurableActionBindings(c.catalog), HostBinding: hostActionBinding(bound)})
}

// hostActionBinding is the binding a call resolved through when the host
// supplied it for the run rather than storing it.
func hostActionBinding(bound *skill.BoundAction) *skill.Binding {
	if bound == nil || !bound.Host {
		return nil
	}
	return bound.Binding
}

func usesDurableActionBindings(catalog interface{}) bool {
	configured, ok := catalog.(interface{ UsesDurableBindings() bool })
	return ok && configured.UsesDurableBindings()
}

// persistedActionArguments strips values that must be supplied through the
// credential resolver. Raw sensitive input remains available only during the
// ephemeral validation/policy decision that precedes proposal persistence.
func persistedActionArguments(arguments map[string]interface{}, schema map[string]interface{}) map[string]interface{} {
	properties, _ := schema["properties"].(map[string]interface{})
	result := make(map[string]interface{}, len(arguments))
	for key, value := range arguments {
		childSchema, _ := properties[key].(map[string]interface{})
		if schemaSensitive(childSchema) || (sensitiveFieldName(key) && !schemaPublicEnum(childSchema)) {
			continue
		}
		result[key] = persistedActionValue(value, childSchema)
	}
	return result
}

func persistedActionValue(value interface{}, schema map[string]interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return persistedActionArguments(typed, schema)
	case []interface{}:
		itemSchema, _ := schema["items"].(map[string]interface{})
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = persistedActionValue(child, itemSchema)
		}
		return result
	default:
		return typed
	}
}

func validActionDisposition(value ActionDisposition) bool {
	return value == ActionDispositionAllow || value == ActionDispositionDeny || value == ActionDispositionRequireApproval
}

func boundCredentialReferences(bound *skill.BoundAction) map[string]skill.CredentialReference {
	result := make(map[string]skill.CredentialReference)
	for _, requirement := range bound.Action.Credentials {
		if reference, ok := bound.Binding.Credentials[requirement.Name]; ok {
			result[requirement.Name] = reference
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// boundUsesPersonCredential reports whether any credential slot the action
// uses holds the per_person reference.
func boundUsesPersonCredential(bound *skill.BoundAction) bool {
	for _, reference := range boundCredentialReferences(bound) {
		if skill.IsPersonCredentialReference(reference) {
			return true
		}
	}
	return false
}

func approvalPreview(bound *skill.BoundAction, arguments map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"skillId": bound.Definition.ID, "skillVersion": bound.Definition.Version, "action": bound.Action.Name,
		"risk": bound.Action.Risk, "sideEffect": bound.Action.SideEffect,
		"arguments": sanitizeApprovalValue(arguments, bound.Action.InputSchema).(map[string]interface{}),
	}
}

// enrichApprovalPreview carries the exact, already-persisted inputs that
// prepared an approval-backed effect into its immutable review record. Action
// arguments have already crossed the Skill schema's persistence boundary, so
// credential values are absent. The additional conservative scrub protects
// records created by older hosts before that boundary was enforced.
//
// Keeping this in the kernel means Studio, a TUI, Slack, and future approval
// destinations all review the same proposal instead of provider adapters
// guessing what a preceding Skill action meant.
func (c *ActionCoordinator) enrichApprovalPreview(
	ctx context.Context,
	preview map[string]interface{},
	run *AgentRun,
	evidenceRefs []string,
	externalOperation *ExternalOperationIdentity,
) map[string]interface{} {
	result := maps.Clone(preview)
	if result == nil {
		result = map[string]interface{}{}
	}
	if externalOperation != nil {
		resource, err := canonicalExternalOperationResource(externalOperation.Resource)
		if err == nil {
			result["externalOperation"] = map[string]interface{}{
				"resource": resource, "operation": strings.ToLower(strings.TrimSpace(externalOperation.Operation)),
			}
		}
	}
	if c == nil || c.actions == nil || run == nil {
		return result
	}
	prepared := make([]interface{}, 0, len(evidenceRefs))
	for _, reference := range evidenceRefs {
		actionID, ok := strings.CutPrefix(strings.TrimSpace(reference), "action-call:")
		if !ok || strings.TrimSpace(actionID) == "" {
			continue
		}
		call, err := c.actions.GetActionCall(ctx, run.Scope, strings.TrimSpace(actionID))
		if err != nil || call == nil || call.RunID != run.ID || call.Status != ActionCallStatusSucceeded {
			continue
		}
		prepared = append(prepared, map[string]interface{}{
			"actionCallId": call.ID,
			"skillId":      call.SkillID,
			"skillVersion": call.SkillVersion,
			"action":       call.Action,
			"arguments":    sanitizePersistedApprovalValue(call.Arguments),
		})
	}
	if len(prepared) > 0 {
		result["preparedEvidence"] = prepared
	}
	return result
}

func sanitizePersistedApprovalValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			if sensitiveFieldName(key) {
				result[key] = "[REDACTED]"
				continue
			}
			result[key] = sanitizePersistedApprovalValue(child)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = sanitizePersistedApprovalValue(child)
		}
		return result
	default:
		return typed
	}
}

func sanitizeApprovalValue(value interface{}, schema map[string]interface{}) interface{} {
	if schemaSensitive(schema) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		properties, _ := schema["properties"].(map[string]interface{})
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			childSchema, _ := properties[key].(map[string]interface{})
			if schemaSensitive(childSchema) || (sensitiveFieldName(key) && !schemaPublicEnum(childSchema)) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = sanitizeApprovalValue(child, childSchema)
			}
		}
		return result
	case []interface{}:
		itemSchema, _ := schema["items"].(map[string]interface{})
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = sanitizeApprovalValue(child, itemSchema)
		}
		return result
	default:
		return typed
	}
}

func schemaSensitive(schema map[string]interface{}) bool {
	if schema == nil {
		return false
	}
	writeOnly, _ := schema["writeOnly"].(bool)
	extension, _ := schema["x-sensitive"].(bool)
	format, _ := schema["format"].(string)
	return writeOnly || extension || format == "password"
}

// schemaPublicEnum identifies a selector whose complete value set is already
// part of the public Skill contract. A validated value such as
// credentialField=username selects which opaque binding field the trusted host
// may resolve; it is not credential material and must survive persistence and
// approval. Explicit sensitive schema annotations always take precedence.
func schemaPublicEnum(schema map[string]interface{}) bool {
	if schema == nil {
		return false
	}
	switch values := schema["enum"].(type) {
	case []interface{}:
		return len(values) > 0
	case []string:
		return len(values) > 0
	default:
		return false
	}
}

func sensitiveFieldName(value string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(value, "_", ""), "-", ""))
	for _, fragment := range []string{"password", "passwd", "secret", "token", "apikey", "privatekey", "credential"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}
