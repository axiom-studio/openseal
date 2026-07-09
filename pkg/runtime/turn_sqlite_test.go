package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestSQLiteAgentTurnsEnforceOneActiveAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	turns := NewAgentTurnService(store, store)
	const contenders = 12
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, beginErr := turns.BeginTurn(ctx, BeginAgentTurnRequest{
				Scope: scope, RunID: run.ID, Model: "test-model", WorkerID: fmt.Sprintf("worker-%d", i),
			})
			errs <- beginErr
		}(i)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrActiveTurnExists) {
			t.Fatalf("unexpected begin error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent turn starts = %d, want 1", successes)
	}
	listed, err := turns.ListTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("unexpected turns: %#v", listed)
	}
	finished, err := turns.FinishTurn(ctx, scope, listed[0].ID, FinishAgentTurnRequest{
		ExpectedRevision: 1, Status: AgentTurnStatusCompleted, WorkerID: listed[0].LeaseOwner, OutputSummary: "Turn complete",
		ContinuationCheckpoint: map[string]interface{}{"next": "inspect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := turns.BeginTurn(ctx, BeginAgentTurnRequest{Scope: scope, RunID: run.ID, Model: "test-model", WorkerID: "worker-next"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second turn sequence = %d, want 2", second.Sequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := NewAgentTurnService(reopened, reopened).GetTurn(ctx, scope, finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 2 || loaded.CompletedAt == nil || loaded.ContinuationCheckpoint["next"] != "inspect" {
		t.Fatalf("finished turn did not survive restart: %#v", loaded)
	}
	if _, err := NewAgentTurnService(reopened, reopened).GetTurn(ctx, Scope{Kind: "local", ID: "other"}, finished.ID); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("cross-scope turn read error = %v", err)
	}
}
