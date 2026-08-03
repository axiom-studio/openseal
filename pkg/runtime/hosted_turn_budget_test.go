package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHostedTurnModelInputCompactsHistoricalActionEvidenceWithoutMutatingCheckpoint(t *testing.T) {
	largeEvidence := strings.Repeat("semantic browser snapshot ", 400)
	checkpoint := map[string]interface{}{
		"_opensealActionHistory": []interface{}{
			map[string]interface{}{"actionCallId": "older", "status": "succeeded", "result": map[string]interface{}{"url": "https://example.test/rules", "text": largeEvidence, "elements": []interface{}{largeEvidence}}},
			map[string]interface{}{"actionCallId": "latest", "status": "succeeded", "result": map[string]interface{}{"snapshot": largeEvidence}},
		},
		"lastAction": map[string]interface{}{"actionCallId": "latest", "status": "succeeded", "result": map[string]interface{}{"snapshot": largeEvidence}},
	}
	input, err := MarshalHostedTurnModelInput(HostedTurnRequest{Goal: "Continue", ContinuationCheckpoint: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(input) >= len(largeEvidence)*2 {
		t.Fatalf("model input retained duplicated historical evidence: %d bytes", len(input))
	}
	var projected HostedTurnModelInput
	if err := json.Unmarshal(input, &projected); err != nil {
		t.Fatal(err)
	}
	history := projected.ContinuationCheckpoint[actionHistoryCheckpointKey].([]interface{})
	for index, value := range history {
		result := value.(map[string]interface{})["result"].(map[string]interface{})
		if result["compacted"] != true || !strings.HasPrefix(result["evidenceRef"].(string), "action-call:") {
			t.Fatalf("projected historical result = %#v", result)
		}
		if index == 0 && (result["url"] != "https://example.test/rules" || !strings.Contains(result["text"].(string), "semantic browser snapshot") || result["elements"] != nil) {
			t.Fatalf("concise historical findings were not retained: %#v", result)
		}
	}
	latest := projected.ContinuationCheckpoint["lastAction"].(map[string]interface{})["result"].(map[string]interface{})
	if latest["snapshot"] != largeEvidence {
		t.Fatal("latest action evidence was not preserved for the model")
	}
	original := checkpoint[actionHistoryCheckpointKey].([]interface{})[0].(map[string]interface{})["result"].(map[string]interface{})
	if original["text"] != largeEvidence || original["compacted"] != nil || len(original["elements"].([]interface{})) != 1 {
		t.Fatalf("durable checkpoint was mutated: %#v", checkpoint)
	}
}

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

type reservationBoundaryHostedTurnHost struct{ over int64 }

func (h reservationBoundaryHostedTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
		ModelProvider: "test", Model: "test-model", NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "done", Usage: TurnUsage{
			InputTokens: int(request.Budget.TurnReservation.InputTokens + h.over), OutputTokens: 1,
		},
	}, nil
}

type reportedOutputHostedTurnHost struct{ outputTokens int }

func (h reportedOutputHostedTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
		ModelProvider: "test", Model: "reasoning-model", NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "done", Usage: TurnUsage{InputTokens: 100, OutputTokens: h.outputTokens},
	}, nil
}

func TestHostedTurnReasoningUsageSettlesAgainstRemainingRunBudget(t *testing.T) {
	const turnID = "turn-reasoning-usage"
	run := &AgentRun{
		ID: "run-reasoning-usage", Scope: Scope{Kind: "tenant", ID: "7"}, AssignedAgentID: "agent", Goal: "Do bounded work",
		Budget: &BudgetPolicy{MaxInputTokens: 100000, MaxOutputTokens: 250000, MaxTotalTokens: 350000},
	}
	runner, err := NewHostedTurnRunner(reportedOutputHostedTurnHost{outputTokens: 33550}, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn := &AgentTurn{ID: turnID, RunID: run.ID}
	reservation, err := runner.PlanTurnBudget(t.Context(), TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.OutputTokens != run.Budget.MaxOutputTokens {
		t.Fatalf("output reservation = %d, want %d", reservation.OutputTokens, run.Budget.MaxOutputTokens)
	}
	run.BudgetReservations = map[string]BudgetReservation{turnID: {
		ID: turnID, Usage: reservation, CreatedAt: time.Now(),
	}}
	if _, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: turn}); err != nil {
		t.Fatalf("reasoning usage inside the Run budget was rejected: %v", err)
	}
}

