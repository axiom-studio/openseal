package runtime

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestEvaluateBudgetUsesWarningAndExhaustedStates(t *testing.T) {
	policy := BudgetPolicy{MaxAttempts: 4, MaxTurns: 10, MaxTotalTokens: 100, WarningPermille: 750}
	state, reasons, err := EvaluateBudget(policy, BudgetUsage{Attempts: 2, Turns: 7, InputTokens: 50, OutputTokens: 25})
	if err != nil || state != BudgetStateWarning || len(reasons) != 1 {
		t.Fatalf("warning evaluation = %q %#v %v", state, reasons, err)
	}
	state, reasons, err = EvaluateBudget(policy, BudgetUsage{Attempts: 4, Turns: 10, InputTokens: 50, OutputTokens: 50})
	if err != nil || state != BudgetStateExhausted || len(reasons) != 3 {
		t.Fatalf("exhausted evaluation = %q %#v %v", state, reasons, err)
	}
}

func TestBudgetRejectsInvalidPolicyAndUsage(t *testing.T) {
	if _, _, err := EvaluateBudget(BudgetPolicy{MaxTurns: -1}, BudgetUsage{}); err == nil || !strings.Contains(err.Error(), "maxTurns") {
		t.Fatalf("negative policy error = %v", err)
	}
	if _, _, err := EvaluateBudget(BudgetPolicy{}, BudgetUsage{CostMicros: -1}); err == nil {
		t.Fatal("negative usage was accepted")
	}
	if _, _, err := EvaluateBudget(BudgetPolicy{MaxAttempts: -1}, BudgetUsage{}); err == nil {
		t.Fatal("negative attempt policy was accepted")
	}
}

func TestBudgetJSONDistinguishesOmittedFromExplicitZeroLimits(t *testing.T) {
	var partial BudgetPolicy
	if err := json.Unmarshal([]byte(`{"maxAttempts":3,"maxTurns":3,"maxInputTokens":50000,"maxOutputTokens":10000,"maxTotalTokens":60000,"maxDurationMs":120000}`), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.MaxActions != 0 || partial.MaxCostMicros != 0 {
		t.Fatalf("omitted dimensions were not unbounded: %#v", partial)
	}
	for _, body := range []string{
		`{"maxActions":0}`,
		`{"maxCostMicros":0}`,
		`{"maxTurns":0}`,
	} {
		var policy BudgetPolicy
		err := json.Unmarshal([]byte(body), &policy)
		if err == nil || !strings.Contains(err.Error(), "must be positive when specified") || !strings.Contains(err.Error(), "omit it for an unbounded dimension") {
			t.Fatalf("explicit zero %s error = %v", body, err)
		}
	}
	var unknown BudgetPolicy
	if err := json.Unmarshal([]byte(`{"maxTurns":3,"maxUnknown":1}`), &unknown); err == nil || !strings.Contains(err.Error(), `unknown field "maxUnknown"`) {
		t.Fatalf("unknown budget field error = %v", err)
	}
}

func TestPartialBudgetAllowsFirstTurnAndUnboundedAction(t *testing.T) {
	run := &AgentRun{
		Budget: &BudgetPolicy{
			MaxAttempts: 3, MaxTurns: 3, MaxInputTokens: 50000, MaxOutputTokens: 10000,
			MaxTotalTokens: 60000, MaxDurationMS: 120000,
		},
		BudgetState: BudgetStateActive,
	}
	turn := BudgetReservation{ID: "turn-1", Usage: BudgetUsage{Turns: 1, InputTokens: 12000, OutputTokens: 1000, DurationMS: 500}, CreatedAt: time.Now()}
	if err := reserveRunBudget(run, turn); err != nil {
		t.Fatalf("reserve first turn: %v", err)
	}
	if err := settleRunBudgetReservation(run, turn.ID, turn.Usage); err != nil {
		t.Fatalf("settle first turn: %v", err)
	}
	action := BudgetReservation{ID: "action-1", Usage: BudgetUsage{Actions: 1}, CreatedAt: time.Now()}
	if err := reserveRunBudget(run, action); err != nil {
		t.Fatalf("reserve action on omitted unbounded maxActions: %v", err)
	}
	if run.BudgetState != BudgetStateActive {
		t.Fatalf("partial budget state = %s", run.BudgetState)
	}
}

func TestTurnUsageRejectsNonFiniteCost(t *testing.T) {
	for _, cost := range []float64{-1, math.Inf(1), math.NaN()} {
		if err := (TurnUsage{Cost: cost}).Validate(); err == nil {
			t.Fatalf("invalid cost %v was accepted", cost)
		}
	}
	for _, usage := range []TurnUsage{{ProviderDurationMS: -1}, {ValidationDurationMS: -1}, {RepairAttempts: -1}} {
		if err := usage.Validate(); err == nil {
			t.Fatalf("invalid phase usage %#v was accepted", usage)
		}
	}
}

func TestEffectiveBudgetUsageIncludesReservations(t *testing.T) {
	committed := BudgetUsage{Turns: 2, InputTokens: 20}
	effective, err := EffectiveBudgetUsage(committed, map[string]BudgetReservation{
		"turn-3": {ID: "turn-3", Usage: BudgetUsage{Turns: 1, InputTokens: 10}},
	})
	if err != nil || effective.Turns != 3 || effective.InputTokens != 30 {
		t.Fatalf("effective usage = %#v, %v", effective, err)
	}
	exceeded, reasons, err := BudgetWouldExceed(BudgetPolicy{MaxTurns: 2}, effective)
	if err != nil || !exceeded || len(reasons) != 1 {
		t.Fatalf("exceeded = %t %#v %v", exceeded, reasons, err)
	}
}
