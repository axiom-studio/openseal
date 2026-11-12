//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// Postgres store test: it moves to store/postgres in U2.8. It runs the
// session conformance suite (agent_session_conformance_test.go, for
// storetest) and the entry helpers of agent_session_test.go.

// newAgentSessionPostgresStores opens n PostgresStores (replicas) on one fresh
// schema. The schema is dropped when the test ends.
func newAgentSessionPostgresStores(t *testing.T, n int) []*PostgresStore {
	t.Helper()
	pgSchema := newTestPostgresSchema(t, "agent_session_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	stores := make([]*PostgresStore, n)
	for i := range stores {
		store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
	}
	t.Cleanup(func() {
		_, _ = stores[0].db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+stores[0].quotedSchema()+` CASCADE`)
		for _, store := range stores {
			_ = store.Close()
		}
	})
	return stores
}

// TestPostgresAgentSessionStore runs the session store conformance suite
// (agent_session_conformance_test.go) against the Postgres backend.
func TestPostgresAgentSessionStore(t *testing.T) {
	runAgentSessionStoreConformance(t, func(t *testing.T) (agentSessionConformanceStore, createRunSessionInOwnTx) {
		store := newAgentSessionPostgresStores(t, 1)[0]
		return store, func(ctx context.Context, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
			return createPostgresRunSessionInOwnTx(ctx, store, req)
		}
	})
}

// createPostgresRunSessionInOwnTx runs the store's in-transaction
// createRunAgentSessionTx helper in a transaction of its own.
func createPostgresRunSessionInOwnTx(ctx context.Context, s *PostgresStore, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := s.createRunAgentSessionTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// sessionReplicaFixture is one conversation Run with an open session, served
// by two replicas of one database.
type sessionReplicaFixture struct {
	*sessionConformance
	replicas []*PostgresStore
	leased   *models.AgentRun
	lane     *models.AgentSession
}

func newSessionReplicaFixture(t *testing.T) *sessionReplicaFixture {
	t.Helper()
	replicas := newAgentSessionPostgresStores(t, 2)
	f := &sessionConformance{
		t:     t,
		ctx:   context.Background(),
		store: replicas[0],
		scope: models.Scope{Kind: "tenant", ID: "session-" + uuid.NewString()[:8]},
		now:   time.Now().UTC().Truncate(time.Millisecond),
	}
	run, session := f.conversationRun("agent-a")
	return &sessionReplicaFixture{sessionConformance: f, replicas: replicas, leased: run, lane: session}
}

// A replica whose lease expired and was taken over by another replica cannot
// append to the session or settle the Run's tool entries; nothing it sends is
// written. The new holder's writes go through.
func TestPostgresAgentSessionStaleReplicaAppendRefused(t *testing.T) {
	f := newSessionReplicaFixture(t)
	stale, fresh := f.replicas[0], f.replicas[1]
	f.mustAppend(f.lane.ID, f.leased.ID, testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", models.ToolEntryPending))

	// The stale worker's lease runs out; the other replica reclaims the Run.
	later := f.now.Add(2 * time.Hour)
	claimed, err := fresh.ClaimNextAgentRun(f.ctx, models.AgentRunClaim{
		Scope: f.scope, WorkerID: "fresh-worker", Now: later, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != f.leased.ID || claimed.LeaseOwner != "fresh-worker" {
		t.Fatalf("reclaim = %#v, %v", claimed, err)
	}

	// The stale worker still believes its lease is live (its clock says f.now).
	staleLease := &models.AgentRunLeaseGuard{WorkerID: conformanceWorker, Now: f.now}
	_, err = stale.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: f.lane.ID, RunID: f.leased.ID, Lease: staleLease,
		Entries: []*models.SessionEntry{testUserEntry("u-stale", "late")}, Events: []*models.ActivityEvent{f.event(f.leased.ID, "stale")}, Now: f.now,
	})
	expectErr(t, "stale append", err, models.ErrLeaseLost)
	f.assertNoEntry("u-stale")
	_, err = stale.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: f.scope, EntryID: "e1", RunID: f.leased.ID, Lease: *staleLease, From: models.ToolEntryPending, To: models.ToolEntryRunning, Now: f.now,
	})
	expectErr(t, "stale transition", err, models.ErrLeaseLost)
	if got := f.entry("e1"); got.State != models.ToolEntryPending || got.Revision != 1 {
		t.Fatalf("stale transition wrote %s rev %d", got.State, got.Revision)
	}
	if got := f.session(f.lane.ID); got.NextSequence != 3 {
		t.Fatalf("stale append moved NextSequence to %d", got.NextSequence)
	}

	freshLease := &models.AgentRunLeaseGuard{WorkerID: "fresh-worker", Now: later}
	result, err := fresh.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: f.lane.ID, RunID: f.leased.ID, Lease: freshLease,
		Entries: []*models.SessionEntry{testUserEntry("u-fresh", "now")}, Now: later,
	})
	if err != nil || idsOf(result.Entries) != "u-fresh" || result.Entries[0].Sequence != 3 {
		t.Fatalf("fresh append = %+v, %v", result, err)
	}
	if _, err := fresh.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: f.scope, EntryID: "e1", RunID: f.leased.ID, Lease: *freshLease, From: models.ToolEntryPending, To: models.ToolEntryInterrupted,
		Result: conformanceResult(models.ToolEntryInterrupted), Now: later,
	}); err != nil {
		t.Fatalf("fresh transition: %v", err)
	}
}

