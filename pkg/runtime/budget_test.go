package runtime

import (
	"math"
	"testing"
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
	if _, _, err := EvaluateBudget(BudgetPolicy{MaxTurns: -1}, BudgetUsage{}); err == nil {
		t.Fatal("negative policy was accepted")
	}
	if _, _, err := EvaluateBudget(BudgetPolicy{}, BudgetUsage{CostMicros: -1}); err == nil {
		t.Fatal("negative usage was accepted")
	}
	if _, _, err := EvaluateBudget(BudgetPolicy{MaxAttempts: -1}, BudgetUsage{}); err == nil {
		t.Fatal("negative attempt policy was accepted")
	}
}

func TestTurnUsageRejectsNonFiniteCost(t *testing.T) {
	for _, cost := range []float64{-1, math.Inf(1), math.NaN()} {
		if err := (TurnUsage{Cost: cost}).Validate(); err == nil {
			t.Fatalf("invalid cost %v was accepted", cost)
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
