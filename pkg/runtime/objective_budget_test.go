package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestObjectiveBudgetAllocatesConcurrentTopLevelRunsAtomically(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "objective-budget.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "objective-budget"}
			portfolio := NewPortfolioService(store)
			objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Research", Goal: "Track the market",
				Status: ObjectiveStatusActive, Budget: &BudgetPolicy{MaxTurns: 10, MaxTotalTokens: 1000},
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: "researcher-one",
				Goal: "Monitor forums", Budget: &BudgetPolicy{MaxTurns: 6, MaxTotalTokens: 600}, IdempotencyKey: "forums",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: "researcher-two",
				Goal: "Monitor reviews", Budget: &BudgetPolicy{MaxTurns: 5, MaxTotalTokens: 300},
			})
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("over-allocation error = %v", err)
			}
			second, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: "researcher-two",
				Goal: "Monitor reviews", Budget: &BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 400},
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := portfolio.GetObjective(ctx, scope, objective.ID)
			if err != nil || len(loaded.BudgetAllocations) != 2 || loaded.BudgetAllocations[first.Run.ID].MaxTurns != 6 || loaded.BudgetAllocations[second.ID].MaxTurns != 4 {
				t.Fatalf("objective allocations = %#v, %v", loaded, err)
			}
			child, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, ParentRunID: first.Run.ID, Owner: objective.Owner,
				AssignedAgentID: "research-helper", Goal: "Analyze one source", Budget: &BudgetPolicy{MaxTurns: 2, MaxTotalTokens: 200},
			})
			if err != nil || child.ParentRunID != first.Run.ID {
				t.Fatalf("child run = %#v, %v", child, err)
			}
			loaded, err = portfolio.GetObjective(ctx, scope, objective.ID)
			if err != nil || len(loaded.BudgetAllocations) != 2 {
				t.Fatalf("child double-allocated objective = %#v, %v", loaded, err)
			}
		})
	}
}

func TestObjectiveBudgetRejectsConcurrentOverAllocation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "objective-budget-race.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "objective-budget-race"}
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Bounded", Goal: "Run concurrently",
				Budget: &BudgetPolicy{MaxTurns: 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			errorsByWorker := make(chan error, 2)
			var wg sync.WaitGroup
			for index := 0; index < 2; index++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					<-start
					_, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
						Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: objective.Owner.ID,
						Goal: "Concurrent work", Budget: &BudgetPolicy{MaxTurns: 6}, IdempotencyKey: string(rune('a' + index)),
					})
					errorsByWorker <- err
				}(index)
			}
			close(start)
			wg.Wait()
			close(errorsByWorker)
			succeeded, exhausted := 0, 0
			for err := range errorsByWorker {
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, ErrBudgetExhausted):
					exhausted++
				default:
					t.Fatalf("unexpected concurrent error: %v", err)
				}
			}
			loaded, err := NewPortfolioService(store).GetObjective(ctx, scope, objective.ID)
			if err != nil || succeeded != 1 || exhausted != 1 || len(loaded.BudgetAllocations) != 1 {
				t.Fatalf("concurrent outcome success=%d exhausted=%d objective=%#v err=%v", succeeded, exhausted, loaded, err)
			}
		})
	}
}
