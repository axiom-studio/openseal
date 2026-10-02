package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunEventRetentionBoundsAndSafetyPolicy(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		oldCutoff := time.Now().UTC().Add(-RunEventRetention - time.Minute)
		for _, test := range []struct {
			name   string
			before time.Time
			limit  int
		}{
			{"zero cutoff", time.Time{}, 10},
			{"recent cutoff", time.Now().UTC().Add(-RunEventRetention + time.Hour), 10},
			{"future cutoff", time.Now().UTC().Add(time.Hour), 10},
			{"out of range cutoff", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), 10},
			{"zero batch", oldCutoff, 0},
			{"negative batch", oldCutoff, -1},
			{"oversized batch", oldCutoff, 1001},
		} {
			t.Run(test.name, func(t *testing.T) {
				if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), test.before, test.limit); count != 0 || !errors.Is(err, ErrInvalidRunEventWait) {
					t.Fatalf("unsafe maintenance accepted: count=%d error=%v", count, err)
				}
			})
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if count, err := fixture.store.PruneExpiredRunEvents(ctx, oldCutoff, 1000); count != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled maintenance: count=%d error=%v", count, err)
		}
	})
}

func TestRunEventRetentionBoundedSweepProtectsPausedWaitsAcrossTenants(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		old := now.Add(-40 * 24 * time.Hour)
		before := now.Add(-RunEventRetention)
		own := Scope{Kind: "tenant", ID: "a-retention"}
		foreign := Scope{Kind: "tenant", ID: "z-retention"}
		spec := eventWaitContractSpec("reply", "kept-buffer", old)
		pending := eventWaitContractCreate(t, fixture.store, own, spec, old)
		paused := eventWaitContractCreate(t, fixture.store, foreign, spec, old)
		commands := NewRunCommandService(fixture.store)
		commands.now = func() time.Time { return old }
		pausedResult, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
			Scope: foreign, RunID: paused.ID, ExpectedRevision: paused.Revision, Kind: AgentRunCommandPause,
			Actor: ActivityActor{Type: "user", ID: "owner"},
		})
		if err != nil {
			t.Fatal(err)
		}
		paused = pausedResult.Run
		for _, scope := range []Scope{own, foreign} {
			// The identity intentionally repeats across tenants. Protection must
			// use the complete scoped selector rather than a provider event ID.
			eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, spec, "protected", old), old)
		}
		const expiredCount = 2500
		unrelated := spec
		unrelated.Subject = "unrelated"
		for index := 0; index < expiredCount; index++ {
			scope := own
			if index%2 == 1 {
				scope = foreign
			}
			at := old.Add(time.Minute)
			eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, unrelated, fmt.Sprintf("expired-%04d", index), at), at)
		}
		// Exact matching must not retain events with the right broad selector
		// but the wrong sender, event boundary, or deadline.
		wrongSender := eventWaitContractEvent(own, spec, "wrong-sender", old.Add(time.Minute))
		wrongSender.Attributes["externalParticipantId"] = "another-person"
		eventWaitContractPublish(t, fixture.store, wrongSender, old.Add(time.Minute))
		tooEarly := eventWaitContractEvent(foreign, spec, "too-early", old.Add(-time.Nanosecond))
		eventWaitContractPublish(t, fixture.store, tooEarly, old.Add(time.Minute))
		late := eventWaitContractEvent(foreign, spec, "late", old.Add(time.Minute))
		eventWaitContractPublish(t, fixture.store, late, spec.Deadline.Add(time.Second))
		young := eventWaitContractEvent(own, unrelated, "young", now.Add(-time.Hour))
		eventWaitContractPublish(t, fixture.store, young, now.Add(-time.Hour))
		if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), before, 1); err != nil || count != 0 {
			t.Fatalf("first protected candidate was deleted: count=%d error=%v", count, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), before, 1); err != nil || count != 0 {
			t.Fatalf("foreign paused candidate was deleted: count=%d error=%v", count, err)
		}
		if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), before, 1); err != nil || count != 1 {
			t.Fatalf("protected rows starved subsequent candidates after restart: count=%d error=%v", count, err)
		}
		deleted := 1
		for tick := 0; tick < 6; tick++ {
			count, err := fixture.store.PruneExpiredRunEvents(t.Context(), before, 1000)
			if err != nil || count < 0 || count > 1000 {
				t.Fatalf("unbounded maintenance batch: count=%d error=%v", count, err)
			}
			deleted += count
		}
		if deleted != expiredCount+3 {
			t.Fatalf("incorrect expired payload cleanup: deleted=%d want=%d", deleted, expiredCount+3)
		}
		if sqlite, ok := fixture.store.(*SQLiteStore); ok {
			var orphaned int
			if err := sqlite.db.QueryRow(`SELECT COUNT(*) FROM run_event_notifications n
				WHERE NOT EXISTS (SELECT 1 FROM run_event_inbox e WHERE e.scope_kind=n.scope_kind
				AND e.scope_id=n.scope_id AND e.source=n.source AND e.event_id=n.event_id)`).Scan(&orphaned); err != nil || orphaned != 0 {
				t.Fatalf("deleted payload left orphan notifications: count=%d error=%v", orphaned, err)
			}
		}
		// The buffered events are still consumable even after a long pause.
		eventWaitContractProcess(t, fixture.store, own, now, 1)
		pending, err = fixture.store.GetAgentRun(t.Context(), own, pending.ID)
		if err != nil || pending.Checkpoint["lastEventWait"].(map[string]interface{})["event"].(map[string]interface{})["id"] != "protected" {
			t.Fatalf("pending protected receipt was lost: run=%#v error=%v", pending, err)
		}
		commands = NewRunCommandService(fixture.store)
		commands.now = func() time.Time { return now }
		if _, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
			Scope: foreign, RunID: paused.ID, ExpectedRevision: paused.Revision, Kind: AgentRunCommandResume,
			Actor: ActivityActor{Type: "user", ID: "owner"},
		}); err != nil {
			t.Fatal(err)
		}
		eventWaitContractProcess(t, fixture.store, foreign, now, 1)
		paused, err = fixture.store.GetAgentRun(t.Context(), foreign, paused.ID)
		if err != nil || paused.Checkpoint["lastEventWait"].(map[string]interface{})["event"].(map[string]interface{})["id"] != "protected" {
			t.Fatalf("paused protected receipt was lost: run=%#v error=%v", paused, err)
		}
		cleaned := 0
		for tick := 0; tick < 3; tick++ {
			count, err := fixture.store.PruneExpiredRunEvents(t.Context(), before, 1000)
			if err != nil {
				t.Fatal(err)
			}
			cleaned += count
		}
		if cleaned != 2 {
			t.Fatalf("resolved buffers were not collected: count=%d", cleaned)
		}
		if inserted := eventWaitContractPublish(t, fixture.store, young, now.Add(-time.Hour)); inserted {
			t.Fatal("maintenance deleted a payload inside the retention period")
		}
	})
}

