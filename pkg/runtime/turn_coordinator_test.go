package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTurnCoordinatorReconcilesPersistedTurnWithoutReinvocation(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "wait for work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	forcedCrash := errors.New("forced crash after persisted turn")
	coordinator.afterTurnPersisted = func() error { return forcedCrash }
	calls := 0
	runner := TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		if input.Run.ID != run.ID || input.Turn.Sequence != 1 {
			t.Fatalf("unexpected turn input: %#v", input)
		}
		return &TurnOutcome{
			OutputSummary: "Waiting for the next event", NextRunStatus: AgentRunStatusWaitingForEvent,
			WakeCondition:          &WakeCondition{Type: "event", Reference: "work.ready"},
			ContinuationCheckpoint: map[string]interface{}{"phase": "waiting"},
		}, nil
	})
	partial, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1", Model: "fake",
	}, runner)
	if !errors.Is(err, forcedCrash) {
		t.Fatalf("advance error = %v", err)
	}
	if partial.Turn.Status != AgentTurnStatusCompleted || calls != 1 {
		t.Fatalf("turn was not persisted before crash: result=%#v calls=%d", partial, calls)
	}
	loaded, err := NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != AgentRunStatusRunning || loaded.LastAppliedTurn != 0 {
		t.Fatalf("run advanced before reconciliation: %#v", loaded)
	}

	restarted := NewTurnCoordinator(store, store, store)
	result, err := restarted.Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2", Model: "fake",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
	if !result.Reconciled || result.Run.Status != AgentRunStatusWaitingForEvent || result.Run.LastAppliedTurn != 1 {
		t.Fatalf("unexpected reconciliation: %#v", result)
	}
	if result.Event.TurnID != result.Turn.ID || result.Event.CausationID != result.Turn.ID {
		t.Fatalf("activity is not linked to turn: %#v", result.Event)
	}
	if result.Run.WakeCondition == nil || result.Run.WakeCondition.Reference != "work.ready" || result.Run.Checkpoint["phase"] != "waiting" {
		t.Fatalf("checkpoint was not applied: %#v", result.Run)
	}
}

func TestTurnCoordinatorPersistsRunnerFailure(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", Model: "fake",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return nil, errors.New("provider unavailable")
	}))
	if err == nil || err.Error() != "provider unavailable" {
		t.Fatalf("advance error = %v", err)
	}
	if result.Turn.Status != AgentTurnStatusFailed || result.Run.Status != AgentRunStatusFailed || result.Run.LastAppliedTurn != 1 {
		t.Fatalf("failure was not durable: %#v", result)
	}
}

func TestTurnCoordinatorRequeuesSameTurnWhenHostIsUnavailable(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAgentRunScheduler(store).ClaimNext(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: "worker-1"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	calls := 0
	var turnID string
	runner := TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		if turnID == "" {
			turnID = input.Turn.ID
		} else if input.Turn.ID != turnID {
			t.Fatalf("retry created a different turn: first=%s retry=%s", turnID, input.Turn.ID)
		}
		if calls == 1 {
			return nil, retryableTurnHostError{cause: errors.New("connection reset")}
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "done"}, nil
	})
	first, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1",
	}, runner)
	if !errors.Is(err, ErrTurnHostUnavailable) || first.Run.Status != AgentRunStatusSleeping || first.Run.WakeCondition == nil || first.Run.WakeCondition.Reference != "hosted-turn-retry" || first.Turn.Status != AgentTurnStatusRunning || first.Turn.LeaseOwner != "" || first.Event.EventType != "turn.retry_scheduled" {
		t.Fatalf("retry result=%#v err=%v", first, err)
	}
	retryNow := first.Run.WakeCondition.WakeAt.Add(time.Second)
	if _, err := NewAgentRunWakeService(store, store).WakeDueTimers(ctx, scope, retryNow); err != nil {
		t.Fatal(err)
	}
	retryScheduler := NewAgentRunScheduler(store)
	retryScheduler.now = func() time.Time { return retryNow }
	claimed, err = retryScheduler.ClaimNext(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: "worker-2"})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("retry claim=%#v err=%v", claimed, err)
	}
	second, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2",
	}, runner)
	if err != nil || second.Run.Status != AgentRunStatusCompleted || second.Run.LastAppliedTurn != 1 || calls != 2 {
		t.Fatalf("completion=%#v calls=%d err=%v", second, calls, err)
	}
	turns, err := NewAgentTurnService(store, store).ListTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID, Limit: 10})
	if err != nil || len(turns) != 1 || turns[0].ID != turnID {
		t.Fatalf("turns=%#v err=%v", turns, err)
	}
}

