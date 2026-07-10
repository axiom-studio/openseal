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
	store := NewMemoryStore(20)
	catalog, scope := governedActionCatalog(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := claimedActionRun(t, store, scope, now, "worker")
	policy := ActionPolicyEvaluatorFunc(func(_ context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		if input.Bound.Action.Name != "deploy" || input.Run.ID != run.ID {
			t.Fatalf("policy input mismatch: %#v", input)
		}
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "production change",
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "release-manager"}}, ApprovalTTL: time.Hour,
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
		Arguments:      map[string]interface{}{"environment": "production", "apiToken": "raw-secret", "nested": map[string]interface{}{"password": "also-secret", "region": "us"}},
		IdempotencyKey: "deploy-production-v1", Summary: "Deploy release to production",
		Actor: ActivityActor{Type: "agent", ID: "release-agent"}, EvidenceRefs: []string{"artifact://change-plan"},
		ContinuationCheckpoint: map[string]interface{}{"step": "await-release-approval"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Status != ActionCallStatusWaitingApproval || result.Approval == nil || result.Approval.ID != "approval" {
		t.Fatalf("approval proposal mismatch: %#v", result)
	}
	previewArgs := result.Approval.ProposedAction["arguments"].(map[string]interface{})
	if previewArgs["apiToken"] != "[REDACTED]" || previewArgs["environment"] != "production" || previewArgs["nested"].(map[string]interface{})["password"] != "[REDACTED]" {
		t.Fatalf("approval preview was not sanitized: %#v", previewArgs)
	}
	if result.Call.CredentialRefs["token"].ID != "release-secret" {
		t.Fatalf("opaque credential binding missing: %#v", result.Call.CredentialRefs)
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
	store := NewMemoryStore(20)
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

func (d ActionDisposition) String() string { return string(d) }

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
					"environment": map[string]interface{}{"type": "string"},
					"apiToken":    map[string]interface{}{"type": "string", "writeOnly": true},
					"nested":      map[string]interface{}{"type": "object", "properties": map[string]interface{}{"password": map[string]interface{}{"type": "string"}, "region": map[string]interface{}{"type": "string"}}},
				}, "required": []interface{}{"environment"},
			},
			Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "vault"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "vault", ID: "release-secret"}}, Revision: 1,
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
