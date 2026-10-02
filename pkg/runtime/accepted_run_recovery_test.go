package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestAcceptedRunRecoveryPreservesUnprovableLegacyWorkWithoutAutomaticRetriesAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		scope := Scope{Kind: "tenant", ID: "legacy-recovery"}
		original, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Entrypoint: "manual", Goal: "Complete the accepted older method", Source: RunSourceSchedule,
			Context: map[string]interface{}{"runbookDefinitionId": "older-method", "runbookDefinitionVersion": "0", "inputs": map[string]interface{}{"preserved": "accepted"}},
			Plan:    map[string]interface{}{"runbook": map[string]interface{}{"id": "older-method", "version": "0"}}, Checkpoint: map[string]interface{}{"committed": float64(7)}, Budget: &BudgetPolicy{MaxAttempts: 4, MaxTurns: 8},
		})
		if err != nil {
			t.Fatal(err)
		}
		deployment, definition := acceptedMethodFixture(scope)
		catalog := &resolverCatalog{deployment: deployment, definition: definition, activation: &skill.ActivationSnapshot{SnapshotID: "live", Scope: skill.ScopeReference(scope), DeploymentID: deployment.ID}}
		resolver := TurnRunnerResolverFunc(func(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
			binding, err := ResolveCatalogTurnRunner(ctx, catalog, run, CatalogTurnResolverConfig{})
			if !errors.Is(err, ErrAcceptedRunExecution) {
				t.Fatalf("legacy method guessed current implementation: %#v %v", binding, err)
			}
			return binding, err
		})
		pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "recovery-worker", Now: at, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		pool.executeClaim(t.Context(), "recovery-worker", claimed)
		parked, err := store.GetAgentRun(t.Context(), scope, original.ID)
		if err != nil || parked.Status != AgentRunStatusPaused || parked.PausedFrom != AgentRunStatusRunning || parked.Error == "" || parked.WakeCondition != nil || !reflect.DeepEqual(parked.Context, claimed.Context) || !reflect.DeepEqual(parked.Plan, claimed.Plan) || !reflect.DeepEqual(parked.Checkpoint, claimed.Checkpoint) || !reflect.DeepEqual(parked.Output, claimed.Output) || parked.LastAppliedTurn != claimed.LastAppliedTurn || !reflect.DeepEqual(parked.BudgetUsage, claimed.BudgetUsage) {
			t.Fatalf("legacy recovery changed or lost accepted work: %#v %v", parked, err)
		}
		calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: original.ID})
		if err != nil || len(calls) != 0 {
			t.Fatalf("unprovable continuation dispatched actions: %#v %v", calls, err)
		}
		activities, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: original.ID, EventTypes: []string{"run.execution_recovery_required"}, Descending: true})
		if err != nil || len(activities) != 1 {
			t.Fatalf("recovery classification missing/duplicated: %#v %v", activities, err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
		}
		wake := NewAgentRunWakeService(store, store)
		for i := 0; i < 3; i++ {
			if result, err := wake.WakeDueTimers(t.Context(), scope, at.Add(time.Duration(i+1)*time.Hour)); err != nil || len(result.Runs) != 0 {
				t.Fatalf("manual recovery automatically woke: %#v %v", result, err)
			}
			if next, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "another-worker", Now: at.Add(time.Duration(i+1) * time.Hour), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || next != nil {
				t.Fatalf("manual recovery automatically retried: %#v %v", next, err)
			}
		}
		final, err := store.GetAgentRun(t.Context(), scope, original.ID)
		if err != nil || !reflect.DeepEqual(final, parked) {
			t.Fatalf("restart/timer polling rewrote parked history or consumed budget: %#v %v", final, err)
		}
	})
}
