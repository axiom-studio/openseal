package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionWorkerExecutesGovernedDependencyAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "worker.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			catalog, proposal := createRunnableAction(t, store, now)
			worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(_ context.Context, request CredentialResolutionRequest) (map[string]string, error) {
				if request.Scope != proposal.Call.Scope || request.References["token"].ID != "release-secret" ||
					request.DeploymentID != proposal.Call.DeploymentID || request.SkillID != proposal.Call.SkillID ||
					request.SkillVersion != proposal.Call.SkillVersion || request.Action != proposal.Call.Action ||
					request.ActionCallID != proposal.Call.ID || request.RunID != proposal.Call.RunID {
					t.Fatalf("credential request mismatch: %#v", request)
				}
				return map[string]string{"token": "resolved-super-secret"}, nil
			}), ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
				if input.Credentials["token"] != "resolved-super-secret" || input.Arguments["environment"] != "production" {
					t.Fatalf("dispatch input mismatch: %#v", input)
				}
				if input.Run == nil || input.Run.ID != proposal.Call.RunID || input.Run.AssignedAgentID == "" {
					t.Fatalf("dispatch omitted durable Run execution context: %#v", input.Run)
				}
				return map[string]interface{}{
					"deploymentId": "deploy-123", "status": "healthy",
					"nested": map[string]interface{}{"echo": "Bearer resolved-super-secret"},
				}, nil
			}))
			worker.now = func() time.Time { return now.Add(2 * time.Second) }
			worker.newID = func() string { return "execution-event" }
			result, err := worker.RunOnce(context.Background(), proposal.Call.Scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if result.Call.Status != ActionCallStatusSucceeded || result.Call.Attempt != 1 || result.Call.Output["deploymentId"] != "deploy-123" || result.Run.Status != AgentRunStatusQueued || result.Run.WakeCondition != nil {
				t.Fatalf("execution result mismatch: %#v", result)
			}
			lastAction := result.Run.Checkpoint["lastAction"].(map[string]interface{})
			if lastAction["actionCallId"] != proposal.Call.ID || lastAction["skillId"] != "release" ||
				lastAction["result"].(map[string]interface{})["deploymentId"] != "deploy-123" {
				t.Fatalf("run checkpoint missing action reference: %#v", result.Run.Checkpoint)
			}
			history := actionHistoryEntries(result.Run.Checkpoint)
			if len(history) != 1 || history[0]["actionCallId"] != proposal.Call.ID || history[0]["semanticDigest"] == "" {
				t.Fatalf("run checkpoint missing durable action history: %#v", result.Run.Checkpoint)
			}
			persistedAfterExecution, persistErr := store.GetAgentRun(context.Background(), proposal.Call.Scope, proposal.Call.RunID)
			if persistErr != nil || len(actionHistoryEntries(persistedAfterExecution.Checkpoint)) != 1 {
				t.Fatalf("action history did not survive store round-trip: %#v, %v", persistedAfterExecution, persistErr)
			}
			callJSON, _ := json.Marshal(result.Call)
			checkpointJSON, _ := json.Marshal(result.Run.Checkpoint)
			if strings.Contains(string(callJSON), "resolved-super-secret") || strings.Contains(string(checkpointJSON), "resolved-super-secret") ||
				!strings.Contains(string(callJSON), "[REDACTED]") || !strings.Contains(string(checkpointJSON), "[REDACTED]") {
				t.Fatalf("action output was not safely projected: call=%s checkpoint=%s", callJSON, checkpointJSON)
			}
			eventJSON, _ := json.Marshal(result.Event)
			if strings.Contains(string(eventJSON), "resolved-super-secret") || strings.Contains(string(eventJSON), "deploy-123") {
				t.Fatalf("activity leaked credentials or action output: %s", eventJSON)
			}
			claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != proposal.Call.RunID {
				t.Fatalf("completed dependency did not resume run: %#v, %v", claimed, err)
			}
		})
	}
}

