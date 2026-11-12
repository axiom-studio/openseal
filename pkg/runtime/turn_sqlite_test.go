package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestSQLiteMigratesAgentTurnLeases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-turns.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agent_turns (
		id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL,
		sequence INTEGER NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL,
		created_at DATETIME NOT NULL, payload TEXT NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id), UNIQUE (scope_kind, scope_id, run_id, sequence));`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := kerneltest.OpenSQLiteDB(t, path)
	defer store.Close()
	rows, err := sqlDB.Query(`PRAGMA table_info(agent_turns)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["lease_owner"] || !columns["lease_expires_at"] {
		t.Fatalf("lease columns were not migrated: %#v", columns)
	}
}

func TestSQLiteAgentTurnsEnforceOneActiveAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	run, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	turns := activity.NewAgentTurnService(store, store)
	const contenders = 12
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, beginErr := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{
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
		} else if !errors.Is(err, models.ErrActiveTurnExists) {
			t.Fatalf("unexpected begin error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent turn starts = %d, want 1", successes)
	}
	listed, err := turns.ListTurns(ctx, models.AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("unexpected turns: %#v", listed)
	}
	finished, err := turns.FinishTurn(ctx, scope, listed[0].ID, activity.FinishAgentTurnRequest{
		ExpectedRevision: 1, Status: models.AgentTurnStatusCompleted, WorkerID: listed[0].LeaseOwner, OutputSummary: "Turn complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: run.ID, Model: "test-model", WorkerID: "worker-next"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second turn sequence = %d, want 2", second.Sequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := activity.NewAgentTurnService(reopened, reopened).GetTurn(ctx, scope, finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 2 || loaded.CompletedAt == nil || loaded.OutputSummary != "Turn complete" {
		t.Fatalf("finished turn did not survive restart: %#v", loaded)
	}
	if _, err := activity.NewAgentTurnService(reopened, reopened).GetTurn(ctx, models.Scope{Kind: "local", ID: "other"}, finished.ID); !errors.Is(err, models.ErrTurnNotFound) {
		t.Fatalf("cross-scope turn read error = %v", err)
	}
}
