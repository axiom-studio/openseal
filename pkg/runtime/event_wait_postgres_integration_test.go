//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresRunEventWaitConcurrentClaimsRestartAndConsumption(t *testing.T) {
	primary, replica := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "event-waits"}
	spec := RunEventWaitSpec{Key: "first", Type: "message.received", Source: "host:mail", Subject: "thread-123", After: now.Add(-time.Minute), Deadline: now.Add(time.Hour), Attributes: map[string]interface{}{"sender": "someone", "read": false, "number": 2}}
	receipt := postgresRunEventWaitTestReceipt(scope, spec, "event-1", now)
	if created, err := primary.PublishRunEvent(ctx, receipt); err != nil || !created {
		t.Fatalf("publish before arm: created=%v err=%v", created, err)
	}
	if scopes, err := primary.ListRunEventWaitWorkScopes(ctx, now, 100); err != nil || len(scopes) != 1 || scopes[0] != scope {
		t.Fatalf("publication did not schedule bounded acquisition: scopes=%v err=%v", scopes, err)
	}
	postgresRunEventWaitTestDrainUnmatchedNotifications(t, primary, scope, now)
	if created, err := replica.PublishRunEvent(ctx, receipt); err != nil || created {
		t.Fatalf("duplicate publish: created=%v err=%v", created, err)
	}
	collision := postgresRunEventWaitTestReceipt(scope, spec, "event-1", now)
	collision.Event.Payload = map[string]interface{}{"text": "different immutable observation"}
	collision = postgresRunEventWaitTestRedigest(collision)
	if _, err := primary.PublishRunEvent(ctx, collision); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("collision error=%v", err)
	}
	run := postgresRunEventWaitTestCreateRun(t, primary, scope, spec, now)
	if scopes, err := primary.ListRunEventWaitWorkScopes(ctx, now, 1); err != nil || len(scopes) != 1 || scopes[0] != scope {
		t.Fatalf("armed work discovery: scopes=%v err=%v", scopes, err)
	}
	var wg sync.WaitGroup
	claims := make(chan *RunEventWait, 2)
	failures := make(chan error, 2)
	for index, store := range []*PostgresStore{primary, replica} {
		wg.Go(func() {
			waits, err := store.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: []string{"one", "two"}[index], Now: now, LeaseDuration: time.Minute, Limit: 100})
			if err != nil {
				failures <- err
				return
			}
			for _, wait := range waits {
				claims <- wait
			}
		})
	}
	wg.Wait()
	close(claims)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var claim *RunEventWait
	for current := range claims {
		if claim != nil {
			t.Fatal("two replicas leased the same event wait")
		}
		claim = current
	}
	if claim == nil {
		t.Fatal("no initial claim")
	}
	if scopes, err := primary.ListRunEventWaitWorkScopes(ctx, now.Add(time.Second), 100); err != nil || len(scopes) != 0 {
		t.Fatalf("leased wait kept tenant hot: scopes=%v err=%v", scopes, err)
	}
	if _, err := primary.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: "foreign-worker", LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign lease error=%v", err)
	}
	// Reclaim after the first worker dies. Its old lease cannot subsequently
	// change the execution, even when it comes back with the original receipt.
	recoveredAt := now.Add(2 * time.Minute)
	recovered := postgresRunEventWaitTestClaim(t, replica, scope, claim.LeaseOwner, recoveredAt, 1)
	if _, err := primary.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: recoveredAt}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired original lease error=%v", err)
	}
	resolved, err := replica.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: recovered.LeaseOwner, LeaseExpiresAt: *recovered.LeaseExpiresAt, Now: recoveredAt})
	if err != nil || resolved.Wait.Status != RunEventWaitMatched || resolved.Run.Status != AgentRunStatusQueued || resolved.Event.ID != receipt.Event.ID {
		t.Fatalf("resolution=%#v err=%v", resolved, err)
	}
	if _, err := primary.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: recovered.LeaseOwner, LeaseExpiresAt: *recovered.LeaseExpiresAt, Now: recoveredAt}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("repeat resolution error=%v", err)
	}
	if _, err := primary.GetRunEventWait(ctx, Scope{Kind: "tenant", ID: "foreign"}, run.ID, spec.Key); !errors.Is(err, ErrRunEventWaitNotFound) {
		t.Fatalf("cross tenant read error=%v", err)
	}
	restarted, err := NewPostgresStore(ctx, os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"), WithPostgresSchema(primary.schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetRunEventWait(ctx, scope, run.ID, spec.Key)
	if err != nil || restored.Status != RunEventWaitMatched || restored.EventID != receipt.Event.ID {
		t.Fatalf("restart wait=%#v err=%v", restored, err)
	}
	// A successive wait in the same execution must not consume event-1 again.
	nextSpec := spec
	nextSpec.Key = "second"
	next := cloneAgentRun(resolved.Run)
	next.Status = AgentRunStatusWaitingForEvent
	next.WakeCondition = &WakeCondition{Type: "event", EventWait: &nextSpec}
	next.Revision++
	next.UpdatedAt = recoveredAt
	postgresRunEventWaitTestUpdateRun(t, restarted, next, resolved.Run.Revision)
	second := postgresRunEventWaitTestClaim(t, restarted, scope, "second-worker", recoveredAt, 1)
	idle, err := restarted.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: nextSpec.Key, WorkerID: second.LeaseOwner, LeaseExpiresAt: *second.LeaseExpiresAt, Now: recoveredAt})
	if err != nil || idle.Run != nil || idle.Wait.Status != RunEventWaitPending || idle.Wait.AvailableAt == nil || !idle.Wait.AvailableAt.Equal(spec.Deadline) {
		t.Fatalf("consumed event repeated or idle polled: result=%#v err=%v", idle, err)
	}
	if claims, err := restarted.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "idle", Now: recoveredAt.Add(time.Minute), LeaseDuration: time.Minute, Limit: 100}); err != nil || len(claims) != 0 {
		t.Fatalf("idle claims=%d err=%v", len(claims), err)
	}
	if scopes, err := restarted.ListRunEventWaitWorkScopes(ctx, recoveredAt.Add(time.Minute), 100); err != nil || len(scopes) != 0 {
		t.Fatalf("idle tenant rediscovered: scopes=%v err=%v", scopes, err)
	}
}