func TestActionWorkerPersistsTypedHumanInterventionAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "human-intervention.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			now := time.Date(2026, 7, 27, 7, 0, 0, 0, time.UTC)
			catalog, proposal := createRunnableAction(t, store, now)
			worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
				return map[string]string{"token": "credential-never-copied"}, nil
			}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{
					"success": true, "requiresHuman": true,
					"challenges": []interface{}{"captcha", "captcha"},
					"currentUrl": "https://example.invalid/challenge?secret=credential-never-copied",
				}, nil
			}))
			worker.now = func() time.Time { return now.Add(2 * time.Second) }
			ids := []string{"human-request", "human-event"}
			worker.newID = func() string {
				id := ids[0]
				ids = ids[1:]
				return id
			}
			result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.Status != AgentRunStatusWaitingForEvent || result.Run.WakeCondition == nil || result.Run.WakeCondition.Type != "human_intervention" || result.Run.WakeCondition.Reference != "human-request" {
				t.Fatalf("waiting run = %#v", result.Run)
			}
			if len(result.Run.HumanInterventions) != 1 {
				t.Fatalf("human interventions = %#v", result.Run.HumanInterventions)
			}
			request := result.Run.HumanInterventions[0]
			if request.Status != HumanInterventionStatusPending || request.ActionCallID != proposal.Call.ID || len(request.Challenge) != 1 || request.Challenge[0] != "captcha" {
				t.Fatalf("human intervention = %#v", request)
			}
			encoded, _ := json.Marshal(request)
			if strings.Contains(string(encoded), "credential-never-copied") || strings.Contains(string(encoded), "example.invalid") {
				t.Fatalf("human intervention copied capability output: %s", encoded)
			}
			if result.Event.EventType != "action.human_intervention_required" || result.Event.Payload["humanInterventionId"] != request.ID {
				t.Fatalf("human intervention event = %#v", result.Event)
			}
			persisted, err := store.GetAgentRun(t.Context(), proposal.Call.Scope, proposal.Call.RunID)
			if err != nil || len(persisted.HumanInterventions) != 1 || persisted.HumanInterventions[0].ID != request.ID {
				t.Fatalf("persisted human intervention = %#v, %v", persisted, err)
			}
		})
	}
}

func TestActionWorkerDispatchesOpaqueCredentialLeaseWithDurableAuthority(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	baseCatalog, proposal := createRunnableAction(t, store, now)
	catalog := &toolTransportActionCatalog{ActionExecutionCatalog: baseCatalog, endpoint: "release_deploy"}
	signer := testCredentialLeaseSigner{key: []byte("lease-signing-key")}
	issuerCalls := 0
	worker := NewActionWorkerWithCredentialLeaseIssuer(store, catalog, ActionCredentialLeaseIssuerFunc(func(ctx context.Context, request ActionCredentialLeaseIssueRequest) (*SignedActionCredentialLease, error) {
		issuerCalls++
		if request.Call == nil || request.Run == nil || request.Call.ID != proposal.Call.ID || request.Run.ID != proposal.Call.RunID || request.Run.AssignedAgentID == "" || request.References["token"] != proposal.Call.CredentialRefs["token"] || request.Transport != "release_deploy" {
			t.Fatalf("credential lease issue request = %#v", request)
		}
		lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
			TenantID: request.Call.Scope.ID, Call: request.Call, Run: request.Run, CredentialFields: map[string][]string{"token": {"value"}}, Transport: request.Transport,
			Issuer: "control-plane", Audience: "execution-host", IssuedAt: now.Add(2 * time.Second), ExpiresAt: now.Add(30 * time.Second), Nonce: "nonce-worker-0000000001",
		})
		if err != nil {
			return nil, err
		}
		return SignActionCredentialLease(ctx, *lease, signer)
	}), ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		if len(input.Credentials) != 0 || input.CredentialLease == nil || input.CredentialLease.Lease.ActionCallID != proposal.Call.ID || input.CredentialReferences["token"] != proposal.Call.CredentialRefs["token"] {
			t.Fatalf("opaque dispatch input = %#v", input)
		}
		if input.Run == nil || input.Run.ID != proposal.Call.RunID || input.Call == nil || input.Call.LeaseOwner != "lease-worker" {
			t.Fatalf("durable dispatch authority = call %#v run %#v", input.Call, input.Run)
		}
		return map[string]interface{}{"ok": true}, nil
	}))
	worker.now = func() time.Time { return now.Add(2 * time.Second) }
	worker.newID = func() string { return "opaque-lease-execution" }
	result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lease-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if issuerCalls != 1 || result.Call.Status != ActionCallStatusSucceeded {
		t.Fatalf("opaque credential execution = calls %d result %#v", issuerCalls, result)
	}
}

type toolTransportActionCatalog struct {
	ActionExecutionCatalog
	endpoint string
}

func (c *toolTransportActionCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	bound, err := c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
	if err != nil {
		return nil, err
	}
	definition := *bound.Definition
	definition.Transport = skill.TransportReference{Kind: "tool", Endpoint: c.endpoint}
	bound.Definition = &definition
	return bound, nil
}

