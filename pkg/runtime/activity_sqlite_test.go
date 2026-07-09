package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteActivityIsOrderedAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
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
	activity := NewRunActivityService(store, store)
	const count = 25
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, appendErr := activity.AppendActivity(ctx, &ActivityEvent{
				Scope: scope, RunID: run.ID, EventType: "work.progress", Summary: fmt.Sprintf("step %d", i),
			})
			errs <- appendErr
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append activity: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := NewRunActivityService(reopened, reopened).ListActivity(ctx, ActivityFilter{
		Scope: scope, RunID: run.ID, Limit: count,
	})
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
	other, err := NewRunActivityService(reopened, reopened).ListActivity(ctx, ActivityFilter{
		Scope: Scope{Kind: "local", ID: "other"}, RunID: run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-scope activity leaked: %#v", other)
	}
}

func TestSQLiteMigratesAgentRunRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-portfolio.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agent_runs (
		id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
		objective_id TEXT NOT NULL DEFAULT '', parent_run_id TEXT NOT NULL DEFAULT '',
		root_run_id TEXT NOT NULL, assigned_agent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
		priority INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL, payload TEXT NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id));`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	run := &AgentRun{
		ID: "legacy-run", Scope: Scope{Kind: "local", ID: "test"}, RootRunID: "legacy-run",
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "continue work",
		Source: RunSourceManual, Status: AgentRunStatusQueued, CreatedAt: now, UpdatedAt: now,
	}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO agent_runs
		(id, scope_kind, scope_id, root_run_id, status, created_at, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Scope.Kind, run.Scope.ID, run.RootRunID, run.Status, run.CreatedAt, string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := NewPortfolioService(store).GetAgentRun(context.Background(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 {
		t.Fatalf("migrated revision = %d, want 1", loaded.Revision)
	}
	transitioned, event, err := NewRunActivityService(store, store).TransitionRun(
		context.Background(), run.Scope, run.ID,
		RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusRunning},
	)
	if err != nil {
		t.Fatal(err)
	}
	if transitioned.Revision != 2 || event.Sequence != 1 {
		t.Fatalf("post-migration transition = revision %d, sequence %d", transitioned.Revision, event.Sequence)
	}
}

func TestSQLiteRunTransitionIsAtomicAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transition.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	portfolio := NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	run, event, err := activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "Started work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != 2 || event.Sequence != 1 {
		t.Fatalf("transition = run revision %d, event sequence %d", run.Revision, event.Sequence)
	}
	wake := &WakeCondition{Type: "event", Reference: "dependency.ready"}
	run, event, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForDependency, WakeCondition: wake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != 3 || event.Sequence != 2 {
		t.Fatalf("waiting transition = run revision %d, event sequence %d", run.Revision, event.Sequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := NewPortfolioService(reopened).GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 3 || loaded.Status != AgentRunStatusWaitingForDependency ||
		loaded.WakeCondition == nil || loaded.WakeCondition.Reference != wake.Reference {
		t.Fatalf("run transition did not survive restart: %#v", loaded)
	}
	events, err := NewRunActivityService(reopened, reopened).ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Summary == "" {
		t.Fatalf("transition activity did not survive restart: %#v", events)
	}
}
