package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
