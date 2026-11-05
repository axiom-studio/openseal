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

func TestCommentaryPersistsSeparatelyFromMatchingActivitySummary(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Inspect", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		for _, report := range []func(context.Context, string) error{ReportTurnProgress, ReportTurnCommentary, ReportTurnCommentary} {
			if err := report(ctx, "Let’s check the sources."); err != nil {
				return nil, err
			}
		}
		events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, event := range events {
			if event.EventType == "turn.progress" || event.EventType == "turn.commentary" {
				counts[event.EventType]++
				if event.Scope != scope || event.TurnID != input.Turn.ID || event.Visibility != ActivityVisibilityScope {
					t.Fatalf("wrong identity: %+v", event)
				}
			}
		}
		if counts["turn.progress"] != 1 || counts["turn.commentary"] != 1 {
			t.Fatalf("wrong event kinds: %v", counts)
		}
		return &TurnOutcome{OutputSummary: "Done", NextRunStatus: AgentRunStatusCompleted}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestTurnProgressCarriesItsOriginRef(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Inspect", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		first := WithTurnProgressOrigin(ctx, " command:ab ")
		second := WithTurnProgressOrigin(ctx, "command:cd")
		for _, step := range []struct {
			ctx     context.Context
			summary string
		}{{first, "Running a command in the secure VM…"}, {first, "Running a command in the secure VM"},
			{second, "Running a command in the secure VM…"}, {second, "Running a command in the secure VM"},
			{WithTurnProgressOrigin(ctx, strings.Repeat("x", MaxTurnProgressOriginRefBytes+1)), "Saving report.html"}, {ctx, "Saving report.html…"}} {
			if err := ReportTurnProgress(step.ctx, step.summary); err != nil {
				return nil, err
			}
		}
		return &TurnOutcome{OutputSummary: "Done.", NextRunStatus: AgentRunStatusCompleted}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{"turn.progress"}})
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, event := range events {
		ref, _ := event.Payload["originRef"].(string)
		refs = append(refs, ref)
	}
	// The same step text under a new command is a new step, not a repeat.
	if strings.Join(refs, ",") != "command:ab,command:ab,command:cd,command:cd,," {
		t.Fatalf("progress refs = %q", refs)
	}
	if feed, err := NewRunActivityService(store, store).ListActivityFeed(t.Context(), ActivityFeedRequest{Scope: scope, RunID: run.ID, EventTypes: []string{"turn.progress"}, IncludeDetails: true}); err != nil || len(feed.Items) != len(events) || feed.Items[len(feed.Items)-1].Payload["originRef"] != "command:ab" {
		t.Fatalf("feed: %+v %v", feed, err)
	}
}
