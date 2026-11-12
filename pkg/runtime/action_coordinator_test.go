package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionCoordinatorPersistsSecretSafeApproval(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	storetest.SeedActionCall(t, store, &ActionCall{
		ID: "prepared-comment", Scope: scope, RunID: run.ID, TurnID: "step-0", DeploymentID: "release-agent",
		SkillID: "browser", SkillVersion: "2.0.0", Action: "fill", Status: ActionCallStatusSucceeded,
		Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Arguments: map[string]interface{}{
			"value": "The exact reviewed comment.", "target": "comment-box", "apiToken": "legacy-secret",
		},
		Attempt: 1, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	policy := ActionPolicyEvaluatorFunc(func(_ context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		if input.Bound.Action.Name != "deploy" || input.Run.ID != run.ID {
			t.Fatalf("policy input mismatch: %#v", input)
		}
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "production change",
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "release-manager"}}, ApprovalTTL: time.Hour,
			ApprovalTimeout: ApprovalTimeoutApprove,
		}, nil
	})
	coordinator := NewActionCoordinator(store, store, catalog, policy, nil)
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	ids := []string{"call", "approval", "event"}
	coordinator.newID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	result, err := coordinator.Propose(ctx, loopProposeRequest(ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "production", "credentialField": "username", "apiToken": "raw-secret", "nested": map[string]interface{}{"password": "also-secret", "region": "us"}},
		Summary:   "Deploy release to production",
		Actor:     ActivityActor{Type: "agent", ID: "release-agent"}, EvidenceRefs: []string{"artifact://change-plan", "action-call:prepared-comment"},
		ReviewContext: &ApprovalReviewContext{
			Summary: "Deploy release 42", Target: "production", Audience: "All customers",
			Purpose: "Ship the reviewed release", Consequences: []string{"Updates the production service"},
		},
	}, "deploy-production"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != ActionCallStatusWaitingApproval || result.Approval == nil || result.Approval.ID != "approval" {
		t.Fatalf("approval proposal mismatch: %#v", result)
	}
	if result.Approval.TimeoutDecision != ApprovalTimeoutApprove {
		t.Fatalf("approval timeout policy was not persisted: %#v", result.Approval)
	}
	review := result.Approval.ProposedAction["reviewContext"].(map[string]interface{})
	if review["summary"] != "Deploy release 42" || review["target"] != "production" || review["audience"] != "All customers" {
		t.Fatalf("approval review context was not persisted: %#v", review)
	}
	if result.Call.BindingID != "release-binding" || result.Call.BindingRevision != 1 {
		t.Fatalf("action did not persist its exact binding: %#v", result.Call)
	}
	previewArgs := result.Approval.ProposedAction["arguments"].(map[string]interface{})
	if previewArgs["apiToken"] != "[REDACTED]" || previewArgs["credentialField"] != "username" || previewArgs["environment"] != "production" || previewArgs["nested"].(map[string]interface{})["password"] != "[REDACTED]" {
		t.Fatalf("approval preview was not sanitized: %#v", previewArgs)
	}
	prepared := result.Approval.ProposedAction["preparedEvidence"].([]interface{})
	preparedArgs := prepared[0].(map[string]interface{})["arguments"].(map[string]interface{})
	if len(prepared) != 1 || preparedArgs["value"] != "The exact reviewed comment." ||
		preparedArgs["apiToken"] != "[REDACTED]" {
		t.Fatalf("prepared approval evidence was not exact and secret-safe: %#v", prepared)
	}
	if result.Call.CredentialRefs["token"].ID != "release-secret" {
		t.Fatalf("opaque credential binding missing: %#v", result.Call.CredentialRefs)
	}
	callJSON, _ := json.Marshal(result.Call)
	if strings.Contains(string(callJSON), "raw-secret") || strings.Contains(string(callJSON), "also-secret") || result.Call.Arguments["apiToken"] != nil || result.Call.Arguments["credentialField"] != "username" || result.Call.Arguments["nested"].(map[string]interface{})["password"] != nil {
		t.Fatalf("durable action call leaked sensitive arguments: %s", callJSON)
	}
	eventJSON, _ := json.Marshal(result.Event)
	if strings.Contains(string(eventJSON), "raw-secret") || strings.Contains(string(eventJSON), "release-secret") {
		t.Fatalf("activity leaked secret material or credential references: %s", eventJSON)
	}
	assertLoopRunUntouched(t, store, run)
}

