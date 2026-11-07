package openseal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func conversationBudgetEngine(t *testing.T, store *runtime.MemoryStore, scope Scope, budget *BudgetPolicy, resolver TurnRunnerResolver) *Engine {
	t.Helper()
	engine, err := New(WithStore(store), WithConversationCoordinator(
		ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return nil, fmt.Errorf("Agent channel cannot resolve a Team roster")
		}),
		ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
			return ConversationRouteDecision{}, fmt.Errorf("Agent channel cannot route Team messages")
		}), DefaultConversationCoordinatorConfig()),
		WithDynamicConversationRuns(ConversationRunConfig{Scheduler: ConversationRunSchedulerConfig{Budget: budget}, Runner: ConversationRunTurnRunnerConfig{AgentTurns: resolver}, Workers: DynamicAgentRunWorkerConfig{Kind: RunKindConversation, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, Reconciler: ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond}}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })),
		WithAgentRunWorkers(AgentRunWorkerConfig{Scope: scope, Kind: RunKindAgentWork, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, resolver))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func conversationBudgetWait(t *testing.T, ctx context.Context, engine *Engine, scope Scope, id string, expected AgentRunStatus) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := engine.GetAgentRun(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == expected {
			return
		}
		if run.Status == AgentRunStatusCompleted || run.Status == AgentRunStatusFailed || run.Status == AgentRunStatusCanceled || run.Status == AgentRunStatusPaused {
			t.Fatalf("run %s status=%s expected=%s error=%s", id, run.Status, expected, run.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run %s did not reach %s: %v", id, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}
