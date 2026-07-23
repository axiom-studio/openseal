package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type countedHostedTurnHost struct {
	calls   int
	request HostedTurnRequest
}

func (h *countedHostedTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.calls++
	h.request = request
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
		ModelProvider: "test", Model: "test-model", NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "done", Usage: TurnUsage{InputTokens: 100, OutputTokens: 20},
	}, nil
}

func TestHostedTurnBudgetPreflightRejectsBeforeProviderDispatch(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxTotalTokens: 2000},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &countedHostedTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker",
	}, runner)
	if !errors.Is(err, ErrBudgetExhausted) || result == nil || result.Run.Status != AgentRunStatusPaused || host.calls != 0 {
		t.Fatalf("result=%#v err=%v providerCalls=%d", result, err, host.calls)
	}
	if result.Turn == nil || result.Turn.Status != AgentTurnStatusCanceled || result.Event == nil || result.Event.EventType != "budget.exhausted" {
		t.Fatalf("turn=%#v event=%#v", result.Turn, result.Event)
	}
	if result.Run.BudgetUsage != (BudgetUsage{}) || len(result.Run.BudgetReservations) != 0 {
		t.Fatalf("rejected preflight consumed budget: %#v", result.Run)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.UsageDelta != nil {
			t.Fatalf("rejected preflight reported usage: %#v", event)
		}
	}
}

func TestHostedTurnBudgetReservationCapsProviderOutputAndSettlesActualUsage(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{
			MaxAttempts: 3, MaxTurns: 3, MaxInputTokens: 8000, MaxOutputTokens: 2000,
			MaxTotalTokens: 10000, MaxDurationMS: 120000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &countedHostedTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if host.calls != 1 || host.request.Budget == nil {
		t.Fatalf("provider calls=%d budget=%#v", host.calls, host.request.Budget)
	}
	reserved := host.request.Budget.TurnReservation
	if reserved.InputTokens < HostedTurnProtocolInputReserveTokens || reserved.OutputTokens <= 0 || reserved.OutputTokens > 2000 {
		t.Fatalf("reservation=%#v", reserved)
	}
	if host.request.Budget.Remaining.MaxTotalTokens != 10000 || host.request.Budget.Remaining.MaxOutputTokens != 2000 {
		t.Fatalf("remaining budget must exclude current reservation: %#v", host.request.Budget.Remaining)
	}
	if host.request.Budget.MinimumChild.MaxAttempts != HostedTurnMinimumChildAttempts ||
		host.request.Budget.MinimumChild.MaxTurns != HostedTurnMinimumChildTurns ||
		host.request.Budget.MinimumChild.MaxInputTokens != HostedTurnMinimumChildInputTokens ||
		host.request.Budget.MinimumChild.MaxOutputTokens != HostedTurnMinimumChildOutputTokens ||
		host.request.Budget.MinimumChild.MaxTotalTokens != HostedTurnMinimumChildTotalTokens ||
		host.request.Budget.MinimumChild.MaxDurationMS != HostedTurnMinimumChildDurationMS {
		t.Fatalf("minimum child budget = %#v", host.request.Budget.MinimumChild)
	}
	if host.request.Budget.Policy.MaxActions != 0 || host.request.Budget.Policy.MaxCostMicros != 0 ||
		host.request.Budget.Remaining.MaxActions != 0 || host.request.Budget.Remaining.MaxCostMicros != 0 ||
		host.request.Budget.MinimumChild.MaxActions != 0 || host.request.Budget.MinimumChild.MaxCostMicros != 0 {
		t.Fatalf("omitted action and cost dimensions must remain unbounded: %#v", host.request.Budget)
	}
	if result.Run.Status != AgentRunStatusCompleted || result.Run.BudgetUsage.Turns != 1 || result.Run.BudgetUsage.InputTokens != 100 || result.Run.BudgetUsage.OutputTokens != 20 || len(result.Run.BudgetReservations) != 0 {
		t.Fatalf("settled run=%#v", result.Run)
	}
}

func TestHostedTurnInputEstimateIsStableAfterReservationProjection(t *testing.T) {
	request := HostedTurnRequest{
		Goal: "Do bounded work",
		Budget: &HostedRunBudget{
			Policy:    BudgetPolicy{MaxTotalTokens: 10000},
			Remaining: BudgetPolicy{MaxTotalTokens: 10000},
		},
	}
	before, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	withoutReservation, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Budget.TurnReservation = BudgetUsage{Turns: 1, InputTokens: before, OutputTokens: 2048}
	after, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	withReservation, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("estimate changed after reservation projection: before=%d after=%d", before, after)
	}
	if growth := int64(len(withReservation) - len(withoutReservation)); growth <= 0 || growth > HostedTurnBudgetEnvelopeReserveTokens {
		t.Fatalf("reservation envelope growth=%d reserve=%d", growth, HostedTurnBudgetEnvelopeReserveTokens)
	}
}

