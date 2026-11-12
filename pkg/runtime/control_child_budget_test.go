package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestControlMinimumChildTurnsIsEightSteps(t *testing.T) {
	minimum := controlMinimumChildBudget(models.BudgetPolicy{MaxTurns: 64, MaxTotalTokens: 512000, MaxActions: 64})
	if minimum.MaxTurns != 8 || minimum.MaxTotalTokens != controlMinimumChildTotalTokens || minimum.MaxActions != 1 {
		t.Fatalf("minimum child = %#v", minimum)
	}
	if minimum.MaxAttempts != 0 || minimum.MaxInputTokens != 0 || minimum.MaxOutputTokens != 0 || minimum.MaxCostMicros != 0 || minimum.MaxDurationMS != 0 {
		t.Fatalf("unbounded parent dimensions must stay unbounded: %#v", minimum)
	}
}

func TestControlBranchBudgetsUnboundedRunGivesNilBudgets(t *testing.T) {
	allocations, err := controlBranchBudgets(&models.AgentRun{ID: "run_1"}, "step_1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 3 {
		t.Fatalf("allocations = %#v", allocations)
	}
	for index, allocation := range allocations {
		if allocation != nil {
			t.Fatalf("branch %d budget = %#v, want unbounded", index, allocation)
		}
	}
}

func TestControlBranchBudgetsBoundedRunAllocatesMinimumChild(t *testing.T) {
	run := &models.AgentRun{ID: "run_1", Budget: &models.BudgetPolicy{MaxTurns: 64, MaxTotalTokens: 512000, MaxActions: 64}}
	allocations, err := controlBranchBudgets(run, "step_1", 2)
	if err != nil {
		t.Fatal(err)
	}
	for index, allocation := range allocations {
		if allocation == nil || allocation.MaxTurns != 8 || allocation.MaxTotalTokens != controlMinimumChildTotalTokens || allocation.MaxActions != 1 {
			t.Fatalf("branch %d budget = %#v", index, allocation)
		}
	}
	if allocations[0] == allocations[1] {
		t.Fatal("branches share one budget value")
	}
}

func TestControlBranchBudgetsInsufficientIsToolError(t *testing.T) {
	run := &models.AgentRun{
		ID: "run_1", Budget: &models.BudgetPolicy{MaxTurns: 20},
		BudgetUsage:       models.BudgetUsage{Turns: 3},
		BudgetAllocations: map[string]models.BudgetPolicy{"child_1": {MaxTurns: 8}},
	}
	// Remaining turns = 20 - 3 - 8 = 9: one child fits, two do not.
	if _, err := controlBranchBudgets(run, "step_1", 1); err != nil {
		t.Fatalf("one child: %v", err)
	}
	_, err := controlBranchBudgets(run, "step_1", 2)
	var insufficient *ControlBudgetInsufficientError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want ControlBudgetInsufficientError", err)
	}
	payload := insufficient.ToolError()
	if payload["error"] != "budget_insufficient" || payload["message"] != "This Run's remaining budget cannot fund 2 more child Runs." {
		t.Fatalf("tool error = %#v", payload)
	}
}

func TestControlRunBudgetExcludesCurrentStepReservation(t *testing.T) {
	now := time.Now()
	run := &models.AgentRun{
		ID: "run_1", Budget: &models.BudgetPolicy{MaxTurns: 20, MaxTotalTokens: 100000},
		BudgetUsage: models.BudgetUsage{Turns: 2, InputTokens: 1000, OutputTokens: 500},
		BudgetReservations: map[string]models.BudgetReservation{
			"step_1": {ID: "step_1", Usage: models.BudgetUsage{Turns: 1, InputTokens: 9000}, CreatedAt: now},
			"step_0": {ID: "step_0", Usage: models.BudgetUsage{Turns: 1, InputTokens: 4000}, CreatedAt: now},
		},
		BudgetAllocations: map[string]models.BudgetPolicy{"child_1": {MaxTurns: 8, MaxTotalTokens: 20000}},
	}
	budget, err := controlRunBudget(run, "step_1")
	if err != nil {
		t.Fatal(err)
	}
	if budget.TurnReservation.InputTokens != 9000 {
		t.Fatalf("current reservation = %#v", budget.TurnReservation)
	}
	if budget.Remaining.MaxTurns != 20-2-1-8 || budget.Remaining.MaxTotalTokens != 100000-1500-4000-20000 {
		t.Fatalf("remaining = %#v", budget.Remaining)
	}
	if budget.MinimumChild.MaxTurns != 8 {
		t.Fatalf("minimum child = %#v", budget.MinimumChild)
	}
	if unbounded, err := controlRunBudget(&models.AgentRun{ID: "run_2"}, "step_1"); err != nil || unbounded != nil {
		t.Fatalf("unbounded Run budget = %#v, %v", unbounded, err)
	}
}

func TestControlChildBudgetFloorAndCapacity(t *testing.T) {
	budget, err := controlRunBudget(&models.AgentRun{ID: "run_1", Budget: &models.BudgetPolicy{MaxTurns: 12}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := controlChildBudgetFloor(&models.BudgetPolicy{MaxTurns: 7}, budget); err == nil {
		t.Fatal("7 turns passed the 8-step floor")
	}
	if err := controlChildBudgetFloor(&models.BudgetPolicy{MaxTurns: 8}, budget); err != nil {
		t.Fatal(err)
	}
	if err := controlChildBudgetCapacity([]*models.BudgetPolicy{{MaxTurns: 8}, {MaxTurns: 4}}, budget); err != nil {
		t.Fatal(err)
	}
	if err := controlChildBudgetCapacity([]*models.BudgetPolicy{{MaxTurns: 8}, {MaxTurns: 5}}, budget); err == nil {
		t.Fatal("13 turns fit in 12")
	}
	if err := controlChildBudgetFloor(&models.BudgetPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestControlTaskBudgetInheritsRemainingBoundedDimensions(t *testing.T) {
	run := &models.AgentRun{ID: "run_1", Budget: &models.BudgetPolicy{MaxTurns: 64, MaxActions: 64}, BudgetUsage: models.BudgetUsage{Turns: 4, Actions: 10}}
	budget, err := controlRunBudget(run, "step_1")
	if err != nil {
		t.Fatal(err)
	}
	task := controlTaskBudget(budget)
	if task.MaxTurns != 60 || task.MaxActions != 54 || task.MaxTotalTokens != 0 || task.MaxAttempts != 0 {
		t.Fatalf("task budget = %#v", task)
	}
	if unbounded := controlTaskBudget(nil); unbounded == nil || *unbounded != (models.BudgetPolicy{}) {
		t.Fatalf("unbounded task budget = %#v", unbounded)
	}
}
