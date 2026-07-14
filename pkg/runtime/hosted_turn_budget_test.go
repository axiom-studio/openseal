package runtime

import (
	"context"
	"errors"
	"testing"
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
	store := NewMemoryStore(100)
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
}

func TestHostedTurnBudgetReservationCapsProviderOutputAndSettlesActualUsage(t *testing.T) {
	store := NewMemoryStore(100)
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
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
	if result.Run.Status != AgentRunStatusCompleted || result.Run.BudgetUsage.Turns != 1 || result.Run.BudgetUsage.InputTokens != 100 || result.Run.BudgetUsage.OutputTokens != 20 || len(result.Run.BudgetReservations) != 0 {
		t.Fatalf("settled run=%#v", result.Run)
	}
}