func TestRunEventRetentionKeepsOncePerRunConsumptionAfterPayloadDeletion(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		old := now.Add(-40 * 24 * time.Hour)
		scope := Scope{Kind: "tenant", ID: "retention-consumption"}
		spec := eventWaitContractSpec("first", "response", old)
		run := eventWaitContractCreate(t, fixture.store, scope, spec, old)
		event := eventWaitContractEvent(scope, spec, "reply", old.Add(time.Second))
		eventWaitContractPublish(t, fixture.store, event, old.Add(time.Second))
		eventWaitContractProcess(t, fixture.store, scope, old.Add(2*time.Second), 1)
		if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 10); err != nil || count != 1 {
			t.Fatalf("consumed payload cleanup: count=%d error=%v", count, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		if inserted := eventWaitContractPublish(t, fixture.store, event, old.Add(time.Second)); !inserted {
			t.Fatal("test event payload was not removed")
		}
		stored, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		spec.Key = "second"
		stored.Status = AgentRunStatusWaitingForEvent
		stored.WakeCondition = &WakeCondition{Type: "event", EventWait: &spec}
		expected := stored.Revision
		stored.Revision++
		stored.UpdatedAt = old.Add(3 * time.Second)
		if _, err := fixture.store.UpdateAgentRunWithEvent(t.Context(), stored, expected, &ActivityEvent{
			ID: uuid.NewString(), Scope: scope, RunID: stored.ID, EventType: "run.waiting", Summary: "Await another reply", CreatedAt: stored.UpdatedAt,
		}, nil); err != nil {
			t.Fatal(err)
		}
		eventWaitContractProcess(t, fixture.store, scope, old.Add(4*time.Second), 0)
		wait, err := fixture.store.GetRunEventWait(t.Context(), scope, stored.ID, spec.Key)
		if err != nil || wait.Status != RunEventWaitPending {
			t.Fatalf("deleted payload erased the once-per-Run consumption ledger: wait=%#v error=%v", wait, err)
		}
		if count, err := fixture.store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 10); err != nil || count != 1 {
			t.Fatalf("already consumed event incorrectly protected: count=%d error=%v", count, err)
		}
	})
}

