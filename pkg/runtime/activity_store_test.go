package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestActivitySequencesConcurrentAppends(t *testing.T) {
	store := kerneltest.NewStore(t)
	portfolio := portfolio.NewPortfolioService(store)
	activity := runtimeactivity.NewRunActivityService(store, store)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "work", Source: models.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}

	const count = 25
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, appendErr := activity.AppendActivity(ctx, &models.ActivityEvent{
				Scope: scope, RunID: run.ID, EventType: "game.move", Summary: fmt.Sprintf("move %d", i),
			})
			if appendErr != nil {
				t.Errorf("append: %v", appendErr)
			}
		}(i)
	}
	wg.Wait()
	events, err := activity.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: run.ID, Limit: count})
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

func TestRunTransitionPersistsWakeAndTerminalState(t *testing.T) {
	store := kerneltest.NewStore(t)
	portfolio := portfolio.NewPortfolioService(store)
	activity := runtimeactivity.NewRunActivityService(store, store)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "work", Source: models.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: 1, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	wake := &models.WakeCondition{Type: "event", Reference: "game.turn"}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusWaitingForEvent, WakeCondition: wake,
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
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if run.CompletedAt == nil || run.WakeCondition != nil {
		t.Fatalf("terminal state not persisted: %#v", run)
	}
}
