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
	Run       *AgentRun
	Bound     *skill.BoundAction
	Arguments map[string]interface{}
	Actor     ActivityActor
	Summary   string
}

type ActionPolicyDecision struct {
	Disposition       ActionDisposition
	Reason            string
	EligibleApprovers []ApprovalPrincipal
	ApprovalTTL       time.Duration
}

type ActionPolicyEvaluator interface {
	EvaluateAction(context.Context, ActionPolicyInput) (ActionPolicyDecision, error)
}

type ActionPolicyEvaluatorFunc func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error)

func (f ActionPolicyEvaluatorFunc) EvaluateAction(ctx context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
	return f(ctx, input)
}

type ActionCatalog interface {
	Resolve(context.Context, skill.ScopeReference, string, string, string, string) (*skill.BoundAction, error)
	ValidateInput(context.Context, *skill.BoundAction, map[string]interface{}) error
}

type ProposeActionRequest struct {
	Scope                  Scope
	RunID                  string
	TurnID                 string
	WorkerID               string
	DeploymentID           string
	SkillID                string
	SkillVersion           string
	Action                 string
	Arguments              map[string]interface{}
	IdempotencyKey         string
	Summary                string
	Actor                  ActivityActor
	EvidenceRefs           []string
	ContinuationCheckpoint map[string]interface{}
	CorrelationID          string
	CausationID            string
}

type ActionCoordinator struct {
	portfolio PortfolioStore
	actions   ActionStore
	catalog   ActionCatalog
	policy    ActionPolicyEvaluator
	now       func() time.Time
	newID     func() string
}

func NewActionCoordinator(portfolio PortfolioStore, actions ActionStore, catalog ActionCatalog, policy ActionPolicyEvaluator) *ActionCoordinator {
	return &ActionCoordinator{portfolio: portfolio, actions: actions, catalog: catalog, policy: policy, now: time.Now, newID: uuid.NewString}
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
	var lease *AgentRunLeaseGuard
	now := c.now().UTC()
	if run.LeaseOwner != "" {
		if req.WorkerID == "" || run.LeaseOwner != req.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
			return nil, ErrLeaseLost
		}
		lease = &AgentRunLeaseGuard{WorkerID: req.WorkerID, Now: now}
	}
	bound, err := c.catalog.Resolve(ctx, skill.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}, req.DeploymentID, req.SkillID, req.SkillVersion, req.Action)
	if err != nil {
		return nil, err
	}
	if err := c.catalog.ValidateInput(ctx, bound, req.Arguments); err != nil {
		return nil, err
	}
	if bound.Action.Idempotency == skill.IdempotencyRequired && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, errors.New("skill action requires an idempotency key")
	}
	decision, err := c.policy.EvaluateAction(ctx, ActionPolicyInput{Run: cloneAgentRun(run), Bound: bound, Arguments: cloneMap(req.Arguments), Actor: req.Actor, Summary: req.Summary})
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
		SkillID: req.SkillID, SkillVersion: req.SkillVersion, Action: req.Action,
		Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Arguments: persistedActionArguments(req.Arguments, bound.Action.InputSchema),
		CredentialRefs: boundCredentialReferences(bound), EvidenceRefs: append([]string(nil), req.EvidenceRefs...), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		MaxAttempts: max(1, bound.Action.Retry.MaxAttempts), AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
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
		updatedRun.Checkpoint = cloneMap(req.ContinuationCheckpoint)
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
	case ActionDispositionDeny:
		call.Status = ActionCallStatusDenied
		call.Error = decision.Reason
		updatedRun.Status = AgentRunStatusQueued
		if budgetDenied {
			updatedRun.Status = AgentRunStatusPaused
		}
		updatedRun.WakeCondition = nil
		updatedRun.Checkpoint = cloneMap(req.ContinuationCheckpoint)
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
			Risk: bound.Action.Risk, Summary: eventSummary, PolicyReason: decision.Reason, ProposedAction: approvalPreview(bound, req.Arguments),
			EvidenceRefs: append([]string(nil), req.EvidenceRefs...), EligibleApprovers: append([]ApprovalPrincipal(nil), decision.EligibleApprovers...),
			ContinuationCheckpoint: cloneMap(req.ContinuationCheckpoint), ExpiresAt: now.Add(ttl), Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		updatedRun.Status = AgentRunStatusWaitingForApproval
		updatedRun.WakeCondition = &WakeCondition{Type: "approval", Reference: approvalID}
		updatedRun.Checkpoint = cloneMap(req.ContinuationCheckpoint)
		updatedRun.LeaseOwner = ""
		updatedRun.LeaseExpiresAt = nil
		eventType = "action.approval_requested"
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
	if decision.Reason != "" && decision.Disposition != ActionDispositionAllow {
		event.Payload["policyReason"] = decision.Reason
	}
	return c.actions.CreateActionProposal(ctx, ActionProposalRecord{Call: call, Approval: approval, Run: updatedRun, ExpectedRunRevision: run.Revision, Lease: lease, Event: event})
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
		if sensitiveFieldName(key) || schemaSensitive(childSchema) {
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
			if sensitiveFieldName(key) || schemaSensitive(childSchema) {
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

func sensitiveFieldName(value string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(value, "_", ""), "-", ""))
	for _, fragment := range []string{"password", "passwd", "secret", "token", "apikey", "privatekey", "credential"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}
