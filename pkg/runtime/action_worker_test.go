package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionWorkerExecutesGovernedDependencyAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(20), func() {} }},
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
				return map[string]interface{}{"deploymentId": "deploy-123", "status": "healthy"}, nil
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
			if result.Run.Checkpoint["lastAction"].(map[string]interface{})["actionCallId"] != proposal.Call.ID {
				t.Fatalf("run checkpoint missing action reference: %#v", result.Run.Checkpoint)
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

func TestActionWorkerRetriesWithoutLeakingCredentials(t *testing.T) {
	store := NewMemoryStore(20)
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
	store := NewMemoryStore(20)
	now := time.Now().UTC()
	catalog, proposal := createRunnableAction(t, store, now)
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(ctx context.Context, _ ActionDispatchInput) (map[string]interface{}, error) {
		select {
		case <-time.After(120 * time.Millisecond):
			return map[string]interface{}{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	started := time.Now()
	worker.now = func() time.Time { return now.Add(2 * time.Second).Add(time.Since(started)) }
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