// A stale append that is already inside its transaction when the lease moves
// either commits before the takeover or is refused after it: the Run row lock
// orders the two.
func TestPostgresAgentSessionAppendRacesLeaseTakeover(t *testing.T) {
	f := newSessionReplicaFixture(t)
	stale, fresh := f.replicas[0], f.replicas[1]
	later := f.now.Add(2 * time.Hour)
	const attempts = 8
	var wg sync.WaitGroup
	appendErrs := make([]error, attempts)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, appendErrs[i] = stale.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
				Scope: f.scope, SessionID: f.lane.ID, RunID: f.leased.ID,
				Lease:   &models.AgentRunLeaseGuard{WorkerID: conformanceWorker, Now: f.now},
				Entries: []*models.SessionEntry{testUserEntry(fmt.Sprintf("u%d", i), "racing")}, Now: f.now,
			})
		}(i)
	}
	// The takeover writes the new lease through the Run row, as a reclaim
	// does. (ClaimNextAgentRun itself skips a row an append holds locked.)
	var takeoverErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		current, err := fresh.GetAgentRun(f.ctx, f.scope, f.leased.ID)
		if err != nil {
			takeoverErr = err
			return
		}
		next := models.CloneAgentRun(current)
		expires := later.Add(time.Hour)
		next.LeaseOwner, next.LeaseExpiresAt = "fresh-worker", &expires
		next.Revision = current.Revision + 1
		next.UpdatedAt = later
		_, takeoverErr = fresh.UpdateAgentRunWithEvent(f.ctx, next, current.Revision, &models.ActivityEvent{
			ID: uuid.NewString(), Scope: f.scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: current.AssignedAgentID,
			RunID: current.ID, Actor: models.ActivityActor{Type: "worker", ID: "fresh-worker"}, Summary: "Run reclaimed", Visibility: models.ActivityVisibilityScope, CreatedAt: later,
		}, nil)
	}()
	close(start)
	wg.Wait()
	if takeoverErr != nil {
		t.Fatalf("takeover: %v", takeoverErr)
	}
	committed := 0
	for i, err := range appendErrs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, models.ErrLeaseLost):
			f.assertNoEntry(fmt.Sprintf("u%d", i))
		default:
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := f.store.ListAgentSessionEntries(f.ctx, models.SessionEntryFilter{Scope: f.scope, SessionID: f.lane.ID})
	if err != nil || len(entries) != committed {
		t.Fatalf("entries = %d, %v; %d appends committed", len(entries), err, committed)
	}
	for i, entry := range entries {
		if entry.Sequence != int64(i+1) {
			t.Fatalf("sequence gap: %s", idsOf(entries))
		}
	}
	// After the takeover nothing from the stale worker gets in.
	_, err = stale.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: f.lane.ID, RunID: f.leased.ID,
		Lease:   &models.AgentRunLeaseGuard{WorkerID: conformanceWorker, Now: f.now},
		Entries: []*models.SessionEntry{testUserEntry("u-after", "late")}, Now: f.now,
	})
	expectErr(t, "append after takeover", err, models.ErrLeaseLost)
}