func TestPostgresRunEventWaitExactNanosecondBoundariesAndIndexedNotification(t *testing.T) {
	store, _ := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second).Add(417 * time.Nanosecond)
	scope := Scope{Kind: "tenant", ID: "nanosecond-boundaries"}
	spec := RunEventWaitSpec{Key: "exact", Type: "message.received", Source: "host:chat", Subject: "thread", After: now, Deadline: now.Add(time.Minute), Attributes: map[string]interface{}{"sender": "right", "enabled": true, "large": json.Number("9007199254740993")}}
	run := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	before := postgresRunEventWaitTestReceipt(scope, spec, "too-early", now.Add(-time.Nanosecond))
	late := postgresRunEventWaitTestReceipt(scope, spec, "too-late", now)
	late.ReceivedAt = spec.Deadline.Add(time.Nanosecond)
	wrong := postgresRunEventWaitTestReceipt(scope, spec, "wrong-type", now)
	wrong.Event.Attributes["enabled"] = "true"
	wrong = postgresRunEventWaitTestRedigest(wrong)
	for _, receipt := range []*RunEventReceipt{before, late, wrong} {
		if _, err := store.PublishRunEvent(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
	claim := postgresRunEventWaitTestClaim(t, store, scope, "initial", now, 1)
	idle, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now})
	if err != nil || idle.Run != nil || idle.Wait.Status != RunEventWaitPending {
		t.Fatalf("sub-microsecond false match: result=%#v err=%v", idle, err)
	}
	postgresRunEventWaitTestDrainUnmatchedNotifications(t, store, scope, now.Add(time.Second))
	// While idle, an unrelated subject must not make this tenant discoverable.
	unrelated := postgresRunEventWaitTestReceipt(scope, spec, "other-thread", now.Add(time.Second))
	unrelated.Event.Subject = "another-thread"
	unrelated = postgresRunEventWaitTestRedigest(unrelated)
	if _, err := store.PublishRunEvent(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	postgresRunEventWaitTestDrainUnmatchedNotifications(t, store, scope, now.Add(time.Second))
	if scopes, err := store.ListRunEventWaitWorkScopes(ctx, now.Add(time.Second), 100); err != nil || len(scopes) != 0 {
		t.Fatalf("unrelated notification scheduled tenant: scopes=%v err=%v", scopes, err)
	}
	exact := postgresRunEventWaitTestReceipt(scope, spec, "exact-match", now)
	if _, err := store.PublishRunEvent(ctx, exact); err != nil {
		t.Fatal(err)
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(ctx, now.Add(time.Second), 100); err != nil || len(scopes) != 1 || scopes[0] != scope {
		t.Fatalf("matching notification did not schedule tenant: scopes=%v err=%v", scopes, err)
	}
	claim = postgresRunEventWaitTestClaim(t, store, scope, "exact-worker", now.Add(time.Second), 1)
	if matched, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now.Add(time.Second)}); err != nil || matched.Wait.Status != RunEventWaitMatched || matched.Event.ID != exact.Event.ID {
		t.Fatalf("exact boundary result=%#v err=%v", matched, err)
	}
}

