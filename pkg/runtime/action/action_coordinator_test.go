package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionCoordinatorPersistsSecretSafeApproval(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := actiontest.ClaimedRun(t, store, scope, now, "worker")
	storetest.SeedActionCall(t, store, &models.ActionCall{
		ID: "prepared-comment", Scope: scope, RunID: run.ID, TurnID: "step-0", DeploymentID: "release-agent",
		SkillID: "browser", SkillVersion: "2.0.0", Action: "fill", Status: models.ActionCallStatusSucceeded,
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Arguments: map[string]interface{}{
			"value": "The exact reviewed comment.", "target": "comment-box", "apiToken": "legacy-secret",
		},
		Attempt: 1, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	policy := action.PolicyEvaluatorFunc(func(_ context.Context, input action.PolicyInput) (action.PolicyDecision, error) {
		if input.Bound.Action.Name != "deploy" || input.Run.ID != run.ID {
			t.Fatalf("policy input mismatch: %#v", input)
		}
		return action.PolicyDecision{
			Disposition: action.DispositionRequireApproval, Reason: "production change",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "release-manager"}}, ApprovalTTL: time.Hour,
			ApprovalTimeout: models.ApprovalTimeoutApprove,
		}, nil
	})
	coordinator := action.NewCoordinator(store, store, catalog, policy, nil, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	ids := []string{"call", "approval", "event"}
	coordinator.SetNewID(func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	result, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "production", "credentialField": "username", "apiToken": "raw-secret", "nested": map[string]interface{}{"password": "also-secret", "region": "us"}},
		Summary:   "Deploy release to production",
		Actor:     models.ActivityActor{Type: "agent", ID: "release-agent"}, EvidenceRefs: []string{"artifact://change-plan", "action-call:prepared-comment"},
		ReviewContext: &activity.ApprovalReviewContext{
			Summary: "Deploy release 42", Target: "production", Audience: "All customers",
			Purpose: "Ship the reviewed release", Consequences: []string{"Updates the production service"},
		},
	}, "deploy-production"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != models.ActionCallStatusWaitingApproval || result.Approval == nil || result.Approval.ID != "approval" {
		t.Fatalf("approval proposal mismatch: %#v", result)
	}
	if result.Approval.TimeoutDecision != models.ApprovalTimeoutApprove {
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
	actiontest.AssertRunUntouched(t, store, run)
}

func TestActionCoordinatorEnforcesSchemaPolicyIdempotencyAndLease(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := actiontest.ClaimedRun(t, store, scope, now, "worker")
	decision := action.DispositionAllow
	coordinator := action.NewCoordinator(store, store, catalog, action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		return action.PolicyDecision{Disposition: decision, Reason: "test"}, nil
	}), nil, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	coordinator.SetNewID(func() string { return "id-" + string(decision) })
	base := action.ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy"}
	invalid := actiontest.LoopProposeRequest(base, "invalid")
	invalid.Arguments = map[string]interface{}{"environment": 42}
	var rejection *action.ProposalRejection
	if _, err := coordinator.Propose(ctx, invalid); !errors.As(err, &rejection) || rejection.Code != action.ProposalRejectionToolArgumentsInvalid {
		t.Fatalf("invalid input schema error = %v", err)
	}
	missingKey := actiontest.LoopProposeRequest(base, "missing")
	missingKey.IdempotencyKey = ""
	missingKey.Arguments = map[string]interface{}{"environment": "staging"}
	if _, err := coordinator.Propose(ctx, missingKey); err == nil || !strings.Contains(err.Error(), "idempotency") || errors.As(err, &rejection) {
		t.Fatalf("missing idempotency error = %v", err)
	}
	stale := actiontest.LoopProposeRequest(base, "stale")
	stale.Arguments = map[string]interface{}{"environment": "staging"}
	stale.WorkerID = "other-worker"
	if _, err := coordinator.Propose(ctx, stale); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("stale worker error = %v", err)
	}
	allowed := actiontest.LoopProposeRequest(base, "allowed")
	allowed.Arguments = map[string]interface{}{"environment": "staging"}
	result, err := coordinator.Propose(ctx, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != models.ActionCallStatusReady || result.Approval != nil || result.Run != nil {
		t.Fatalf("allowed call mismatch: %#v", result)
	}
	actiontest.AssertRunUntouched(t, store, run)
}

func TestActionCoordinatorTreatsASeparateIdempotencyOccurrenceAsNewExternalWork(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
	policyEvaluations := 0
	coordinator := action.NewCoordinator(store, store, catalog, action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		policyEvaluations++
		return action.PolicyDecision{Disposition: action.DispositionAllow}, nil
	}), nil)
	identity := &activity.ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}

	firstRun := actiontest.ClaimedRun(t, store, scope, time.Now().UTC(), "worker-one")
	first, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: firstRun.ID, WorkerID: "worker-one", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		ExternalOperation: identity,
	}, "occurrence-one"))
	if err != nil {
		t.Fatal(err)
	}
	secondRun := actiontest.ClaimedRun(t, store, scope, time.Now().UTC().Add(time.Second), "worker-two")
	second, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: secondRun.ID, WorkerID: "worker-two", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		ExternalOperation: identity,
	}, "occurrence-two"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Call.Status != models.ActionCallStatusReady || second.Call.DuplicateOfActionCallID != "" || second.Event.EventType == "action.duplicate_suppressed" {
		t.Fatalf("separate occurrence was suppressed: %#v", second)
	}
	if first.Call.ExternalOperationDigest == second.Call.ExternalOperationDigest || policyEvaluations != 2 {
		t.Fatalf("external digests or policy evaluations did not distinguish occurrences: first=%s second=%s evaluations=%d", first.Call.ExternalOperationDigest, second.Call.ExternalOperationDigest, policyEvaluations)
	}
}

func TestActionCoordinatorEnforcesExternalOperationPolicy(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog, scope := externalOperationPolicyCatalog(t)
	run := actiontest.ClaimedRun(t, store, scope, time.Now().UTC(), "worker")
	coordinator := action.NewCoordinator(store, store, catalog, action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		return action.PolicyDecision{Disposition: action.DispositionAllow}, nil
	}), nil)
	base := actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "browser-agent", SkillID: "browser", SkillVersion: "1.0.0", Arguments: map[string]interface{}{}}, "operation-policy")
	var rejection *action.ProposalRejection
	forbidden := base
	forbidden.Action = "click"
	forbidden.ExternalOperation = &activity.ExternalOperationIdentity{Resource: "browser-session:one", Operation: "login"}
	if _, err := coordinator.Propose(ctx, forbidden); !errors.As(err, &rejection) || rejection.Code != action.ProposalRejectionExternalIdentityInvalid || !strings.Contains(err.Error(), "forbids an external operation identity") {
		t.Fatalf("forbidden error=%v", err)
	}
	required := base
	required.Action = "commit"
	if _, err := coordinator.Propose(ctx, required); !errors.As(err, &rejection) || rejection.Code != action.ProposalRejectionExternalIdentityInvalid || !strings.Contains(err.Error(), "requires an external operation identity") {
		t.Fatalf("required error=%v", err)
	}
	calls, err := store.ListActionCalls(ctx, models.ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("rejected proposals were recorded: %#v, %v", calls, err)
	}
}

func externalOperationPolicyCatalog(t *testing.T) (*skill.Catalog, models.Scope) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "one"}
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