func TestActionBudgetReservationSettlesOnceAndPausesNextProposal(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	catalog, scope := governedActionCatalog(t)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent",
		Goal: "deploy once", Budget: &BudgetPolicy{MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{
		Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	ids := []string{"action-one", "proposal-one", "action-two", "proposal-two"}
	coordinator.newID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	proposal, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "staging"}, IdempotencyKey: "deploy-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil || persisted.BudgetState != BudgetStateExhausted || len(persisted.BudgetReservations) != 1 || persisted.BudgetUsage.Actions != 0 {
		t.Fatalf("reserved action budget = %#v, %v", persisted, err)
	}
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "opaque"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}))
	worker.now = func() time.Time { return now.Add(2 * time.Second) }
	worker.newID = func() string { return "execution-one" }
	executed, err := worker.RunOnce(context.Background(), scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.ID != proposal.Call.ID || executed.Run.BudgetUsage.Actions != 1 || len(executed.Run.BudgetReservations) != 0 ||
		executed.Event == nil || executed.Event.UsageDelta == nil || executed.Event.UsageDelta.Actions != 1 {
		t.Fatalf("settled action budget = %#v", executed)
	}
	claimed, err = store.ClaimNextAgentRun(context.Background(), AgentRunClaim{
		Scope: scope, WorkerID: "agent-worker", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		t.Fatalf("reclaim = %#v, %v", claimed, err)
	}
	coordinator.now = func() time.Time { return now.Add(4 * time.Second) }
	denied, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "staging"}, IdempotencyKey: "deploy-two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if denied.Call.Status != ActionCallStatusDenied || denied.Event.EventType != "budget.exhausted" {
		t.Fatalf("second proposal = %#v", denied)
	}
	paused, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil || paused.Status != AgentRunStatusPaused || paused.BudgetUsage.Actions != 1 || len(paused.BudgetReservations) != 0 {
		t.Fatalf("paused run = %#v, %v", paused, err)
	}
}

func TestActionWorkerRetriesWithoutLeakingCredentials(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	catalog, proposal := createRunnableAction(t, store, now)
	attempts := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "raw-secret-value"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("provider rejected raw-secret-value")
		}
		return map[string]interface{}{"ok": true}, nil
	}))
	current := now.Add(2 * time.Second)
	worker.now = func() time.Time { return current }
	worker.newID = func() string { return "event-" + string(rune('0'+attempts)) }
	first, err := worker.RunOnce(context.Background(), proposal.Call.Scope, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Call.Status != ActionCallStatusReady || first.Run != nil || !strings.Contains(first.Call.Error, "[REDACTED]") || strings.Contains(first.Call.Error, "raw-secret-value") {
		t.Fatalf("retry result mismatch: %#v", first)
	}
	persistedRun, err := store.GetAgentRun(context.Background(), proposal.Call.Scope, proposal.Call.RunID)
	if err != nil || persistedRun.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("run resumed during retry: %#v, %v", persistedRun, err)
	}
	current = first.Call.AvailableAt.Add(time.Millisecond)
	second, err := worker.RunOnce(context.Background(), proposal.Call.Scope, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Call.Status != ActionCallStatusSucceeded || second.Call.Attempt != 2 || second.Run.Status != AgentRunStatusQueued {
		t.Fatalf("retry did not complete: %#v", second)
	}
}

func TestActionWorkerRenewsLeaseDuringLongDispatch(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	catalog, proposal := createRunnableAction(t, store, now)
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(ctx context.Context, _ ActionDispatchInput) (map[string]interface{}, error) {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			call, err := store.GetActionCall(ctx, proposal.Call.Scope, proposal.Call.ID)
			if err != nil {
				return nil, err
			}
			if call.Revision >= 4 {
				return map[string]interface{}{"ok": true}, nil
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				return nil, errors.New("action lease was not renewed twice")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}))
	var clockTicks atomic.Int64
	worker.now = func() time.Time { return now.Add(2*time.Second + time.Duration(clockTicks.Add(1))*10*time.Millisecond) }
	result, err := worker.RunOnce(context.Background(), proposal.Call.Scope, "long-worker", 45*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Call.Status != ActionCallStatusSucceeded || result.Call.Revision < 4 {
		t.Fatalf("long action did not renew and complete: %#v", result.Call)
	}
}

func createRunnableAction(t *testing.T, store KernelStore, now time.Time) (*skill.Catalog, *ActionProposalResult) {
	t.Helper()
	catalog, scope := governedActionCatalog(t)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	ids := []string{"call", "proposal-event"}
	coordinator.newID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	proposal, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "deploy-production",
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog, proposal
}