func TestActionCoordinatorEnforcesSchemaPolicyIdempotencyAndLease(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	decision := ActionDispositionAllow
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: decision, Reason: "test"}, nil
	}), nil)
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	coordinator.newID = func() string { return "id-" + string(decision) }
	base := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy"}
	invalid := loopProposeRequest(base, "invalid")
	invalid.Arguments = map[string]interface{}{"environment": 42}
	var rejection *ActionProposalRejection
	if _, err := coordinator.Propose(ctx, invalid); !errors.As(err, &rejection) || rejection.Code != ProposalRejectionToolArgumentsInvalid {
		t.Fatalf("invalid input schema error = %v", err)
	}
	missingKey := loopProposeRequest(base, "missing")
	missingKey.IdempotencyKey = ""
	missingKey.Arguments = map[string]interface{}{"environment": "staging"}
	if _, err := coordinator.Propose(ctx, missingKey); err == nil || !strings.Contains(err.Error(), "idempotency") || errors.As(err, &rejection) {
		t.Fatalf("missing idempotency error = %v", err)
	}
	stale := loopProposeRequest(base, "stale")
	stale.Arguments = map[string]interface{}{"environment": "staging"}
	stale.WorkerID = "other-worker"
	if _, err := coordinator.Propose(ctx, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker error = %v", err)
	}
	allowed := loopProposeRequest(base, "allowed")
	allowed.Arguments = map[string]interface{}{"environment": "staging"}
	result, err := coordinator.Propose(ctx, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != ActionCallStatusReady || result.Approval != nil || result.Run != nil {
		t.Fatalf("allowed call mismatch: %#v", result)
	}
	assertLoopRunUntouched(t, store, run)
}

func TestActionCoordinatorTreatsASeparateIdempotencyOccurrenceAsNewExternalWork(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog, scope := governedActionCatalog(t)
	policyEvaluations := 0
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		policyEvaluations++
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}), nil)
	identity := &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}

	firstRun := claimedActionRun(t, store, scope, time.Now().UTC(), "worker-one")
	first, err := coordinator.Propose(ctx, loopProposeRequest(ProposeActionRequest{
		Scope: scope, RunID: firstRun.ID, WorkerID: "worker-one", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		ExternalOperation: identity,
	}, "occurrence-one"))
	if err != nil {
		t.Fatal(err)
	}
	secondRun := claimedActionRun(t, store, scope, time.Now().UTC().Add(time.Second), "worker-two")
	second, err := coordinator.Propose(ctx, loopProposeRequest(ProposeActionRequest{
		Scope: scope, RunID: secondRun.ID, WorkerID: "worker-two", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		ExternalOperation: identity,
	}, "occurrence-two"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Call.Status != ActionCallStatusReady || second.Call.DuplicateOfActionCallID != "" || second.Event.EventType == "action.duplicate_suppressed" {
		t.Fatalf("separate occurrence was suppressed: %#v", second)
	}
	if first.Call.ExternalOperationDigest == second.Call.ExternalOperationDigest || policyEvaluations != 2 {
		t.Fatalf("external digests or policy evaluations did not distinguish occurrences: first=%s second=%s evaluations=%d", first.Call.ExternalOperationDigest, second.Call.ExternalOperationDigest, policyEvaluations)
	}
}

func TestActionCoordinatorEnforcesExternalOperationPolicy(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog, scope := externalOperationPolicyCatalog(t)
	run := claimedActionRun(t, store, scope, time.Now().UTC(), "worker")
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}), nil)
	base := loopProposeRequest(ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "browser-agent", SkillID: "browser", SkillVersion: "1.0.0", Arguments: map[string]interface{}{}}, "operation-policy")
	var rejection *ActionProposalRejection
	forbidden := base
	forbidden.Action = "click"
	forbidden.ExternalOperation = &ExternalOperationIdentity{Resource: "browser-session:one", Operation: "login"}
	if _, err := coordinator.Propose(ctx, forbidden); !errors.As(err, &rejection) || rejection.Code != ProposalRejectionExternalIdentityInvalid || !strings.Contains(err.Error(), "forbids an external operation identity") {
		t.Fatalf("forbidden error=%v", err)
	}
	required := base
	required.Action = "commit"
	if _, err := coordinator.Propose(ctx, required); !errors.As(err, &rejection) || rejection.Code != ProposalRejectionExternalIdentityInvalid || !strings.Contains(err.Error(), "requires an external operation identity") {
		t.Fatalf("required error=%v", err)
	}
	calls, err := store.ListActionCalls(ctx, ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("rejected proposals were recorded: %#v, %v", calls, err)
	}
}

