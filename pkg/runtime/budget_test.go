package runtime

import (
	"math"
	"testing"
)

func TestEvaluateBudgetUsesWarningAndExhaustedStates(t *testing.T) {
	policy := BudgetPolicy{MaxTurns: 10, MaxTotalTokens: 100, WarningPermille: 750}
	state, reasons, err := EvaluateBudget(policy, BudgetUsage{Turns: 7, InputTokens: 50, OutputTokens: 25})
	if err != nil || state != BudgetStateWarning || len(reasons) != 1 {
		t.Fatalf("warning evaluation = %q %#v %v", state, reasons, err)
	}
	state, reasons, err = EvaluateBudget(policy, BudgetUsage{Turns: 10, InputTokens: 50, OutputTokens: 50})
	if err != nil || state != BudgetStateExhausted || len(reasons) != 2 {
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
}

func TestTurnUsageRejectsNonFiniteCost(t *testing.T) {
	for _, cost := range []float64{-1, math.Inf(1), math.NaN()} {
		if err := (TurnUsage{Cost: cost}).Validate(); err == nil {
			t.Fatalf("invalid cost %v was accepted", cost)
		}
	}
}