func TestHostedTurnProviderUsageCannotExceedConservativeReservation(t *testing.T) {
	const turnID = "turn-reservation-boundary"
	run := &AgentRun{
		ID: "run-reservation-boundary", Scope: Scope{Kind: "tenant", ID: "7"}, AssignedAgentID: "agent", Goal: "Do bounded work",
		Budget: &BudgetPolicy{MaxInputTokens: 32000, MaxOutputTokens: 4000, MaxTotalTokens: 36000},
		BudgetReservations: map[string]BudgetReservation{turnID: {
			ID: turnID, Usage: BudgetUsage{Turns: 1, InputTokens: 12000, OutputTokens: 1000}, CreatedAt: time.Now(),
		}},
	}
	for _, test := range []struct {
		name       string
		over       int64
		inputLimit int64
		wantErr    bool
	}{
		{name: "at reservation", over: 0},
		{name: "bounded provider envelope drift", over: 89},
		{name: "bounded drift beyond user budget", over: 89, inputLimit: 12050, wantErr: true},
		{name: "estimate drift inside Run budget", over: 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			testRun := *run
			testBudget := *run.Budget
			if test.inputLimit > 0 {
				testBudget.MaxInputTokens = test.inputLimit
			}
			testRun.Budget = &testBudget
			runner, err := NewHostedTurnRunner(reservationBoundaryHostedTurnHost{over: test.over}, HostedTurnRunnerConfig{
				AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: &testRun, Turn: &AgentTurn{ID: turnID}})
			if test.wantErr != (err != nil) || (test.wantErr && !strings.Contains(err.Error(), "beyond the remaining Run budget")) {
				t.Fatalf("error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
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
	if result.Run.BudgetState != BudgetStateExhausted || result.Run.BudgetAdmission == nil ||
		result.Run.BudgetAdmission.Dimension != "total_tokens" || result.Run.BudgetAdmission.Required <= result.Run.BudgetAdmission.Remaining {
		t.Fatalf("rejected preflight admission state = %#v", result.Run)
	}
	if result.Event == nil || result.Event.Payload["admission"] == nil {
		t.Fatalf("rejected preflight did not expose reservation math: %#v", result.Event)
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

func TestHostedTurnBudgetReservesInitialAndRepairProviderInputs(t *testing.T) {
	runner, err := NewHostedTurnRunner(&countedHostedTurnHost{}, HostedTurnRunnerConfig{
		AgentID: "browser-agent", DefinitionID: "browser-agent", DefinitionVersion: "1",
		Actions: []capability.ModelAction{{Name: "browser.snapshot", Description: "Capture the current semantic page snapshot."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{
		ID: "browser-run", Scope: Scope{Kind: "tenant", ID: "7"}, AssignedAgentID: "browser-agent",
		Goal: "Inspect the page and continue", Checkpoint: map[string]interface{}{
			"lastAction": map[string]interface{}{
				"actionCallId": "snapshot-1", "status": "succeeded", "action": "browser.snapshot",
				"result": map[string]interface{}{"snapshot": strings.Repeat("semantic browser evidence ", 1200)},
			},
		},
		Budget: &BudgetPolicy{MaxInputTokens: 200000, MaxOutputTokens: 20000, MaxTotalTokens: 220000},
	}
	turn := &AgentTurn{ID: "turn-4"}
	oneRequest, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	oneAttempt, err := EstimateHostedTurnInputTokens(oneRequest)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := runner.PlanTurnBudget(t.Context(), TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	want := oneAttempt*HostedTurnMaximumProviderAttempts + HostedTurnRepairInputReserveTokens
	if reservation.InputTokens != want || reservation.InputTokens <= oneAttempt {
		t.Fatalf("input reservation=%d, one attempt=%d, want bounded attempts=%d", reservation.InputTokens, oneAttempt, want)
	}
}

func TestHostedBrowserNextTurnPersistsInadmissibleBudgetState(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "browser-multi-turn"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser-agent"}, AssignedAgentID: "browser-agent",
		Goal: "Continue a bounded Browser run", Budget: &BudgetPolicy{MaxTurns: 3, MaxTotalTokens: 12000},
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := BudgetUsage{Turns: 1, InputTokens: 10000, OutputTokens: 1000}
	run, _, err = NewRunActivityService(store, store).TransitionRun(t.Context(), scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "Settled first Browser turn",
		BudgetUsageDelta: &prior,
	})
	if err != nil || run.BudgetState == BudgetStateExhausted {
		t.Fatalf("prior turn state = %#v, err=%v", run, err)
	}
	host := &countedHostedTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "browser-agent", DefinitionID: "browser-agent", DefinitionVersion: "1",
		SkillPrompts: []HostedSkillPrompt{{
			SkillID: "browser", Version: "1", Name: "Browser",
			Instructions: "Inspect the current page and continue through one governed browser action at a time.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "browser-worker",
	}, runner)
	if !errors.Is(err, ErrBudgetExhausted) || result == nil || host.calls != 0 {
		t.Fatalf("next Browser turn result=%#v err=%v calls=%d", result, err, host.calls)
	}
	if result.Run.Status != AgentRunStatusPaused || result.Run.BudgetState != BudgetStateExhausted || result.Run.BudgetAdmission == nil {
		t.Fatalf("next Browser admission was not persisted: %#v", result.Run)
	}
	admission := result.Run.BudgetAdmission
	if admission.Dimension != "total_tokens" || admission.Required <= admission.Remaining || admission.Reservation.InputTokens == 0 || admission.Reservation.OutputTokens != HostedTurnMinimumOutputTokens {
		t.Fatalf("next Browser reservation math = %#v", admission)
	}
	loaded, err := NewPortfolioService(store).GetAgentRun(t.Context(), scope, run.ID)
	if err != nil || loaded.BudgetState != BudgetStateExhausted || loaded.BudgetAdmission == nil || loaded.BudgetAdmission.TurnID != result.Turn.ID {
		t.Fatalf("persisted next-turn admission = %#v, err=%v", loaded, err)
	}
}

func TestHostedTurnBudgetReservationCapsProviderOutputAndSettlesActualUsage(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{
			MaxAttempts: 3, MaxTurns: 3, MaxInputTokens: 32000, MaxOutputTokens: 2000,
			MaxTotalTokens: 34000, MaxDurationMS: 120000,
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
	if host.request.Budget.Remaining.MaxTotalTokens != 34000 || host.request.Budget.Remaining.MaxOutputTokens != 2000 {
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

func TestHostedTurnBudgetReservationUsesRemainingRunOutputBudget(t *testing.T) {
	runner, err := NewHostedTurnRunner(&countedHostedTurnHost{}, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run", AssignedAgentID: "agent", Goal: "Do work", Budget: &BudgetPolicy{
		MaxInputTokens: 512000, MaxOutputTokens: 512000, MaxTotalTokens: 1100000,
	}}
	turn := &AgentTurn{ID: "turn", RunID: run.ID}
	reservation, err := runner.PlanTurnBudget(t.Context(), TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.OutputTokens != run.Budget.MaxOutputTokens {
		t.Fatalf("output reservation = %d, want remaining Run output budget %d", reservation.OutputTokens, run.Budget.MaxOutputTokens)
	}
}

func TestHostedTurnMinimumChildBudgetCoversAuthorizedEnvelope(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-large-context",
		NextRunStatus: AgentRunStatusCompleted, ModelProvider: "test", Model: "test-model",
		SkillSelections: []HostedSkillSelection{{
			SkillRef: "skill:large-context@1", Disposition: HostedSkillApplied, Summary: "Applied authorized context",
		}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
		SkillPrompts: []HostedSkillPrompt{{
			SkillID: "large-context", Version: "1", Name: "Large context",
			Instructions: strings.Repeat("bounded authorized instructions ", 900),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{
		ID: "run-large-context", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Complete bounded work",
		Budget: &BudgetPolicy{
			MaxAttempts: 5, MaxTurns: 5, MaxInputTokens: 100000, MaxOutputTokens: 20000,
			MaxTotalTokens: 120000, MaxDurationMS: 600000, MaxActions: 2,
		},
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: run, Turn: &AgentTurn{ID: "turn-large-context"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if host.request.Budget.MinimumChild.MaxInputTokens <= HostedTurnMinimumChildInputTokens {
		t.Fatalf("large authorized envelope kept static child floor: %#v", host.request.Budget.MinimumChild)
	}
	estimated, err := EstimateHostedTurnInputTokens(host.request)
	if err != nil {
		t.Fatal(err)
	}
	if host.request.Budget.MinimumChild.MaxInputTokens < estimated ||
		host.request.Budget.MinimumChild.MaxTotalTokens < host.request.Budget.MinimumChild.MaxInputTokens+HostedTurnMinimumChildOutputTokens {
		t.Fatalf("minimum child budget %#v does not cover hosted estimate %d", host.request.Budget.MinimumChild, estimated)
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

func TestHostedTurnInputEstimateKeepsTextualManagementEnvelopeWithinCanonicalMinimum(t *testing.T) {
	request := HostedTurnRequest{
		Goal:               "Answer a concise question",
		SystemInstructions: []string{strings.Repeat("bounded textual management schema ", 500)},
	}
	encoded, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	estimated, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	want := HostedTurnProtocolInputReserveTokens + HostedTurnBudgetEnvelopeReserveTokens + (int64(len(encoded))+1)/2
	const canonicalHostedInputMinimum = int64(16000)
	if estimated != want || estimated >= canonicalHostedInputMinimum {
		t.Fatalf("textual envelope bytes=%d estimate=%d want=%d minimum=%d", len(encoded), estimated, want, canonicalHostedInputMinimum)
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
	estimate, err := EstimateHostedTurnMaximumInputTokens(request)
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
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 32000, MaxOutputTokens: 2000, MaxTotalTokens: 34000},
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
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 32000, MaxOutputTokens: 2000, MaxTotalTokens: 34000},
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
	if host.calls != 2 || len(host.invocations) != 2 || host.invocations[0] != host.invocations[1] ||
		host.reservations[1].InputTokens < host.reservations[0].InputTokens {
		t.Fatalf("host retry calls=%d invocations=%#v reservations=%#v", host.calls, host.invocations, host.reservations)
	}
	if second.Run.Status != AgentRunStatusCompleted || second.Run.BudgetUsage.Turns != 1 || second.Run.BudgetUsage.InputTokens != 100 || second.Run.BudgetUsage.OutputTokens != 20 || len(second.Run.BudgetReservations) != 0 {
		t.Fatalf("settled retry = %#v", second.Run)
	}
}

func TestHostedTurnBudgetRetryReconcilesReservationAfterIntervention(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "retry-budget-intervention"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Do bounded work", Budget: &BudgetPolicy{MaxInputTokens: 32000, MaxOutputTokens: 2000, MaxTotalTokens: 34000},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &retryingHostedTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	first, err := coordinator.Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-1",
	}, runner)
	if !errors.Is(err, ErrTurnHostUnavailable) || first == nil || len(first.Run.BudgetReservations) != 1 {
		t.Fatalf("retry scheduling = %#v, %v", first, err)
	}
	intervened, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{
		Scope: scope, RunID: run.ID, ExpectedRevision: first.Run.Revision, Kind: AgentRunCommandIntervene,
		Actor:       ActivityActor{Type: "user", ID: "operator"},
		Instruction: "Use the latest durable evidence and continue with the authorized bounded operation.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if intervened.Run.Status != AgentRunStatusQueued || intervened.Run.WakeCondition != nil {
		t.Fatalf("intervention did not wake retrying run: %#v", intervened.Run)
	}
	scheduler := NewAgentRunScheduler(store)
	claimed, err := scheduler.ClaimNext(t.Context(), AgentRunClaimRequest{Scope: scope, WorkerID: "worker-2"})
	if err != nil || claimed == nil {
		t.Fatalf("retry claim = %#v, %v", claimed, err)
	}
	second, err := coordinator.Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-2",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if host.calls != 2 || len(host.reservations) != 2 || host.reservations[1].InputTokens <= host.reservations[0].InputTokens {
		t.Fatalf("reservation was not reconciled after intervention: %#v", host.reservations)
	}
	if second.Run.Status != AgentRunStatusCompleted || second.Run.BudgetUsage.Turns != 1 || len(second.Run.BudgetReservations) != 0 {
		t.Fatalf("settled retry = %#v", second.Run)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{"budget.reservation_reconciled"}})
	if err != nil || len(events) != 1 {
		t.Fatalf("reservation reconciliation events = %#v, %v", events, err)
	}
}