func TestRunEventRetentionConcurrentMaintenanceCountsEachPayloadOnce(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		old := now.Add(-40 * 24 * time.Hour)
		scope := Scope{Kind: "tenant", ID: "concurrent-retention"}
		spec := eventWaitContractSpec("reply", "unmatched", old)
		const count = 250
		for index := 0; index < count; index++ {
			eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, spec, fmt.Sprintf("expired-%03d", index), old), old)
		}
		var group sync.WaitGroup
		deleted := make(chan int, 8)
		failures := make(chan error, 8)
		for worker := 0; worker < 8; worker++ {
			group.Go(func() {
				local := 0
				for tick := 0; tick < 10; tick++ {
					removed, err := fixture.store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 10)
					if err != nil {
						failures <- err
						return
					}
					local += removed
				}
				deleted <- local
			})
		}
		group.Wait()
		close(deleted)
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
		total := 0
		for removed := range deleted {
			total += removed
		}
		// Replicas that skip a held PostgreSQL cursor may complete all their
		// ticks before its owner; finish the sweep through the same API.
		for tick := 0; tick < count/10+2; tick++ {
			removed, err := fixture.store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 10)
			if err != nil {
				t.Fatal(err)
			}
			total += removed
		}
		if total != count {
			t.Fatalf("concurrent batches lost or double-counted deletion: count=%d want=%d", total, count)
		}
	})
}

func TestSQLiteRunEventRetentionCursorAndDeleteRollbackTogether(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-40 * 24 * time.Hour)
	scope := Scope{Kind: "tenant", ID: "retention-rollback"}
	spec := eventWaitContractSpec("reply", "unmatched", old)
	eventWaitContractPublish(t, store, eventWaitContractEvent(scope, spec, "expired", old), old)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_retention BEFORE DELETE ON run_event_inbox
		BEGIN SELECT RAISE(ABORT,'injected retention failure'); END`); err != nil {
		t.Fatal(err)
	}
	if count, err := store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 1); err == nil || count != 0 {
		t.Fatalf("injected failure did not abort maintenance: count=%d error=%v", count, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM run_event_inbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("payload deletion survived failed batch: count=%d error=%v", count, err)
	}
	var position interface{}
	if err := store.db.QueryRow(`SELECT received_at FROM run_event_retention_cursor WHERE id=1`).Scan(&position); err != nil || position != nil {
		t.Fatalf("cursor advanced after failed batch: position=%v error=%v", position, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_retention`); err != nil {
		t.Fatal(err)
	}
	if count, err := store.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 1); err != nil || count != 1 {
		t.Fatalf("batch was not retriable: count=%d error=%v", count, err)
	}
}

