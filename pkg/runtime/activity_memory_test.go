package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestMemoryActivitySequencesConcurrentAppends(t *testing.T) {
	store := NewMemoryStore()
	portfolio := NewPortfolioService(store)
	activity := NewRunActivityService(store, store)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "work", Source: RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}

	const count = 25
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, appendErr := activity.AppendActivity(ctx, &ActivityEvent{
				Scope: scope, RunID: run.ID, EventType: "game.move", Summary: fmt.Sprintf("move %d", i),
			})
			if appendErr != nil {
				t.Errorf("append: %v", appendErr)
			}
		}(i)
	}
	wg.Wait()
	events, err := activity.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, Limit: count})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("event count = %d, want %d", len(events), count)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event sequence[%d] = %d", i, event.Sequence)
		}
	}
}

func TestMemoryRunTransitionPersistsWakeAndTerminalState(t *testing.T) {
	store := NewMemoryStore()
	portfolio := NewPortfolioService(store)
	activity := NewRunActivityService(store, store)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "work", Source: RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	wake := &WakeCondition{Type: "event", Reference: "game.turn"}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForEvent, WakeCondition: wake,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := portfolio.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WakeCondition == nil || loaded.WakeCondition.Reference != wake.Reference {
		t.Fatalf("wake condition not persisted: %#v", loaded)
	}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if run.CompletedAt == nil || run.WakeCondition != nil {
		t.Fatalf("terminal state not persisted: %#v", run)
	}
}