func TestGroundedHostedTurnReservationIncludesDraftInstruction(t *testing.T) {
	_, runContext := groundedSnapshot(t)
	runner, err := NewHostedTurnRunner(&countedHostedTurnHost{}, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{
		ID: "grounded", Scope: Scope{Kind: "tenant", ID: "7"}, Goal: "Synthesize cited findings", Context: runContext,
		Budget: &BudgetPolicy{MaxInputTokens: 32000, MaxOutputTokens: 30000, MaxTotalTokens: 62000},
	}
	turn := &AgentTurn{ID: "turn"}
	planned, err := runner.PlanTurnBudget(t.Context(), TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	request, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := evidenceSnapshotForGrounding(run.Context)
	if err != nil {
		t.Fatal(err)
	}
	applyEvidenceGroundingDraftInstruction(&request, snapshot)
	estimate, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	if planned.InputTokens != estimate || !containsString(request.SystemInstructions, evidenceGroundingDraftInstruction) {
		t.Fatalf("planned=%#v estimate=%d instructions=%#v", planned, estimate, request.SystemInstructions)
	}
}

type blockingHostedTurnHost struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
	request HostedTurnRequest
}

func (h *blockingHostedTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.mu.Lock()
	h.calls++
	h.request = request
	h.mu.Unlock()
	close(h.started)
	<-h.release
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
		ModelProvider: "test", Model: "test-model", NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "done", Usage: TurnUsage{InputTokens: 100, OutputTokens: 20},
	}, nil
}

func TestHostedTurnBudgetReservationIsAtomicAgainstConcurrentWorker(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "atomic-budget"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &blockingHostedTurnHost{started: make(chan struct{}), release: make(chan struct{})}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, advanceErr := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
			Scope: scope, RunID: run.ID, WorkerID: "worker-1",
		}, runner)
		firstDone <- advanceErr
	}()
	<-host.started
	reserved, err := NewPortfolioService(store).GetAgentRun(t.Context(), scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reserved.BudgetReservations) != 1 || reserved.BudgetUsage != (BudgetUsage{}) {
		t.Fatalf("in-flight budget state = %#v", reserved)
	}
	_, concurrentErr := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2",
	}, runner)
	if !errors.Is(concurrentErr, ErrTurnLeaseHeld) {
		t.Fatalf("concurrent advance error = %v", concurrentErr)
	}
	host.mu.Lock()
	providerCalls := host.calls
	host.mu.Unlock()
	if providerCalls != 1 {
		t.Fatalf("provider calls during concurrent advance = %d", providerCalls)
	}
	close(host.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	settled, err := NewPortfolioService(store).GetAgentRun(t.Context(), scope, run.ID)
	if err != nil || settled.BudgetUsage.Turns != 1 || settled.BudgetUsage.InputTokens != 100 || settled.BudgetUsage.OutputTokens != 20 || len(settled.BudgetReservations) != 0 {
		t.Fatalf("settled budget state = %#v, %v", settled, err)
	}
}

type retryingHostedTurnHost struct {
	calls        int
	invocations  []string
	reservations []BudgetUsage
}

func (h *retryingHostedTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.calls++
	h.invocations = append(h.invocations, request.InvocationID)
	h.reservations = append(h.reservations, request.Budget.TurnReservation)
	if h.calls == 1 {
		return nil, errors.New("temporary provider outage")
	}
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
		ModelProvider: "test", Model: "test-model", NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "done", Usage: TurnUsage{InputTokens: 100, OutputTokens: 20},
	}, nil
}

func TestHostedTurnBudgetRetryReusesAndSettlesOneReservation(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "retry-budget"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &retryingHostedTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1",
	}, runner)
	if !errors.Is(err, ErrTurnHostUnavailable) || first == nil || first.Run.Status != AgentRunStatusSleeping || len(first.Run.BudgetReservations) != 1 || first.Run.BudgetUsage != (BudgetUsage{}) {
		t.Fatalf("retry scheduling = %#v, %v", first, err)
	}
	retryAt := first.Run.WakeCondition.WakeAt.Add(time.Second)
	if _, err := NewAgentRunWakeService(store, store).WakeDueTimers(t.Context(), scope, retryAt); err != nil {
		t.Fatal(err)
	}
	scheduler := NewAgentRunScheduler(store)
	scheduler.now = func() time.Time { return retryAt }
	claimed, err := scheduler.ClaimNext(t.Context(), AgentRunClaimRequest{Scope: scope, WorkerID: "worker-2"})
	if err != nil || claimed == nil {
		t.Fatalf("retry claim = %#v, %v", claimed, err)
	}
	second, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if host.calls != 2 || len(host.invocations) != 2 || host.invocations[0] != host.invocations[1] || host.reservations[0] != host.reservations[1] {
		t.Fatalf("host retry calls=%d invocations=%#v reservations=%#v", host.calls, host.invocations, host.reservations)
	}
	if second.Run.Status != AgentRunStatusCompleted || second.Run.BudgetUsage.Turns != 1 || second.Run.BudgetUsage.InputTokens != 100 || second.Run.BudgetUsage.OutputTokens != 20 || len(second.Run.BudgetReservations) != 0 {
		t.Fatalf("settled retry = %#v", second.Run)
	}
}
