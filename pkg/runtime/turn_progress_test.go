package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTurnProgressIsDurableAndTenantScopedBeforeCompletion(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Inspect sources", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	_, err = coordinator.Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		if err := ReportTurnProgress(ctx, "I’ll inspect the available sources."); err != nil {
			return nil, err
		}
		if err := ReportTurnProgress(ctx, "I’ll inspect the available sources."); err != nil {
			return nil, err
		}
		events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.EventType == "turn.progress" {
				count++
				if event.TurnID != input.Turn.ID || event.Scope != scope {
					t.Fatalf("wrong identity: %+v", event)
				}
			}
		}
		if count != 1 {
			t.Fatalf("progress count=%d", count)
		}
		current, err := store.GetAgentTurn(ctx, scope, input.Turn.ID)
		if err != nil || current.Status != AgentTurnStatusRunning {
			t.Fatal("summary waited until completion")
		}
		foreign, err := store.ListActivity(ctx, ActivityFilter{Scope: Scope{Kind: "tenant", ID: "other"}, RunID: run.ID})
		if err != nil || len(foreign) != 0 {
			t.Fatalf("cross-tenant progress exposed: %v %v", foreign, err)
		}
		return &TurnOutcome{OutputSummary: "Sources checked.", NextRunStatus: AgentRunStatusCompleted}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestTurnProgressBoundsTextAndHonorsCancellation(t *testing.T) {
	seen := ""
	ctx, cancel := context.WithCancel(WithTurnProgress(t.Context(), func(_ context.Context, s string) error { seen = s; return nil }))
	if err := ReportTurnProgress(ctx, strings.Repeat("界", 2000)); err != nil {
		t.Fatal(err)
	}
	if len(seen) > 4096 || !utf8.ValidString(seen) {
		t.Fatal("summary bounds invalid")
	}
	before := seen
	cancel()
	if err := ReportTurnProgress(ctx, "late"); !errors.Is(err, context.Canceled) || seen != before {
		t.Fatalf("late summary: %v", err)
	}
}