func TestTurnCoordinatorReconcilesAcrossSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coordinator.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	forcedCrash := errors.New("forced crash")
	coordinator.afterTurnPersisted = func() error { return forcedCrash }
	calls := 0
	runner := TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done", RunOutput: map[string]interface{}{"ok": true}}, nil
	})
	if _, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1", Model: "fake",
	}, runner); !errors.Is(err, forcedCrash) {
		t.Fatalf("advance error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := NewTurnCoordinator(reopened, reopened, reopened).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2", Model: "fake",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.Reconciled || result.Run.Status != AgentRunStatusCompleted || result.Run.Output["ok"] != true {
		t.Fatalf("unexpected restart reconciliation: calls=%d result=%#v", calls, result)
	}
}

func TestTurnCoordinatorRejectsConcurrentLiveWorker(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	first := NewTurnCoordinator(store, store, store)
	go func() {
		_, advanceErr := first.Advance(ctx, AdvanceAgentRunRequest{
			Scope: scope, RunID: run.ID, WorkerID: "worker-1", Model: "fake",
		}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			close(started)
			<-release
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
		}))
		firstDone <- advanceErr
	}()
	<-started
	_, err = NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2", Model: "fake",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		t.Fatal("concurrent runner was invoked")
		return nil, nil
	}))
	if !errors.Is(err, ErrTurnLeaseHeld) {
		t.Fatalf("concurrent advance error = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestTurnCoordinatorPausesAndAccountsExhaustedBudget(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "budget"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "bounded work",
		Budget: &BudgetPolicy{MaxTurns: 1, MaxTotalTokens: 30, MaxCostMicros: 250_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", Model: "fake",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusRunning, OutputSummary: "more work remains",
			Usage: TurnUsage{InputTokens: 10, OutputTokens: 20, Cost: 0.25, DurationMS: 500},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusPaused || result.Run.BudgetState != BudgetStateExhausted ||
		result.Run.BudgetUsage.Turns != 1 || result.Run.BudgetUsage.InputTokens != 10 ||
		result.Run.BudgetUsage.OutputTokens != 20 || result.Run.BudgetUsage.CostMicros != 250_000 ||
		result.Run.BudgetUsage.DurationMS != 500 {
		t.Fatalf("budgeted run = %#v", result.Run)
	}
	if result.Run.Error != "" || result.Turn.NextRunStatus != AgentRunStatusPaused {
		t.Fatalf("budget exhaustion should pause without masquerading as failure: run=%#v turn=%#v", result.Run, result.Turn)
	}
}

func TestTurnBudgetReconciliationDoesNotDoubleCharge(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "budget-recovery"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "recover",
		Budget: &BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	forcedCrash := errors.New("forced crash")
	coordinator.afterTurnPersisted = func() error { return forcedCrash }
	runner := TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, Usage: TurnUsage{InputTokens: 7}}, nil
	})
	if _, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "first"}, runner); !errors.Is(err, forcedCrash) {
		t.Fatalf("advance error = %v", err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "recovery"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reconciled || result.Run.BudgetUsage.Turns != 1 || result.Run.BudgetUsage.InputTokens != 7 {
		t.Fatalf("reconciled budget = %#v", result.Run.BudgetUsage)
	}
	second, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "replay"}, runner)
	if err == nil || second != nil {
		t.Fatalf("terminal replay unexpectedly advanced: result=%#v err=%v", second, err)
	}
	loaded, err := NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
	if err != nil || loaded.BudgetUsage.Turns != 1 {
		t.Fatalf("replay usage = %#v, err=%v", loaded, err)
	}
}

func TestTurnBudgetReservationPreventsKnownOverspend(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "budget-reservation"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "bounded",
		Budget: &BudgetPolicy{MaxInputTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", BudgetReservation: BudgetUsage{InputTokens: 11},
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted}, nil
	}))
	if !errors.Is(err, ErrBudgetExhausted) || result == nil || result.Run.Status != AgentRunStatusPaused || calls != 0 {
		t.Fatalf("reservation result=%#v err=%v calls=%d", result, err, calls)
	}
	if result.Run.BudgetUsage != (BudgetUsage{}) || len(result.Run.BudgetReservations) != 0 || result.Turn.Status != AgentTurnStatusCanceled {
		t.Fatalf("rejected reservation mutated usage: run=%#v turn=%#v", result.Run, result.Turn)
	}
}
