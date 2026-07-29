package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestTurnCoordinatorReconcilesPersistedTurnWithoutReinvocation(t *testing.T) {
	store := NewMemoryStore()
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
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "event", Version: "1", Name: "Event", Entrypoints: map[string]string{"manual": "await-event"}, Steps: map[string]runbook.Step{
		"await-event": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Event: "work.ready", Next: "done"}},
		"done":        {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	runner := TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		if input.Run.ID != run.ID || input.Turn.Sequence != 1 {
			t.Fatalf("unexpected turn input: %#v", input)
		}
		checkpoint := map[string]interface{}{"phase": "waiting", "runbook": map[string]interface{}{"current": "await-event"}}
		sequence, traceErr := beginRunbookStepTrace(checkpoint, definition, "await-event", input.Turn.ID, time.Now())
		if traceErr != nil {
			return nil, traceErr
		}
		if traceErr := updateRunbookStepTrace(checkpoint, sequence, RunbookStepTraceWaiting, "", "Waiting for condition", "", nil, time.Now()); traceErr != nil {
			return nil, traceErr
		}
		return &TurnOutcome{
			OutputSummary: "Waiting for the next event", NextRunStatus: AgentRunStatusWaitingForEvent,
			SkillSelections:        []HostedSkillSelection{{SkillRef: "skill:events@1", Disposition: HostedSkillApplied, Summary: "Applied event monitoring"}},
			WakeCondition:          &WakeCondition{Type: "event", Reference: "work.ready"},
			ContinuationCheckpoint: checkpoint,
			Decisions:              []TurnDecision{{Summary: "Use the event monitor", EvidenceRefs: []string{"skill:events@1"}}},
		}, nil
	})
	partial, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1", Model: "fake",
		DefinitionID: "event-agent", DefinitionVersion: "7",
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
	auditPayload, marshalErr := json.Marshal(result.Event.Payload)
	if marshalErr != nil || !strings.Contains(string(auditPayload), `"evidenceRefs":["skill:events@1"]`) || !strings.Contains(string(auditPayload), `"disposition":"applied"`) || !strings.Contains(string(auditPayload), `"runbookTraceDelta"`) || !strings.Contains(string(auditPayload), `"status":"waiting"`) || fmt.Sprint(result.Event.Payload["turnSequence"]) != "1" ||
		result.Event.Payload["definitionId"] != "event-agent" || result.Event.Payload["definitionVersion"] != "7" || result.Event.Payload["runbookStep"] != "await-event" {
		t.Fatalf("activity does not expose bounded-turn audit evidence: %#v", result.Event.Payload)
	}
	if result.Run.WakeCondition == nil || result.Run.WakeCondition.Reference != "work.ready" || result.Run.Checkpoint["phase"] != "waiting" {
		t.Fatalf("checkpoint was not applied: %#v", result.Run)
	}
}

func TestTurnCoordinatorPersistsRunnerFailure(t *testing.T) {
	store := NewMemoryStore()
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
	store := NewMemoryStore()
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
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "done", ModelProvider: "failover-host", Model: "selected-model"}, nil
	})
	first, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1",
	}, runner)
	if !errors.Is(err, ErrTurnHostUnavailable) || first.Run.Status != AgentRunStatusSleeping || first.Run.WakeCondition == nil || first.Run.WakeCondition.Reference != "hosted-turn-retry" || first.Turn.Status != AgentTurnStatusRunning || first.Turn.LeaseOwner != "" || first.Event.EventType != "turn.retry_scheduled" || first.Event.TurnID != first.Turn.ID || first.Event.CausationID != first.Turn.ID {
		t.Fatalf("retry result=%#v err=%v", first, err)
	}
	if got := first.Event.Payload["attempt"]; fmt.Sprint(got) != "1" {
		t.Fatalf("retry attempt = %#v", got)
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
	if err != nil || len(turns) != 1 || turns[0].ID != turnID || turns[0].ModelProvider != "failover-host" || turns[0].Model != "selected-model" {
		t.Fatalf("turns=%#v err=%v", turns, err)
	}
}