// loopProposeRequest is req as the loop proposes tool call toolCallID of
// step-1: loop origin and execution and the per-call idempotency key.
func loopProposeRequest(req ProposeActionRequest, toolCallID string) ProposeActionRequest {
	req.Origin, req.Execution = ActionOriginLoop, ActionExecutionLoop
	if req.TurnID == "" {
		req.TurnID = "step-1"
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "loop:" + req.TurnID + ":" + toolCallID
	}
	return req
}

// assertLoopRunUntouched checks a loop proposal left the Run as claimed:
// running, same lease, same revision, no wake condition.
func assertLoopRunUntouched(t *testing.T, store PortfolioStore, run *AgentRun) {
	t.Helper()
	current, err := store.GetAgentRun(context.Background(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != AgentRunStatusRunning || current.Revision != run.Revision || current.LeaseOwner != run.LeaseOwner || current.WakeCondition != nil {
		t.Fatalf("loop proposal changed the Run: %#v", current)
	}
}

func externalOperationPolicyCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{ID: "browser", Version: "1.0.0", Name: "Browser", Transport: skill.TransportReference{Kind: "tool", Endpoint: "browser"}, Actions: map[string]skill.Action{
		"click":  {Name: "click", Description: "Session-local click", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		"commit": {Name: "commit", Description: "Commit business operation", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, ExternalOperationPolicy: skill.ExternalOperationRequired},
	}}
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "browser-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "browser-agent", SkillID: "browser", SkillVersion: "1.0.0", AllowedActions: []string{"click", "commit"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

func governedActionCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "release", Version: "1.0.0", Name: "Release", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://release.invalid"},
		Actions: map[string]skill.Action{"deploy": {
			Name: "deploy", Description: "Deploy a release", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelProduction,
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"environment":     map[string]interface{}{"type": "string"},
					"credentialField": map[string]interface{}{"type": "string", "enum": []interface{}{"username", "password"}},
					"apiToken":        map[string]interface{}{"type": "string", "writeOnly": true},
					"nested":          map[string]interface{}{"type": "object", "properties": map[string]interface{}{"password": map[string]interface{}{"type": "string"}, "region": map[string]interface{}{"type": "string"}}},
				}, "required": []interface{}{"environment"},
			},
			Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

func claimedActionRun(t *testing.T, store *sqlite.Store, scope Scope, now time.Time, worker string) *AgentRun {
	t.Helper()
	return claimedOwnedActionRun(t, store, scope, now, worker, ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"})
}

// claimedOwnedActionRun claims a run of release-agent under owner.
func claimedOwnedActionRun(t *testing.T, store *sqlite.Store, scope Scope, now time.Time, worker string, owner ObjectiveOwner) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != run.ID {
		t.Fatalf("run was not claimed: %#v", claimed)
	}
	return claimed
}
