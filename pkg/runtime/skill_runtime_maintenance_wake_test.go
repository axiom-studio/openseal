package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSkillRuntimeMaintenanceWaitPreservesCheckpointBudgetAndResumesAfterRestart(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		request := skillRuntimeMaintenanceEmptyRequest()
		request.Now = time.Now().UTC()
		request.LeaseDuration = 2 * time.Minute
		gate, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		portfolio := NewPortfolioService(store)
		portfolio.now = func() time.Time { return request.Now }
		before, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: request.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Goal: "Complete the original request", Source: RunSourceManual,
			Checkpoint: map[string]interface{}{"committed": float64(7)}, Budget: &BudgetPolicy{MaxAttempts: 2, MaxTurns: 4}})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: request.Scope, WorkerID: "chat-worker", Now: request.Now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
			t.Fatal("model ran while runtime was held")
			return nil, nil
		})
		pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: request.Scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		turn := &AgentTurn{ID: "pending-turn", ContinuationCheckpoint: map[string]interface{}{"inventedObservation": "must not commit"}}
		pool.failMaterialization(t.Context(), "chat-worker", claimed, turn, &SkillRuntimeMaintenanceError{Maintenance: *gate})
		parked, err := store.GetAgentRun(t.Context(), request.Scope, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if parked.Status != AgentRunStatusWaitingForDependency || parked.WakeCondition == nil || parked.WakeCondition.Type != skillRuntimeMaintenanceWakeType || parked.Error != "" || parked.Checkpoint["committed"] != float64(7) || parked.Checkpoint["inventedObservation"] != nil || parked.Checkpoint[proposalRecoveryCheckpointKey] != nil {
			t.Fatalf("unsafe parked Run: %#v", parked)
		}
		wake := NewAgentRunWakeService(store, store)
		for i := 0; i < 5; i++ {
			result, err := wake.WakeDueTimers(t.Context(), request.Scope, request.Now.Add(time.Duration(i+1)*time.Second))
			if err != nil || len(result.Runs) != 0 {
				t.Fatalf("active maintenance woke Run: %#v %v", result, err)
			}
		}
		still, err := store.GetAgentRun(t.Context(), request.Scope, before.ID)
		if err != nil || !reflect.DeepEqual(still.BudgetUsage, parked.BudgetUsage) || still.Attempt != parked.Attempt || still.Revision != parked.Revision {
			t.Fatalf("maintenance polling consumed work budget: %#v %v", still, err)
		}
		completion := skillRuntimeMaintenanceCompletion(gate, request.Now.Add(10*time.Second))
		if _, err := store.(SkillRuntimeMaintenanceStore).CompleteSkillRuntimeMaintenance(t.Context(), completion); err != nil {
			t.Fatal(err)
		}
		// Crash after the gate commit and before any wake notification. The next
		// bounded scheduler pass must reconstruct the waiter from durable state.
		store = reopenSkillRuntimeMaintenanceStore(t, store).(KernelStore)
		wake = NewAgentRunWakeService(store, store)
		result, err := wake.WakeDueTimers(t.Context(), request.Scope, request.Now.Add(11*time.Second))
		if err != nil || len(result.Runs) != 1 {
			t.Fatalf("completed maintenance did not wake durable waiter: %#v %v", result, err)
		}
		resumed := result.Runs[0].Run
		if resumed.Status != AgentRunStatusQueued || resumed.Goal != before.Goal || resumed.Checkpoint["committed"] != float64(7) || resumed.Checkpoint[skillRuntimeMaintenanceCheckpointKey] != nil || !reflect.DeepEqual(resumed.BudgetUsage, parked.BudgetUsage) {
			t.Fatalf("resumption changed original work: %#v", resumed)
		}
		if again, err := wake.WakeDueTimers(t.Context(), request.Scope, request.Now.Add(12*time.Second)); err != nil || len(again.Runs) != 0 {
			t.Fatalf("waiter resumed twice: %#v %v", again, err)
		}
	})
}

func TestSkillRuntimeMaintenanceResolverErrorParksBeforeStartingTurn(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		request := skillRuntimeMaintenanceEmptyRequest()
		request.Now = time.Now().UTC()
		request.LeaseDuration = 2 * time.Minute
		gate, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		portfolio := NewPortfolioService(store)
		portfolio.now = func() time.Time { return request.Now }
		run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: request.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Goal: "Continue the original conversation", Source: RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: request.Scope, WorkerID: "chat-worker", Now: request.Now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
			return nil, &SkillRuntimeMaintenanceError{Maintenance: *gate}
		})
		pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: request.Scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		pool.executeClaim(t.Context(), "chat-worker", claimed)
		parked, err := store.GetAgentRun(t.Context(), request.Scope, run.ID)
		if err != nil || parked.Status != AgentRunStatusWaitingForDependency || parked.Error != "" || parked.Goal != run.Goal || parked.WakeCondition == nil || parked.WakeCondition.Reference != request.SkillID || parked.BudgetUsage.Turns != 0 {
			t.Fatalf("runner resolution maintenance lost work: %#v %v", parked, err)
		}
		if _, err := store.(SkillRuntimeMaintenanceStore).CompleteSkillRuntimeMaintenance(t.Context(), skillRuntimeMaintenanceCompletion(gate, request.Now.Add(time.Second))); err != nil {
			t.Fatal(err)
		}
		result, err := NewAgentRunWakeService(store, store).WakeDueTimers(t.Context(), request.Scope, request.Now.Add(2*time.Second))
		if err != nil || len(result.Runs) != 1 || result.Runs[0].Run.ID != run.ID || result.Runs[0].Run.Status != AgentRunStatusQueued || result.Runs[0].Run.Goal != run.Goal {
			t.Fatalf("resolver wait did not resume original request: %#v %v", result, err)
		}
	})
}
