//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"
)

func TestPostgresRunEventRetentionSkipsAnotherReplicaCursorLease(t *testing.T) {
	store, other := newPostgresRunEventWaitTestStores(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-40 * 24 * time.Hour)
	scope := Scope{Kind: "tenant", ID: "retention-replica"}
	spec := eventWaitContractSpec("reply", "unmatched", old)
	eventWaitContractPublish(t, store, eventWaitContractEvent(scope, spec, "expired", old), old)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var id int
	if err := tx.QueryRowContext(t.Context(), `SELECT id FROM `+store.table("run_event_retention_cursor")+` WHERE id=1 FOR UPDATE`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if count, err := other.PruneExpiredRunEvents(ctx, now.Add(-RunEventRetention), 1); err != nil || count != 0 {
		t.Fatalf("replica blocked or stole maintenance lease: count=%d error=%v", count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count, err := other.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 1); err != nil || count != 1 {
		t.Fatalf("released batch lease could not be recovered: count=%d error=%v", count, err)
	}
}

func TestPostgresRunEventRetentionLocksOnlyTheBoundedCandidatePage(t *testing.T) {
	store, other := newPostgresRunEventWaitTestStores(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-40 * 24 * time.Hour)
	scope := Scope{Kind: "tenant", ID: "retention-locked-page"}
	spec := eventWaitContractSpec("reply", "unmatched", old)
	for _, id := range []string{"first", "second"} {
		eventWaitContractPublish(t, store, eventWaitContractEvent(scope, spec, id, old), old)
	}
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(t.Context(), `SELECT event_id FROM `+store.table("run_event_inbox")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND source=$3 AND event_id='first' FOR UPDATE`,
		scope.Kind, scope.ID, spec.Source).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if count, err := other.PruneExpiredRunEvents(ctx, now.Add(-RunEventRetention), 1); err != nil || count != 0 {
		t.Fatalf("batch scanned past its locked candidate: count=%d error=%v", count, err)
	}
	if count, err := other.PruneExpiredRunEvents(ctx, now.Add(-RunEventRetention), 1); err != nil || count != 1 {
		t.Fatalf("locked candidate starved subsequent batch: count=%d error=%v", count, err)
	}
	// Reach the end of the cursor sweep, then release the skipped receipt.
	if count, err := other.PruneExpiredRunEvents(ctx, now.Add(-RunEventRetention), 1); err != nil || count != 0 {
		t.Fatalf("end-of-sweep reset: count=%d error=%v", count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count, err := other.PruneExpiredRunEvents(t.Context(), now.Add(-RunEventRetention), 1); err != nil || count != 1 {
		t.Fatalf("skipped receipt was not revisited: count=%d error=%v", count, err)
	}
}