// Sixteen tool entries of one batch settle concurrently from two replicas,
// while sixteen more goroutines race the same entry: every distinct entry
// settles once, and each contested CAS has exactly one winner.
func TestPostgresAgentSessionSixteenConcurrentTransitionsAcrossReplicas(t *testing.T) {
	f := newSessionReplicaFixture(t)
	const calls = 16
	callIDs := make([]string, calls)
	for i := range callIDs {
		callIDs[i] = fmt.Sprintf("c%02d", i)
	}
	batch := []*models.SessionEntry{testAssistantEntry("a1", "s1", callIDs...)}
	for i, callID := range callIDs {
		batch = append(batch, testToolEntry(fmt.Sprintf("e%02d", i), "s1", callID, models.ToolEntryPending))
	}
	f.mustAppend(f.lane.ID, f.leased.ID, batch...)

	var wg sync.WaitGroup
	start := make(chan struct{})
	distinct := make([]error, calls)
	contested := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			req := f.transitionReq(fmt.Sprintf("e%02d", i), f.leased.ID, models.ToolEntryPending, models.ToolEntryCompleted)
			req.ExpectedRevision = 1
			req.Events = []*models.ActivityEvent{f.event(f.leased.ID, fmt.Sprintf("Finished e%02d", i))}
			_, distinct[i] = f.replicas[i%2].TransitionToolEntry(f.ctx, req)
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			req := f.transitionReq("e00", f.leased.ID, models.ToolEntryPending, models.ToolEntryRunning)
			_, contested[i] = f.replicas[(i+1)%2].TransitionToolEntry(f.ctx, req)
		}(i)
	}
	close(start)
	wg.Wait()

	// e00 is raced by its own completion and sixteen running moves: exactly
	// one of those seventeen CASes wins.
	winners := 0
	if distinct[0] == nil {
		winners++
	} else if !errors.Is(distinct[0], models.ErrToolEntryStateConflict) {
		t.Fatalf("e00 completion: %v", distinct[0])
	}
	for i, err := range contested {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, models.ErrToolEntryStateConflict):
		default:
			t.Fatalf("contested %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("e00 had %d winners", winners)
	}
	for i := 1; i < calls; i++ {
		if distinct[i] != nil {
			t.Fatalf("transition e%02d: %v", i, distinct[i])
		}
		if got := f.entry(fmt.Sprintf("e%02d", i)); got.State != models.ToolEntryCompleted || got.Revision != 2 {
			t.Fatalf("e%02d = %s rev %d", i, got.State, got.Revision)
		}
	}
	if got := f.entry("e00"); got.Revision != 2 {
		t.Fatalf("e00 revision = %d", got.Revision)
	}
	want := calls - 1
	if distinct[0] == nil {
		want = calls
	}
	if got := f.activityCount(f.leased.ID); got < want {
		t.Fatalf("activity events = %d, want at least %d", got, want)
	}
	assertDenseActivitySequences(t, f)
}

func assertDenseActivitySequences(t *testing.T, f *sessionReplicaFixture) {
	t.Helper()
	events, err := f.store.ListActivity(f.ctx, models.ActivityFilter{Scope: f.scope, RunID: f.leased.ID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool, len(events))
	for _, event := range events {
		if seen[event.Sequence] {
			t.Fatalf("duplicate activity sequence %d", event.Sequence)
		}
		seen[event.Sequence] = true
	}
}

// The in-transaction helpers commit or roll back with the caller's
// transaction.
func TestPostgresAgentSessionTxHelpersRollBackWithCaller(t *testing.T) {
	f := newSessionReplicaFixture(t)
	store := f.replicas[0]
	child := f.leasedRun("agent-a", "")

	withRolledBackTx := func(work func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := store.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := work(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}

	withRolledBackTx(func(tx *sql.Tx) error {
		_, err := store.createRunAgentSessionTx(f.ctx, tx, models.CreateRunAgentSessionTxRequest{
			Scope: f.scope, OwnerRunID: child.ID, AgentID: "agent-a", ParentSessionID: f.lane.ID,
			InitialEntries: []*models.SessionEntry{goalEntry("g1", child.ID)}, Now: f.now,
		})
		return err
	})
	if _, err := store.GetAgentSession(f.ctx, f.scope, models.RunAgentSessionID(f.scope, child.ID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
		t.Fatalf("rolled-back create left a session: %v", err)
	}
	f.assertNoEntry("g1")
	if got := f.run(child.ID); got.SessionID != "" || got.Revision != child.Revision {
		t.Fatalf("rolled-back create changed the child Run: %q rev %d", got.SessionID, got.Revision)
	}

	withRolledBackTx(func(tx *sql.Tx) error {
		if _, err := store.lockPostgresSessionRunTx(f.ctx, tx, f.scope, f.leased.ID, nil); err != nil {
			return err
		}
		_, err := store.appendAgentSessionEntriesTx(f.ctx, tx, f.appendReq(f.lane.ID, f.leased.ID, testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", models.ToolEntryPending)))
		return err
	})
	f.assertNoEntry("a1")
	if got := f.session(f.lane.ID); got.NextSequence != 1 {
		t.Fatalf("rolled-back append moved NextSequence to %d", got.NextSequence)
	}

	f.mustAppend(f.lane.ID, f.leased.ID, testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", models.ToolEntryPending))
	withRolledBackTx(func(tx *sql.Tx) error {
		if _, err := store.lockPostgresSessionRunTx(f.ctx, tx, f.scope, f.leased.ID, nil); err != nil {
			return err
		}
		_, err := store.transitionToolEntryTx(f.ctx, tx, f.transitionReq("e1", f.leased.ID, models.ToolEntryPending, models.ToolEntryRunning))
		return err
	})
	if got := f.entry("e1"); got.State != models.ToolEntryPending || got.Revision != 1 {
		t.Fatalf("rolled-back transition wrote %s rev %d", got.State, got.Revision)
	}
}