func TestSQLiteRunEventRetentionUsesGlobalReceivedIndex(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, query := range []string{
		`SELECT received_at,scope_kind,scope_id,source,event_id FROM run_event_inbox WHERE received_at<? ORDER BY received_at,scope_kind,scope_id,source,event_id LIMIT 1000`,
		`SELECT received_at,scope_kind,scope_id,source,event_id FROM run_event_inbox WHERE received_at<? AND (received_at,scope_kind,scope_id,source,event_id)>(?,?,?,?,?) ORDER BY received_at,scope_kind,scope_id,source,event_id LIMIT 1000`,
	} {
		args := []interface{}{now.Add(-RunEventRetention)}
		if strings.Contains(query, ">(?,?,?,?,?)") {
			args = append(args, now.Add(-40*24*time.Hour), "tenant", "one", "host:source", "one")
		}
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			plan += detail
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil || !strings.Contains(plan, "idx_run_event_inbox_received") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("maintenance batch scans or sorts the global inbox: plan=%s error=%v", plan, err)
		}
	}
}

func TestSQLiteRunEventScopedRetentionBoundsRestartAndTenantIsolation(t *testing.T) {
	path := t.TempDir() + "/scoped-retention.db"
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-40 * 24 * time.Hour)
	own := Scope{Kind: "tenant", ID: "scoped-retention"}
	foreign := Scope{Kind: "tenant", ID: "another-retention"}
	spec := eventWaitContractSpec("reply", "kept", old)
	run := eventWaitContractCreate(t, store, own, spec, old)
	eventWaitContractPublish(t, store, eventWaitContractEvent(own, spec, "protected", old), old)
	unrelated := spec
	unrelated.Subject = "unrelated"
	for index := 0; index < 10; index++ {
		at := old.Add(time.Minute)
		eventWaitContractPublish(t, store, eventWaitContractEvent(own, unrelated, fmt.Sprintf("expired-%02d", index), at), at)
	}
	foreignEvent := eventWaitContractEvent(foreign, unrelated, "expired-00", old.Add(time.Minute))
	eventWaitContractPublish(t, store, foreignEvent, old.Add(time.Minute))
	if count, err := store.PruneRunEvents(t.Context(), own, now, 1); err != nil || count != 0 {
		t.Fatalf("scoped protected candidate: count=%d error=%v", count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := store.PruneRunEvents(t.Context(), own, now, 1); err != nil || count != 1 {
		t.Fatalf("scoped cursor did not survive restart: count=%d error=%v", count, err)
	}
	deleted := 1
	for tick := 0; tick < 5; tick++ {
		count, err := store.PruneRunEvents(t.Context(), own, now, 3)
		if err != nil || count > 3 {
			t.Fatalf("scoped batch exceeded candidate bound: count=%d error=%v", count, err)
		}
		deleted += count
	}
	if deleted != 10 {
		t.Fatalf("scoped sweep starved or deleted protected/foreign payloads: deleted=%d", deleted)
	}
	if inserted := eventWaitContractPublish(t, store, foreignEvent, old.Add(time.Minute)); inserted {
		t.Fatal("scoped maintenance deleted a foreign tenant payload")
	}
	var orphaned int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM run_event_notifications n WHERE NOT EXISTS
		(SELECT 1 FROM run_event_inbox e WHERE e.scope_kind=n.scope_kind AND e.scope_id=n.scope_id
		AND e.source=n.source AND e.event_id=n.event_id)`).Scan(&orphaned); err != nil || orphaned != 0 {
		t.Fatalf("scoped maintenance left orphan notifications: count=%d error=%v", orphaned, err)
	}
	eventWaitContractProcess(t, store, own, now, 1)
	wait, err := store.GetRunEventWait(t.Context(), own, run.ID, spec.Key)
	if err != nil || wait.Status != RunEventWaitMatched || wait.EventID != "protected" {
		t.Fatalf("scoped sweep lost protected buffer: wait=%#v error=%v", wait, err)
	}
}
