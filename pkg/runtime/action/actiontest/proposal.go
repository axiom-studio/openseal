package actiontest

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// LoopProposeRequest is req as the loop proposes tool call toolCallID of
// step-1: loop origin and execution and the per-call idempotency key.
func LoopProposeRequest(req action.ProposeActionRequest, toolCallID string) action.ProposeActionRequest {
	req.Origin, req.Execution = models.ActionOriginLoop, models.ActionExecutionLoop
	if req.TurnID == "" {
		req.TurnID = "step-1"
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "loop:" + req.TurnID + ":" + toolCallID
	}
	return req
}

// ProposalValidatorFunc adapts a function to ActionProposalValidator.
type ProposalValidatorFunc func(context.Context, agentrun.ActionProposalValidationInput) (map[string]interface{}, error)

// ValidateActionProposal calls f.
func (f ProposalValidatorFunc) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	return f(ctx, input)
}

// ProposeLoopAction stores a ready loop call of step-1 for the Run's lease
// owner workerID, as ActionCoordinator.Propose does for a governed loop tool
// call: the Run is unchanged except for the action reservation of a budgeted
// Run.
func ProposeLoopAction(t *testing.T, store models.KernelStore, catalog agentrun.ActionExecutionCatalog, run *models.AgentRun, workerID string, now time.Time, spec LoopActionSpec) *models.ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: current.Scope.Kind, ID: current.Scope.ID}, current.AssignedAgentID, spec.SkillID, spec.Version, spec.Action)
	if err != nil {
		t.Fatal(err)
	}
	call := &models.ActionCall{
		ID: spec.ID, Scope: current.Scope, RunID: current.ID, TurnID: "step-1", DeploymentID: current.AssignedAgentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
		SkillID: spec.SkillID, SkillVersion: spec.Version, Action: spec.Action, Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Status: models.ActionCallStatusReady, Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Review: bound.Action.Review,
		Arguments: maps.Clone(spec.Arguments), CredentialRefs: loopActionCredentialRefs(bound), IdempotencyKey: "tool-call:step-1:" + spec.ID,
		MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if spec.ExternalOperation != nil {
		call.ExternalOperationDigest, err = activity.ComputeExternalOperationDigest(current.Scope, current.Owner, current.ObjectiveID, spec.ExternalOperation, call.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	record := models.ActionProposalRecord{
		Call: call, ExpectedRunRevision: current.Revision, Lease: &models.AgentRunLeaseGuard{WorkerID: workerID, Now: now},
		Event: &models.ActivityEvent{
			ID: "proposed-" + spec.ID, Scope: current.Scope, EventType: "action.proposed", Severity: models.ActivitySeverityInfo, AgentID: current.AssignedAgentID,
			RunID: current.ID, TurnID: "step-1", Actor: models.ActivityActor{Type: "worker", ID: workerID}, Summary: "Proposed " + spec.Action,
			Visibility: models.ActivityVisibilityScope, CreatedAt: now, Payload: map[string]interface{}{"actionCallId": spec.ID},
		},
	}
	if current.Budget != nil {
		updated := models.CloneAgentRun(current)
		if err := models.ReserveRunBudget(updated, models.BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: models.BudgetUsage{Actions: 1}, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		updated.Revision++
		updated.UpdatedAt = now
		record.Run = updated
	}
	result, err := store.CreateActionProposal(ctx, record)
	if err != nil || result == nil || !result.Created {
		t.Fatalf("propose loop call %s = %#v, %v", spec.ID, result, err)
	}
	return result
}

// LoopActionSpec describes one loop tool call ProposeLoopAction stores.
type LoopActionSpec struct {
	ID, SkillID, Version, Action string
	Arguments                    map[string]interface{}
	ExternalOperation            *activity.ExternalOperationIdentity
}

// AvailabilityFunc adapts a function to action.CredentialAvailabilityChecker.
type AvailabilityFunc func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error)

// CheckCredentialAvailability calls f.
func (f AvailabilityFunc) CheckCredentialAvailability(ctx context.Context, req action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
	return f(ctx, req)
}

// loopActionCredentialRefs are the binding's references for the credentials
// the bound action requires, as a proposal records them; nil when none.
func loopActionCredentialRefs(bound *skill.BoundAction) map[string]skill.CredentialReference {
	refs := make(map[string]skill.CredentialReference)
	for _, requirement := range bound.Action.Credentials {
		if reference, ok := bound.Binding.Credentials[requirement.Name]; ok {
			refs[requirement.Name] = reference
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}
