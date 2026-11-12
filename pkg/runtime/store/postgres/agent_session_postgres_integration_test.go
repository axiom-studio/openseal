//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// Store test: it runs storetest's session conformance suite and checks
// what only replicas of one Postgres database can show.

// newAgentSessionPostgresStores opens n PostgresStores (replicas) on one fresh
// schema. The schema is dropped when the test ends.
func newAgentSessionPostgresStores(t *testing.T, n int) []*Store {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "agent_session_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	stores := make([]*Store, n)
	for i := range stores {
		store, err := New(ctx, dsn, WithSchema(schema))
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
// (storetest.RunAgentSessionContract) against the Postgres backend.
func TestPostgresAgentSessionStore(t *testing.T) {
	storetest.RunAgentSessionContract(t, func(t *testing.T) (storetest.AgentSessionContractStore, storetest.CreateRunSessionInOwnTx) {
		store := newAgentSessionPostgresStores(t, 1)[0]
		return store, func(ctx context.Context, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
			return createPostgresRunSessionInOwnTx(ctx, store, req)
		}
	})
}

// createPostgresRunSessionInOwnTx runs the store's in-transaction
// createRunAgentSessionTx helper in a transaction of its own.
func createPostgresRunSessionInOwnTx(ctx context.Context, s *Store, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
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
	*storetest.SessionFixture
	replicas []*Store
	leased   *models.AgentRun
	lane     *models.AgentSession
}

func newSessionReplicaFixture(t *testing.T) *sessionReplicaFixture {
	t.Helper()
	replicas := newAgentSessionPostgresStores(t, 2)
	f := &storetest.SessionFixture{
		T:     t,
		Ctx:   context.Background(),
		Store: replicas[0],
		Scope: models.Scope{Kind: "tenant", ID: "session-" + uuid.NewString()[:8]},
		Now:   time.Now().UTC().Truncate(time.Millisecond),
	}
	run, session := f.ConversationRun("agent-a")
	return &sessionReplicaFixture{SessionFixture: f, replicas: replicas, leased: run, lane: session}
}

// A replica whose lease expired and was taken over by another replica cannot
// append to the session or settle the Run's tool entries; nothing it sends is
// written. The new holder's writes go through.
func TestPostgresAgentSessionStaleReplicaAppendRefused(t *testing.T) {
	f := newSessionReplicaFixture(t)
	stale, fresh := f.replicas[0], f.replicas[1]
	f.MustAppend(f.lane.ID, f.leased.ID, storetest.AssistantSessionEntry("a1", "s1", "c1"), storetest.ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending))

	// The stale worker's lease runs out; the other replica reclaims the Run.
	later := f.Now.Add(2 * time.Hour)
	claimed, err := fresh.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Scope, WorkerID: "fresh-worker", Now: later, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != f.leased.ID || claimed.LeaseOwner != "fresh-worker" {
		t.Fatalf("reclaim = %#v, %v", claimed, err)
	}

	// The stale worker still believes its lease is live (its clock says f.now).
	staleLease := &models.AgentRunLeaseGuard{WorkerID: storetest.SessionWorker, Now: f.Now}
	_, err = stale.AppendAgentSessionEntries(f.Ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.Scope, SessionID: f.lane.ID, RunID: f.leased.ID, Lease: staleLease,
		Entries: []*models.SessionEntry{storetest.UserSessionEntry("u-stale", "late")}, Events: []*models.ActivityEvent{f.Event(f.leased.ID, "stale")}, Now: f.Now,
	})
	storetest.ExpectErr(t, "stale append", err, models.ErrLeaseLost)
	f.AssertNoEntry("u-stale")
	_, err = stale.TransitionToolEntry(f.Ctx, models.ToolEntryTransitionRequest{
		Scope: f.Scope, EntryID: "e1", RunID: f.leased.ID, Lease: *staleLease, From: models.ToolEntryPending, To: models.ToolEntryRunning, Now: f.Now,
	})
	storetest.ExpectErr(t, "stale transition", err, models.ErrLeaseLost)
	if got := f.Entry("e1"); got.State != models.ToolEntryPending || got.Revision != 1 {
		t.Fatalf("stale transition wrote %s rev %d", got.State, got.Revision)
	}
	if got := f.Session(f.lane.ID); got.NextSequence != 3 {
		t.Fatalf("stale append moved NextSequence to %d", got.NextSequence)
	}

	freshLease := &models.AgentRunLeaseGuard{WorkerID: "fresh-worker", Now: later}
	result, err := fresh.AppendAgentSessionEntries(f.Ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.Scope, SessionID: f.lane.ID, RunID: f.leased.ID, Lease: freshLease,
		Entries: []*models.SessionEntry{storetest.UserSessionEntry("u-fresh", "now")}, Now: later,
	})
	if err != nil || storetest.EntryIDs(result.Entries) != "u-fresh" || result.Entries[0].Sequence != 3 {
		t.Fatalf("fresh append = %+v, %v", result, err)
	}
	if _, err := fresh.TransitionToolEntry(f.Ctx, models.ToolEntryTransitionRequest{
		Scope: f.Scope, EntryID: "e1", RunID: f.leased.ID, Lease: *freshLease, From: models.ToolEntryPending, To: models.ToolEntryInterrupted,
		Result: storetest.ToolResultFor(models.ToolEntryInterrupted), Now: later,
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
	later := f.Now.Add(2 * time.Hour)
	const attempts = 8
	var wg sync.WaitGroup
	appendErrs := make([]error, attempts)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, appendErrs[i] = stale.AppendAgentSessionEntries(f.Ctx, models.AppendAgentSessionEntriesRequest{
				Scope: f.Scope, SessionID: f.lane.ID, RunID: f.leased.ID,
				Lease:   &models.AgentRunLeaseGuard{WorkerID: storetest.SessionWorker, Now: f.Now},
				Entries: []*models.SessionEntry{storetest.UserSessionEntry(fmt.Sprintf("u%d", i), "racing")}, Now: f.Now,
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
		current, err := fresh.GetAgentRun(f.Ctx, f.Scope, f.leased.ID)
		if err != nil {
			takeoverErr = err
			return
		}
		next := models.CloneAgentRun(current)
		expires := later.Add(time.Hour)
		next.LeaseOwner, next.LeaseExpiresAt = "fresh-worker", &expires
		next.Revision = current.Revision + 1
		next.UpdatedAt = later
		_, takeoverErr = fresh.UpdateAgentRunWithEvent(f.Ctx, next, current.Revision, &models.ActivityEvent{
			ID: uuid.NewString(), Scope: f.Scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: current.AssignedAgentID,
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
			f.AssertNoEntry(fmt.Sprintf("u%d", i))
		default:
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, SessionID: f.lane.ID})
	if err != nil || len(entries) != committed {
		t.Fatalf("entries = %d, %v; %d appends committed", len(entries), err, committed)
	}
	for i, entry := range entries {
		if entry.Sequence != int64(i+1) {
			t.Fatalf("sequence gap: %s", storetest.EntryIDs(entries))
		}
	}
	// After the takeover nothing from the stale worker gets in.
	_, err = stale.AppendAgentSessionEntries(f.Ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.Scope, SessionID: f.lane.ID, RunID: f.leased.ID,
		Lease:   &models.AgentRunLeaseGuard{WorkerID: storetest.SessionWorker, Now: f.Now},
		Entries: []*models.SessionEntry{storetest.UserSessionEntry("u-after", "late")}, Now: f.Now,
	})
	storetest.ExpectErr(t, "append after takeover", err, models.ErrLeaseLost)
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
	batch := []*models.SessionEntry{storetest.AssistantSessionEntry("a1", "s1", callIDs...)}
	for i, callID := range callIDs {
		batch = append(batch, storetest.ToolSessionEntry(fmt.Sprintf("e%02d", i), "s1", callID, models.ToolEntryPending))
	}
	f.MustAppend(f.lane.ID, f.leased.ID, batch...)

	var wg sync.WaitGroup
	start := make(chan struct{})
	distinct := make([]error, calls)
	contested := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			req := f.TransitionRequest(fmt.Sprintf("e%02d", i), f.leased.ID, models.ToolEntryPending, models.ToolEntryCompleted)
			req.ExpectedRevision = 1
			req.Events = []*models.ActivityEvent{f.Event(f.leased.ID, fmt.Sprintf("Finished e%02d", i))}
			_, distinct[i] = f.replicas[i%2].TransitionToolEntry(f.Ctx, req)
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			req := f.TransitionRequest("e00", f.leased.ID, models.ToolEntryPending, models.ToolEntryRunning)
			_, contested[i] = f.replicas[(i+1)%2].TransitionToolEntry(f.Ctx, req)
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
		if got := f.Entry(fmt.Sprintf("e%02d", i)); got.State != models.ToolEntryCompleted || got.Revision != 2 {
			t.Fatalf("e%02d = %s rev %d", i, got.State, got.Revision)
		}
	}
	if got := f.Entry("e00"); got.Revision != 2 {
		t.Fatalf("e00 revision = %d", got.Revision)
	}
	want := calls - 1
	if distinct[0] == nil {
		want = calls
	}
	if got := f.ActivityCount(f.leased.ID); got < want {
		t.Fatalf("activity events = %d, want at least %d", got, want)
	}
	assertDenseActivitySequences(t, f)
}

func assertDenseActivitySequences(t *testing.T, f *sessionReplicaFixture) {
	t.Helper()
	events, err := f.Store.ListActivity(f.Ctx, models.ActivityFilter{Scope: f.Scope, RunID: f.leased.ID, Limit: 500})
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
	child := f.LeasedRun("agent-a", "")

	withRolledBackTx := func(work func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := store.db.BeginTx(f.Ctx, nil)
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
		_, err := store.createRunAgentSessionTx(f.Ctx, tx, models.CreateRunAgentSessionTxRequest{
			Scope: f.Scope, OwnerRunID: child.ID, AgentID: "agent-a", ParentSessionID: f.lane.ID,
			InitialEntries: []*models.SessionEntry{storetest.GoalSessionEntry("g1", child.ID)}, Now: f.Now,
		})
		return err
	})
	if _, err := store.GetAgentSession(f.Ctx, f.Scope, models.RunAgentSessionID(f.Scope, child.ID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
		t.Fatalf("rolled-back create left a session: %v", err)
	}
	f.AssertNoEntry("g1")
	if got := f.Run(child.ID); got.SessionID != "" || got.Revision != child.Revision {
		t.Fatalf("rolled-back create changed the child Run: %q rev %d", got.SessionID, got.Revision)
	}

	withRolledBackTx(func(tx *sql.Tx) error {
		if _, err := store.lockPostgresSessionRunTx(f.Ctx, tx, f.Scope, f.leased.ID, nil); err != nil {
			return err
		}
		_, err := store.appendAgentSessionEntriesTx(f.Ctx, tx, f.AppendRequest(f.lane.ID, f.leased.ID, storetest.AssistantSessionEntry("a1", "s1", "c1"), storetest.ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending)))
		return err
	})
	f.AssertNoEntry("a1")
	if got := f.Session(f.lane.ID); got.NextSequence != 1 {
		t.Fatalf("rolled-back append moved NextSequence to %d", got.NextSequence)
	}

	f.MustAppend(f.lane.ID, f.leased.ID, storetest.AssistantSessionEntry("a1", "s1", "c1"), storetest.ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending))
	withRolledBackTx(func(tx *sql.Tx) error {
		if _, err := store.lockPostgresSessionRunTx(f.Ctx, tx, f.Scope, f.leased.ID, nil); err != nil {
			return err
		}
		_, err := store.transitionToolEntryTx(f.Ctx, tx, f.TransitionRequest("e1", f.leased.ID, models.ToolEntryPending, models.ToolEntryRunning))
		return err
	})
	if got := f.Entry("e1"); got.State != models.ToolEntryPending || got.Revision != 1 {
		t.Fatalf("rolled-back transition wrote %s rev %d", got.State, got.Revision)
	}
}