func TestHostedTurnRetryDelayIsBoundedExponential(t *testing.T) {
	tests := []struct {
		attempt int64
		want    time.Duration
	}{
		{attempt: -1, want: 5 * time.Second},
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 3, want: 20 * time.Second},
		{attempt: 4, want: 40 * time.Second},
		{attempt: 20, want: 40 * time.Second},
	}
	for _, test := range tests {
		if got := hostedTurnRetryDelay(test.attempt); got != test.want {
			t.Fatalf("attempt %d delay = %s, want %s", test.attempt, got, test.want)
		}
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
	store := NewMemoryStore()
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
	store := NewMemoryStore()
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

func TestTurnCoordinatorPreservesActionProposedAtBudgetLimit(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "local", ID: "action-at-budget-limit"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "perform one bounded action",
		Budget: &BudgetPolicy{MaxTurns: 1, MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusRunning, OutputSummary: "Start the browser",
			ProposedActions: []TurnAction{{Type: "skill_action", Capability: "browser.start", Summary: "Start the browser"}},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusRunning || result.Run.BudgetState != BudgetStateExhausted ||
		result.Turn.NextRunStatus != AgentRunStatusRunning || len(result.Turn.RequestedActions) != 1 {
		t.Fatalf("action proposal at budget limit = %#v", result)
	}
}

func TestTurnCoordinatorAllowsTerminalOutcomeAtExactBudgetLimit(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "terminal-budget"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "deliver once",
		Budget: &BudgetPolicy{MaxTurns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", Model: "fake",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Delivery accepted",
			RunOutput: map[string]interface{}{"receiptId": "receipt-1"},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusCompleted || result.Run.BudgetState != BudgetStateExhausted || result.Run.BudgetUsage.Turns != 1 || result.Run.Output["receiptId"] != "receipt-1" {
		t.Fatalf("terminal budget result = %#v", result.Run)
	}
	if result.Turn.NextRunStatus != AgentRunStatusCompleted || result.Turn.OutputSummary != "Delivery accepted" {
		t.Fatalf("terminal turn was overwritten = %#v", result.Turn)
	}
}

func TestTurnCoordinatorCancelsAtDurableDurationCeiling(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "duration-budget"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "remain bounded in wall time",
		Budget: &BudgetPolicy{MaxTurns: 3, MaxDurationMS: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", LeaseDuration: time.Second,
	}, TurnRunnerFunc(func(ctx context.Context, _ TurnExecutionContext) (*TurnOutcome, error) {
		<-ctx.Done()
		// Enterprise transports commonly normalize a canceled request into
		// host-unavailable. The kernel-owned deadline must take precedence.
		return nil, ErrTurnHostUnavailable
	}))
	if !errors.Is(err, ErrBudgetExhausted) || time.Since(started) > time.Second {
		t.Fatalf("duration enforcement err=%v elapsed=%s", err, time.Since(started))
	}
	if result == nil || result.Run.Status != AgentRunStatusPaused || result.Run.BudgetState != BudgetStateExhausted ||
		result.Run.BudgetUsage.Turns != 1 || result.Run.BudgetUsage.DurationMS < 15 || result.Event == nil || result.Event.EventType != "budget.exhausted" {
		t.Fatalf("duration result = %#v", result)
	}
	if result.Event.UsageDelta == nil || result.Event.UsageDelta.Turns != 1 || result.Event.UsageDelta.DurationMS < 15 {
		t.Fatalf("duration activity usage = %#v", result.Event.UsageDelta)
	}
	if result.Run.Error != "" || result.Turn.Status != AgentTurnStatusCanceled {
		t.Fatalf("duration ceiling masqueraded as failure: run=%#v turn=%#v", result.Run, result.Turn)
	}
}

func TestTurnBudgetReconciliationDoesNotDoubleCharge(t *testing.T) {
	store := NewMemoryStore()
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
	store := NewMemoryStore()
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
