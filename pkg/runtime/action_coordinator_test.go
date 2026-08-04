package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionCoordinatorPersistsSecretSafeApprovalAndReleasesRun(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	store.actions[portfolioKey(scope, "prepared-comment")] = &ActionCall{
		ID: "prepared-comment", Scope: scope, RunID: run.ID, DeploymentID: "release-agent",
		SkillID: "browser", SkillVersion: "2.0.0", Action: "fill", Status: ActionCallStatusSucceeded,
		Arguments: map[string]interface{}{
			"value": "The exact reviewed comment.", "target": "comment-box", "apiToken": "legacy-secret",
		},
		Attempt: 1, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
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
	coordinator := NewActionCoordinator(store, store, catalog, policy)
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	ids := []string{"call", "approval", "event"}
	coordinator.newID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	result, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments:      map[string]interface{}{"environment": "production", "credentialField": "username", "apiToken": "raw-secret", "nested": map[string]interface{}{"password": "also-secret", "region": "us"}},
		IdempotencyKey: "deploy-production-v1", Summary: "Deploy release to production",
		Actor: ActivityActor{Type: "agent", ID: "release-agent"}, EvidenceRefs: []string{"artifact://change-plan", "action-call:prepared-comment"},
		ReviewContext: &ApprovalReviewContext{
			Summary: "Deploy release 42", Target: "production", Audience: "All customers",
			Purpose: "Ship the reviewed release", Consequences: []string{"Updates the production service"},
		},
		ContinuationCheckpoint: map[string]interface{}{"step": "await-release-approval"},
	})
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
	persistedRun, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.Status != AgentRunStatusWaitingForApproval || persistedRun.LeaseOwner != "" || persistedRun.WakeCondition == nil || persistedRun.WakeCondition.Reference != "approval" {
		t.Fatalf("run was not durably suspended: %#v", persistedRun)
	}
}

func TestActionCoordinatorEnforcesSchemaPolicyIdempotencyAndLease(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	decision := ActionDispositionAllow
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: decision, Reason: "test"}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	coordinator.newID = func() string { return "id-" + decision.String() }
	base := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy"}
	invalid := base
	invalid.Arguments = map[string]interface{}{"environment": 42}
	invalid.IdempotencyKey = "invalid"
	if _, err := coordinator.Propose(ctx, invalid); err == nil {
		t.Fatal("invalid input schema should be rejected")
	}
	missingKey := base
	missingKey.Arguments = map[string]interface{}{"environment": "staging"}
	if _, err := coordinator.Propose(ctx, missingKey); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("missing idempotency error = %v", err)
	}
	stale := base
	stale.Arguments = map[string]interface{}{"environment": "staging"}
	stale.IdempotencyKey = "stale"
	stale.WorkerID = "other-worker"
	if _, err := coordinator.Propose(ctx, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker error = %v", err)
	}
	allowed := base
	allowed.Arguments = map[string]interface{}{"environment": "staging"}
	allowed.IdempotencyKey = "allowed"
	result, err := coordinator.Propose(ctx, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != ActionCallStatusReady || result.Approval != nil {
		t.Fatalf("allowed call mismatch: %#v", result)
	}
	persisted, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || persisted.Status != AgentRunStatusWaitingForDependency || persisted.WakeCondition == nil || persisted.WakeCondition.Reference != result.Call.ID || persisted.LeaseOwner != "" {
		t.Fatalf("allowed action did not suspend on its dependency: %#v, %v", persisted, err)
	}
}

