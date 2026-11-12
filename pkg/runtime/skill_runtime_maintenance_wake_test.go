package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestSkillRuntimeMaintenanceResolverErrorParksBeforeStartingTurn(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		request := skillRuntimeMaintenanceEmptyRequest()
		request.Now = time.Now().UTC()
		request.LeaseDuration = 2 * time.Minute
		gate, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return request.Now }))
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
