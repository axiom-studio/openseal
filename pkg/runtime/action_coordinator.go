package runtime

import (
	"context"
	"errors"
	"fmt"
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
}

type ActionPolicyDecision struct {
	Disposition          ActionDisposition
	Reason               string
	EligibleApprovers    []ApprovalPrincipal
	ApprovalTTL          time.Duration
	ApprovalTimeout      ApprovalTimeoutDecision
	ApprovalDestinations []ApprovalDestination
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

type ActionProposalValidationInput struct {
	Run       *AgentRun
	Bound     *skill.BoundAction
	Arguments map[string]interface{}
}

type ProposeActionRequest struct {
	Scope                  Scope
	RunID                  string
	TurnID                 string
	WorkerID               string
	DeploymentID           string
	AssignedAgentID        string
	BindingID              string
	BindingRevision        int64
	SkillID                string
	SkillVersion           string
	Action                 string
	Arguments              map[string]interface{}
	PreparedRuntime        *skill.PreparedRuntime
	IdempotencyKey         string
	Summary                string
	Actor                  ActivityActor
	EvidenceRefs           []string
	ExternalOperation      *ExternalOperationIdentity
	ContinuationCheckpoint map[string]interface{}
	CorrelationID          string
	CausationID            string
}

type ActionCoordinator struct {
	portfolio  PortfolioStore
	actions    ActionStore
	catalog    ActionCatalog
	policy     ActionPolicyEvaluator
	validators []ActionProposalValidator
	now        func() time.Time
	newID      func() string
}

func NewActionCoordinator(portfolio PortfolioStore, actions ActionStore, catalog ActionCatalog, policy ActionPolicyEvaluator, validators ...ActionProposalValidator) *ActionCoordinator {
	portable := []ActionProposalValidator{ObservationRefActionProposalValidator{}}
	portable = append(portable, validators...)
	return &ActionCoordinator{portfolio: portfolio, actions: actions, catalog: catalog, policy: policy, validators: portable, now: time.Now, newID: uuid.NewString}
}

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
	run, err := c.portfolio.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.Status != AgentRunStatusRunning {
		return nil, fmt.Errorf("action can only be proposed by a running run, got %s", run.Status)
	}
	if assignedAgentID := strings.TrimSpace(req.AssignedAgentID); assignedAgentID != "" {
		if run.Kind != RunKindConversation || run.Owner.Type != OwnerTypeTeam || strings.TrimSpace(run.AssignedAgentID) != "" {
			return nil, errors.New("action Agent attribution is only valid for an unassigned Team conversation Run")
		}
		run = cloneAgentRun(run)
		run.AssignedAgentID = assignedAgentID
	}
	var lease *AgentRunLeaseGuard
	now := c.now().UTC()
	if run.LeaseOwner != "" {
		if req.WorkerID == "" || run.LeaseOwner != req.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
			return nil, ErrLeaseLost
		}
		lease = &AgentRunLeaseGuard{WorkerID: req.WorkerID, Now: now}
	}
	selection := []skill.BindingReference(nil)
	if req.BindingID != "" || req.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: req.BindingID, Revision: req.BindingRevision})
	}
	bound, err := c.catalog.Resolve(ctx, skill.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}, req.DeploymentID, req.SkillID, req.SkillVersion, req.Action, selection...)
	if err != nil {
		return nil, err
	}
	arguments := cloneMap(req.Arguments)
	for _, validator := range c.validators {
		resolver, ok := validator.(ActionProposalArgumentResolver)
		if !ok || resolver == nil {
			continue
		}
		resolved, handled, resolveErr := resolver.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
			Run: cloneAgentRun(run), Bound: bound, Arguments: cloneMap(arguments),
		})
		if resolveErr != nil {
			return nil, resolveErr
		}
		if handled {
			arguments = cloneMap(resolved)
		}
	}
	if err := c.catalog.ValidateInput(ctx, bound, arguments); err != nil {
		return nil, err
	}
	var proposedAction map[string]interface{}
	for _, validator := range c.validators {
		if validator == nil {
			continue
		}
		preview, validateErr := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: cloneAgentRun(run), Bound: bound, Arguments: cloneMap(arguments)})
		if validateErr != nil {
			return nil, validateErr
		}
		if preview != nil {
			proposedAction = cloneMap(preview)
		}
	}
	if bound.Action.Idempotency == skill.IdempotencyRequired && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, errors.New("skill action requires an idempotency key")
	}
	externalOperationPolicy := effectiveExternalOperationPolicy(bound.Action.SideEffect, bound.Action.ExternalOperationPolicy)
	if externalOperationPolicy == skill.ExternalOperationRequired && req.ExternalOperation == nil {
		return nil, errors.New("skill action requires an external operation identity")
	}
	if externalOperationPolicy == skill.ExternalOperationForbidden && req.ExternalOperation != nil {
		return nil, errors.New("skill action forbids an external operation identity")
	}
	externalOperationDigest, err := computeExternalOperationDigest(req.Scope, run.Owner, run.ObjectiveID, req.ExternalOperation)
	if err != nil {
		return nil, err
	}
	if externalOperationDigest != "" {
		prior, lookupErr := c.actions.GetActionCallByExternalOperation(ctx, req.Scope, externalOperationDigest)
		if lookupErr != nil && !errors.Is(lookupErr, ErrActionNotFound) {
			return nil, lookupErr
		}
		if prior != nil && externalOperationReceiptComplete(prior.Status) {
			return c.suppressDuplicateExternalOperation(ctx, req, run, bound, arguments, externalOperationDigest, prior, lease, now)
		}
		if prior != nil {
			return nil, &ExternalOperationConflictError{Prior: prior}
		}
	}
	decision, err := c.policy.EvaluateAction(ctx, ActionPolicyInput{Run: cloneAgentRun(run), Bound: bound, Arguments: cloneMap(arguments), ExternalOperation: req.ExternalOperation, Actor: req.Actor, Summary: req.Summary})
	if err != nil {
		return nil, fmt.Errorf("evaluate action policy: %w", err)
	}
	if !validActionDisposition(decision.Disposition) {
		return nil, errors.New("action policy returned an invalid disposition")
	}
	if decision.Disposition == ActionDispositionRequireApproval && len(decision.EligibleApprovers) == 0 {
		return nil, errors.New("approval policy must identify at least one eligible approver")
	}

	callID := c.newID()
	call := &ActionCall{
		ID: callID, Scope: req.Scope, RunID: run.ID, TurnID: req.TurnID, DeploymentID: req.DeploymentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
		SkillID: req.SkillID, SkillVersion: req.SkillVersion, Action: req.Action,
		Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Arguments: persistedActionArguments(arguments, bound.Action.InputSchema),
		PreparedRuntime: clonePreparedRuntime(req.PreparedRuntime),
		CredentialRefs:  boundCredentialReferences(bound), EvidenceRefs: append([]string(nil), req.EvidenceRefs...), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		ExternalOperationDigest: externalOperationDigest,
		MaxAttempts:             max(1, bound.Action.Retry.MaxAttempts), AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	updatedRun := cloneAgentRun(run)
	updatedRun.Revision++
	updatedRun.UpdatedAt = now
	budgetDenied := false
	if updatedRun.Budget != nil && decision.Disposition != ActionDispositionDeny {
		err := reserveRunBudget(updatedRun, BudgetReservation{
			ID: actionBudgetReservationID(call.ID), Usage: BudgetUsage{Actions: 1}, CreatedAt: now,
		})
		if errors.Is(err, ErrBudgetExhausted) {
			budgetDenied = true
			decision.Disposition = ActionDispositionDeny
			decision.Reason = "run action budget is exhausted"
		} else if err != nil {
			return nil, err
		}
	}

	eventType := "action.proposed"
	eventSummary := req.Summary
	if eventSummary == "" {
		eventSummary = fmt.Sprintf("Proposed %s.%s", req.SkillID, req.Action)
	}
	var approval *ApprovalCheckpoint
	switch decision.Disposition {
	case ActionDispositionAllow:
		call.Status = ActionCallStatusReady
		updatedRun.Status = AgentRunStatusWaitingForDependency
		updatedRun.WakeCondition = &WakeCondition{Type: "action", Reference: call.ID}
		updatedRun.Checkpoint = preserveKernelActionHistory(run.Checkpoint, req.ContinuationCheckpoint)
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
	case ActionDispositionDeny:
		call.Status = ActionCallStatusDenied
		call.Error = decision.Reason
		call.CompletedAt = &now
		updatedRun.Status = AgentRunStatusQueued
		if budgetDenied {
			updatedRun.Status = AgentRunStatusPaused
		}
		updatedRun.WakeCondition = nil
		updatedRun.Checkpoint = checkpointTerminalAction(preserveKernelActionHistory(run.Checkpoint, req.ContinuationCheckpoint), call, map[string]interface{}{
			"denialSource": "policy",
		})
		updatedRun.AvailableAt = now
		updatedRun.QueueEnteredAt = now
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
		eventType = "action.denied"
		if budgetDenied {
			eventType = "budget.exhausted"
			eventSummary = "Run paused before exceeding its autonomous action budget"
		}
	case ActionDispositionRequireApproval:
		approvalID := c.newID()
		ttl := decision.ApprovalTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		call.Status = ActionCallStatusWaitingApproval
		call.ApprovalID = approvalID
		approval = &ApprovalCheckpoint{
			ID: approvalID, Scope: req.Scope, RunID: run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
			Risk: bound.Action.Risk, Summary: eventSummary, PolicyReason: decision.Reason, ProposedAction: proposedAction,
			EvidenceRefs: append([]string(nil), req.EvidenceRefs...), EligibleApprovers: append([]ApprovalPrincipal(nil), decision.EligibleApprovers...),
			Destinations:           append([]ApprovalDestination(nil), decision.ApprovalDestinations...),
			ContinuationCheckpoint: cloneMap(req.ContinuationCheckpoint), ExpiresAt: now.Add(ttl), TimeoutDecision: decision.ApprovalTimeout,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		updatedRun.Status = AgentRunStatusWaitingForApproval
		updatedRun.WakeCondition = &WakeCondition{Type: "approval", Reference: approvalID}
		updatedRun.Checkpoint = preserveKernelActionHistory(run.Checkpoint, req.ContinuationCheckpoint)
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
		eventType = "action.approval_requested"
	}
	if approval != nil && approval.ProposedAction == nil {
		approval.ProposedAction = approvalPreview(bound, arguments)
	}
	if approval != nil {
		approval.ProposedAction = c.enrichApprovalPreview(ctx, approval.ProposedAction, run, req.EvidenceRefs, req.ExternalOperation)
	}
	actor := req.Actor
	if actor.Type == "" {
		actor = ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &ActivityEvent{
		ID: c.newID(), Scope: req.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TurnID: req.TurnID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: actor, Summary: eventSummary, Visibility: ActivityVisibilityScope,
		Payload:       map[string]interface{}{"actionCallId": call.ID, "approvalId": call.ApprovalID, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "risk": call.Risk},
		CorrelationID: req.CorrelationID, CausationID: req.CausationID, CreatedAt: now,
	}
	if call.PreparedRuntime != nil {
		event.Payload["preparedRuntime"] = clonePreparedRuntime(call.PreparedRuntime)
	}
	if decision.Reason != "" && decision.Disposition != ActionDispositionAllow {
		event.Payload["policyReason"] = decision.Reason
	}
	result, err := c.actions.CreateActionProposal(ctx, ActionProposalRecord{Call: call, Approval: approval, Run: updatedRun, ExpectedRunRevision: run.Revision, Lease: lease, Event: event})
	var conflict *ExternalOperationConflictError
	if errors.As(err, &conflict) && conflict.Prior != nil && externalOperationReceiptComplete(conflict.Prior.Status) {
		return c.suppressDuplicateExternalOperation(ctx, req, run, bound, arguments, externalOperationDigest, conflict.Prior, lease, now)
	}
	return result, err
}

func (c *ActionCoordinator) suppressDuplicateExternalOperation(ctx context.Context, req ProposeActionRequest, run *AgentRun, bound *skill.BoundAction, arguments map[string]interface{}, digest string, prior *ActionCall, lease *AgentRunLeaseGuard, now time.Time) (*ActionProposalResult, error) {
	call := &ActionCall{
		ID: c.newID(), Scope: req.Scope, RunID: run.ID, TurnID: req.TurnID, DeploymentID: req.DeploymentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision, SkillID: req.SkillID, SkillVersion: req.SkillVersion,
		Action: req.Action, Status: ActionCallStatusSucceeded, Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect,
		Arguments: persistedActionArguments(arguments, bound.Action.InputSchema), PreparedRuntime: clonePreparedRuntime(req.PreparedRuntime),
		EvidenceRefs: append([]string(nil), req.EvidenceRefs...), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		ExternalOperationDigest: digest, DuplicateOfActionCallID: prior.ID,
		Output:      map[string]interface{}{"duplicateSuppressed": true, "priorActionCallId": prior.ID, "priorRunId": prior.RunID},
		MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	updatedRun := cloneAgentRun(run)
	updatedRun.Status = AgentRunStatusQueued
	updatedRun.WakeCondition = nil
	updatedRun.Checkpoint = checkpointTerminalAction(preserveKernelActionHistory(run.Checkpoint, req.ContinuationCheckpoint), call, map[string]interface{}{
		"duplicateSuppressed": true, "priorActionCallId": prior.ID, "priorRunId": prior.RunID,
	})
	updatedRun.AvailableAt = now
	updatedRun.QueueEnteredAt = now
	updatedRun.LeaseOwner = ""
	updatedRun.LeaseExpiresAt = nil
	updatedRun.UpdatedAt = now
	updatedRun.Revision++
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
	return c.actions.CreateActionProposal(ctx, ActionProposalRecord{Call: call, Run: updatedRun, ExpectedRunRevision: run.Revision, Lease: lease, Event: event})
}

func actionBudgetReservationID(actionID string) string {
	return "action:" + actionID
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
	result := cloneMap(preview)
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