func TestActionCoordinatorPersistsPolicyDenialAndRequeues(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "production freeze"}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	coordinator.newID = func() string { return "denied" }
	result, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "denied-deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != ActionCallStatusDenied || result.Call.Error != "production freeze" || result.Approval != nil || result.Event.EventType != "action.denied" {
		t.Fatalf("denial mismatch: %#v", result)
	}
	persisted, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil || persisted.Status != AgentRunStatusQueued || persisted.LeaseOwner != "" {
		t.Fatalf("denied run did not requeue: %#v, %v", persisted, err)
	}
	call, err := store.ClaimNextAction(context.Background(), ActionClaim{Scope: scope, WorkerID: "action-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute})
	if err != nil || call != nil {
		t.Fatalf("denied action became executable: %#v, %v", call, err)
	}
}

func TestActionCoordinatorSuppressesExternalOperationAcrossRuns(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	policyEvaluations := 0
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		policyEvaluations++
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }

	firstRun := claimedActionRun(t, store, scope, now, "worker-one")
	identity := &ExternalOperationIdentity{Resource: "https://Forum.Example/topics/42?b=2&a=1#reply", Operation: "comment:create"}
	first, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: firstRun.ID, WorkerID: "worker-one", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		IdempotencyKey: "first-run", ExternalOperation: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAction(ctx, ActionClaim{Scope: scope, WorkerID: "action-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != first.Call.ID {
		t.Fatalf("claimed action = %#v, %v", claimed, err)
	}
	persistedRun, err := store.GetAgentRun(ctx, scope, firstRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := now.Add(3 * time.Second)
	completedCall := cloneActionCall(claimed)
	completedCall.Status = ActionCallStatusSucceeded
	completedCall.Output = map[string]interface{}{"receipt": "external-42"}
	completedCall.CompletedAt = &completedAt
	completedCall.LeaseOwner = ""
	completedCall.LeaseExpiresAt = nil
	completedCall.UpdatedAt = completedAt
	completedCall.Revision++
	completedRun := cloneAgentRun(persistedRun)
	completedRun.Status = AgentRunStatusCompleted
	completedRun.WakeCondition = nil
	completedRun.CompletedAt = &completedAt
	completedRun.UpdatedAt = completedAt
	completedRun.Revision++
	_, err = store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: completedCall, ExpectedCallRevision: claimed.Revision, Run: completedRun, ExpectedRunRevision: persistedRun.Revision,
		WorkerID: "action-worker", Now: now.Add(2 * time.Second),
		Event: &ActivityEvent{ID: "first-succeeded", Scope: scope, RunID: firstRun.ID, EventType: "action.succeeded", Summary: "External action succeeded", CreatedAt: completedAt},
	})
	if err != nil {
		t.Fatal(err)
	}

	secondRun := claimedActionRun(t, store, scope, now.Add(4*time.Second), "worker-two")
	second, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: secondRun.ID, WorkerID: "worker-two", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		IdempotencyKey: "first-run", ExternalOperation: &ExternalOperationIdentity{Resource: "https://forum.example/topics/42?a=1&b=2", Operation: "COMMENT:CREATE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Call.Status != ActionCallStatusSucceeded || second.Call.DuplicateOfActionCallID != first.Call.ID || second.Approval != nil ||
		second.Event.EventType != "action.duplicate_suppressed" || second.Call.Output["duplicateSuppressed"] != true {
		t.Fatalf("duplicate suppression result = %#v", second)
	}
	if policyEvaluations != 1 {
		t.Fatalf("policy evaluations = %d, duplicate should be suppressed before a new proposal", policyEvaluations)
	}
	if executable, err := store.ClaimNextAction(ctx, ActionClaim{Scope: scope, WorkerID: "action-worker", Now: now.Add(5 * time.Second), LeaseDuration: time.Minute}); err != nil || executable != nil {
		t.Fatalf("suppressed operation became executable: %#v, %v", executable, err)
	}
	receipt, err := store.GetActionCallByExternalOperation(ctx, scope, first.Call.ExternalOperationDigest)
	if err != nil || receipt.ID != first.Call.ID {
		t.Fatalf("authoritative receipt = %#v, %v", receipt, err)
	}
}

func TestActionCoordinatorTreatsASeparateIdempotencyOccurrenceAsNewExternalWork(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	policyEvaluations := 0
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		policyEvaluations++
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	identity := &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}

	firstRun := claimedActionRun(t, store, scope, time.Now().UTC(), "worker-one")
	first, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: firstRun.ID, WorkerID: "worker-one", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		IdempotencyKey: "occurrence-one", ExternalOperation: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRun := claimedActionRun(t, store, scope, time.Now().UTC().Add(time.Second), "worker-two")
	second, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: secondRun.ID, WorkerID: "worker-two", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"},
		IdempotencyKey: "occurrence-two", ExternalOperation: identity,
	})
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
	store := NewMemoryStore()
	catalog, scope := externalOperationPolicyCatalog(t)
	run := claimedActionRun(t, store, scope, time.Now().UTC(), "worker")
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	base := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "browser-agent", SkillID: "browser", SkillVersion: "1.0.0", Arguments: map[string]interface{}{}, IdempotencyKey: "operation-policy"}
	forbidden := base
	forbidden.Action = "click"
	forbidden.ExternalOperation = &ExternalOperationIdentity{Resource: "browser-session:one", Operation: "login"}
	if _, err := coordinator.Propose(ctx, forbidden); err == nil || !strings.Contains(err.Error(), "forbids an external operation identity") {
		t.Fatalf("forbidden error=%v", err)
	}
	required := base
	required.Action = "commit"
	if _, err := coordinator.Propose(ctx, required); err == nil || !strings.Contains(err.Error(), "requires an external operation identity") {
		t.Fatalf("required error=%v", err)
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

func (d ActionDisposition) String() string { return string(d) }

func TestActionCoordinatorPersistsTrustedTeamConversationAgentAttribution(t *testing.T) {
	catalog, scope := governedActionCatalog(t)
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "team-release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-team",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	now := time.Now().UTC()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, Goal: "coordinate release", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	result, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-team", AssignedAgentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "staging"}, IdempotencyKey: "team-release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.AssignedAgentID != "release-agent" {
		t.Fatalf("proposal Run attribution = %#v", result.Run)
	}
	persisted, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil || persisted.AssignedAgentID != "release-agent" {
		t.Fatalf("persisted Run attribution = %#v, %v", persisted, err)
	}
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

func claimedActionRun(t *testing.T, store *MemoryStore, scope Scope, now time.Time, worker string) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
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