func TestPostgresRunEventWaitPublicationRacingIdleProcessorDoesNotLoseNotification(t *testing.T) {
	store, publisher := newPostgresRunEventWaitTestStores(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "racing-notification"}
	spec := RunEventWaitSpec{Key: "reply", Type: "message.received", Source: "host:chat", Subject: "race-thread", After: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	run := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	claim := postgresRunEventWaitTestClaim(t, store, scope, "processor", now, 1)
	gate, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	lockKey := int64(crc32.ChecksumIEEE([]byte(store.schema)) & 0x7fffffff)
	if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	defer gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	// This private-schema trigger supplies a deterministic barrier after the
	// processor's no-match snapshot and before its idle deadline update commits.
	// The production store has no synchronization hooks or test-only branches.
	if _, err := store.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$;
		CREATE TRIGGER idle_processor_gate BEFORE UPDATE OF available_at ON %s
		FOR EACH ROW WHEN (NEW.available_at IS DISTINCT FROM OLD.available_at)
		EXECUTE FUNCTION %s()`, store.table("idle_processor_gate"), lockKey, store.table("run_event_waits"), store.table("idle_processor_gate"))); err != nil {
		t.Fatal(err)
	}
	processed := make(chan *RunEventWaitResult, 1)
	processErrors := make(chan error, 1)
	go func() {
		result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now})
		processed <- result
		processErrors <- err
	}()
	var processorPID int
	postgresRunEventWaitTestAwait(t, ctx, func() bool {
		return gate.QueryRowContext(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted`, lockKey).Scan(&processorPID) == nil
	})
	if _, err := publisher.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, spec, "racing-event", now)); err != nil {
		t.Fatal(err)
	}
	fannedOut := make(chan []*RunEventWait, 1)
	fanoutErrors := make(chan error, 1)
	go func() {
		waits, err := publisher.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "fanout", Now: now, LeaseDuration: time.Minute, Limit: 1})
		fannedOut <- waits
		fanoutErrors <- err
	}()
	postgresRunEventWaitTestAwait(t, ctx, func() bool {
		var blocked bool
		err := gate.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, processorPID).Scan(&blocked)
		return err == nil && blocked
	})
	if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	if err := <-processErrors; err != nil {
		t.Fatal(err)
	}
	if result := <-processed; result.Run != nil || result.Wait.AvailableAt == nil || !result.Wait.AvailableAt.Equal(spec.Deadline) {
		t.Fatalf("processor did not idle at barrier: %#v", result)
	}
	if err := <-fanoutErrors; err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER idle_processor_gate ON `+store.table("run_event_waits")); err != nil {
		t.Fatal(err)
	}
	waits := <-fannedOut
	if len(waits) != 1 || waits[0].RunID != run.ID {
		t.Fatalf("publication notification lost behind idle processor: waits=%v", waits)
	}
	claim = waits[0]
	if result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now.Add(time.Second)}); err != nil || result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("racing event failed to resolve: result=%#v err=%v", result, err)
	}
}

func postgresRunEventWaitTestAwait(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatalf("database synchronization barrier: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func postgresRunEventWaitTestDrainUnmatchedNotifications(t *testing.T, store *PostgresStore, scope Scope, now time.Time) {
	t.Helper()
	for attempts := 0; attempts < 100; attempts++ {
		var pending int
		if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+store.table("run_event_notifications")+`
			WHERE scope_kind=$1 AND scope_id=$2 AND available_at <= $3`, scope.Kind, scope.ID, now).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "unmatched-notification", Now: now, LeaseDuration: time.Minute, Limit: 100})
		if err != nil || len(waits) != 0 {
			t.Fatalf("unmatched notification woke an execution: claims=%d err=%v", len(waits), err)
		}
	}
	t.Fatal("notification drain did not finish")
}

func TestPostgresRunEventWaitPauseCancelTimeoutAndAtomicFailure(t *testing.T) {
	store, _ := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "lifecycle"}
	spec := RunEventWaitSpec{Key: "reply", Type: "message.received", Source: "host:chat", Subject: "thread-1", After: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	run := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	claim := postgresRunEventWaitTestClaim(t, store, scope, "pause-worker", now, 1)
	paused := cloneAgentRun(run)
	paused.Status = AgentRunStatusPaused
	paused.PausedFrom = run.Status
	paused.PausedWakeCondition = paused.WakeCondition
	paused.WakeCondition = nil
	paused.Revision++
	paused.UpdatedAt = now
	postgresRunEventWaitTestUpdateRun(t, store, paused, run.Revision)
	if _, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("paused wait lease not revoked: %v", err)
	}
	if _, err := store.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, spec, "during-pause", now)); err != nil {
		t.Fatal(err)
	}
	if waits, err := store.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "paused", Now: now, LeaseDuration: time.Minute, Limit: 100}); err != nil || len(waits) != 0 {
		t.Fatalf("paused wait woke: claims=%d err=%v", len(waits), err)
	}
	resumed := cloneAgentRun(paused)
	resumed.Status = AgentRunStatusWaitingForEvent
	resumed.WakeCondition = resumed.PausedWakeCondition
	resumed.PausedWakeCondition = nil
	resumed.PausedFrom = ""
	resumed.Revision++
	postgresRunEventWaitTestUpdateRun(t, store, resumed, paused.Revision)
	claim = postgresRunEventWaitTestClaim(t, store, scope, "resume-worker", now, 1)
	// Simulate activity storage failing after the event was consumed and the
	// Run changed. The entire transaction must roll back, including the inbox
	// consumption, so a retry can still resolve this exact event.
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE `+store.table("run_activity")+` ADD CONSTRAINT fail_event_resolution CHECK (event_type <> 'run.event_wait_resolved')`); err != nil {
		t.Fatal(err)
	}
	request := ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}
	if _, err := store.ProcessRunEventWait(ctx, request); err == nil {
		t.Fatal("expected activity write failure")
	}
	unchanged, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || unchanged.Revision != resumed.Revision || unchanged.Status != AgentRunStatusWaitingForEvent {
		t.Fatalf("partial Run update survived rollback: run=%#v err=%v", unchanged, err)
	}
	var consumed int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+store.table("run_event_consumptions")+` WHERE run_id=$1`, run.ID).Scan(&consumed); err != nil || consumed != 0 {
		t.Fatalf("partial consumption survived rollback: count=%d err=%v", consumed, err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE `+store.table("run_activity")+` DROP CONSTRAINT fail_event_resolution`); err != nil {
		t.Fatal(err)
	}
	if result, err := store.ProcessRunEventWait(ctx, request); err != nil || result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("resume result=%#v err=%v", result, err)
	}
	// Timeout produces a saved outcome and queues the same Run.
	spec.Key, spec.Subject = "deadline", "missing-thread"
	deadlineRun := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	claim = postgresRunEventWaitTestClaim(t, store, scope, "deadline-worker", now, 1)
	if idle, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: deadlineRun.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}); err != nil || idle.Run != nil {
		t.Fatalf("initial no-match result=%#v err=%v", idle, err)
	}
	claim = postgresRunEventWaitTestClaim(t, store, scope, "timeout-worker", spec.Deadline, 1)
	if result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: deadlineRun.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: spec.Deadline}); err != nil || result.Wait.Status != RunEventWaitTimedOut || result.Run.ID != deadlineRun.ID || result.Run.Status != AgentRunStatusQueued {
		t.Fatalf("timeout result=%#v err=%v", result, err)
	}
	// Cancellation clears pending readiness and fences a previously held lease.
	spec.Key, spec.Subject = "cancel", "cancel-thread"
	cancelRun := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	claim = postgresRunEventWaitTestClaim(t, store, scope, "cancel-worker", now, 1)
	canceled := cloneAgentRun(cancelRun)
	canceled.Status = AgentRunStatusCanceled
	canceled.WakeCondition = nil
	canceled.Revision++
	postgresRunEventWaitTestUpdateRun(t, store, canceled, cancelRun.Revision)
	if _, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: cancelRun.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("cancel lease error=%v", err)
	}
	if wait, err := store.GetRunEventWait(ctx, scope, cancelRun.ID, spec.Key); err != nil || wait.Status != RunEventWaitCanceled {
		t.Fatalf("canceled projection=%#v err=%v", wait, err)
	}
}

func TestPostgresRunEventWaitImmutableIdentityRetentionAndMigration(t *testing.T) {
	store, _ := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "retention"}
	old := now.Add(-40 * 24 * time.Hour)
	spec := RunEventWaitSpec{Key: "old", Type: "message.received", Source: "host:mail", Subject: "keep", After: old.Add(-time.Hour), Deadline: old.Add(time.Hour)}
	run := postgresRunEventWaitTestCreateRun(t, store, scope, spec, old)
	changed := cloneAgentRun(run)
	changed.WakeCondition.EventWait.Subject = "changed"
	changed.Revision++
	if _, err := store.UpdateAgentRunWithEvent(ctx, changed, run.Revision, postgresRunEventWaitTestActivity(changed), nil); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("reused wait key accepted: err=%v", err)
	}
	retained, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || retained.Revision != run.Revision || retained.WakeCondition.EventWait.Subject != spec.Subject {
		t.Fatalf("immutable wait failed atomically: run=%#v err=%v", retained, err)
	}
	if _, err := store.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, spec, "protected", old)); err != nil {
		t.Fatal(err)
	}
	unmatched := spec
	unmatched.Subject = "prune"
	for _, id := range []string{"expired-1", "expired-2"} {
		if _, err := store.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, unmatched, id, old)); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.PruneRunEvents(ctx, scope, now, 1); err != nil || count != 1 {
		t.Fatalf("bounded prune=%d err=%v", count, err)
	}
	if count, err := store.PruneRunEvents(ctx, scope, now, 100); err != nil || count != 1 {
		t.Fatalf("protected prune=%d err=%v", count, err)
	}
	claim := postgresRunEventWaitTestClaim(t, store, scope, "delayed-worker", now, 1)
	if result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: now}); err != nil || result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("timely buffered event lost to retention: result=%#v err=%v", result, err)
	}
	if count, err := store.PruneRunEvents(ctx, scope, now, 100); err != nil || count != 1 {
		t.Fatalf("resolved event prune=%d err=%v", count, err)
	}
	if err := store.RollbackPostgresMigrations(ctx, 46); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != 46 {
		t.Fatalf("rollback version=%d err=%v", version, err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("remigration version=%d err=%v", version, err)
	}
}

func TestPostgresRunEventWaitGlobalRetentionCursorPassesProtectedEvents(t *testing.T) {
	store, _ := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := now.Add(-40 * 24 * time.Hour)
	scope := Scope{Kind: "tenant", ID: "bounded-global-retention"}
	spec := RunEventWaitSpec{Key: "protected", Type: "message.received", Source: "host:mail", Subject: "pending", After: old.Add(-time.Hour), Deadline: old.Add(time.Hour)}
	postgresRunEventWaitTestCreateRun(t, store, scope, spec, old)
	if _, err := store.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, spec, "a-protected", old)); err != nil {
		t.Fatal(err)
	}
	spec.Subject = "expired"
	for _, id := range []string{"b-expired", "c-expired"} {
		if _, err := store.PublishRunEvent(ctx, postgresRunEventWaitTestReceipt(scope, spec, id, old)); err != nil {
			t.Fatal(err)
		}
	}
	if deleted, err := store.PruneRunEvents(ctx, scope, now, 1); err != nil || deleted != 0 {
		t.Fatalf("scoped retention read beyond its protected candidate page: deleted=%d err=%v", deleted, err)
	}
	// Every batch examines at most one candidate. A protected oldest event
	// cannot starve unrelated expired records further along the durable cursor.
	for index, want := range []int{0, 1, 1, 0} {
		got, err := store.PruneExpiredRunEvents(ctx, now.Add(-RunEventRetention), 1)
		if err != nil || got != want {
			t.Fatalf("retention batch %d: deleted=%d want=%d err=%v", index, got, want, err)
		}
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+store.table("run_event_inbox")).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("protected event retention: remaining=%d err=%v", remaining, err)
	}
}

func newPostgresRunEventWaitTestStores(t *testing.T) (*PostgresStore, *PostgresStore) {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := "openseal_event_wait_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replica.Close() })
	return primary, replica
}

func postgresRunEventWaitTestCreateRun(t *testing.T, store *PostgresStore, scope Scope, spec RunEventWaitSpec, now time.Time) *AgentRun {
	t.Helper()
	id := uuid.NewString()
	run := &AgentRun{ID: id, RootRunID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Continue after a correlated event", Source: RunSourceManual, Status: AgentRunStatusWaitingForEvent, Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now, WakeCondition: &WakeCondition{Type: "event", EventWait: cloneRunEventWaitSpec(&spec)}}
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func postgresRunEventWaitTestUpdateRun(t *testing.T, store *PostgresStore, run *AgentRun, expected int64) {
	t.Helper()
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expected, postgresRunEventWaitTestActivity(run), nil); err != nil {
		t.Fatal(err)
	}
}

func postgresRunEventWaitTestActivity(run *AgentRun) *ActivityEvent {
	return &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, EventType: "run.test_transition", Severity: ActivitySeverityInfo, Visibility: ActivityVisibilityScope, Actor: ActivityActor{Type: "system", ID: "test"}, Summary: "Test authoritative run transition", CreatedAt: run.UpdatedAt}
}

func postgresRunEventWaitTestClaim(t *testing.T, store *PostgresStore, scope Scope, worker string, now time.Time, limit int) *RunEventWait {
	t.Helper()
	waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, Limit: limit})
	if err != nil || len(waits) != 1 {
		t.Fatalf("claim count=%d err=%v", len(waits), err)
	}
	return waits[0]
}

func postgresRunEventWaitTestReceipt(scope Scope, spec RunEventWaitSpec, id string, at time.Time) *RunEventReceipt {
	return postgresRunEventWaitTestRedigest(&RunEventReceipt{Event: EventEnvelope{ID: id, Scope: scope, Type: spec.Type, Source: spec.Source, Subject: spec.Subject, OccurredAt: at, Attributes: cloneMap(spec.Attributes)}, ReceivedAt: at})
}

func postgresRunEventWaitTestRedigest(receipt *RunEventReceipt) *RunEventReceipt {
	receipt.Digest, _ = runEventObservationDigest(receipt.Event)
	return receipt
}
