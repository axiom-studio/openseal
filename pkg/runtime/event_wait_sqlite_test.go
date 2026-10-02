package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var sqliteEventWaitNow = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

func newSQLiteEventWaitTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "event-waits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sqliteEventWaitTestRun(id string, attributes map[string]interface{}) *AgentRun {
	return &AgentRun{
		ID: id, Scope: Scope{Kind: "tenant", ID: "event-waits"}, RootRunID: id,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Wait for a verified external response", Source: RunSourceManual,
		Status: AgentRunStatusWaitingForEvent, Revision: 1,
		CreatedAt: sqliteEventWaitNow, UpdatedAt: sqliteEventWaitNow,
		AvailableAt: sqliteEventWaitNow, QueueEnteredAt: sqliteEventWaitNow,
		WakeCondition: &WakeCondition{Type: "event", EventWait: &RunEventWaitSpec{
			Key: "reply", Type: "conversation.message.received", Source: "test:connection", Subject: "thread:123",
			Attributes: attributes, After: sqliteEventWaitNow.Add(-time.Minute), Deadline: sqliteEventWaitNow.Add(time.Hour),
		}},
	}
}

func sqliteEventWaitTestReceipt(run *AgentRun, id string, attributes map[string]interface{}, received time.Time) *RunEventReceipt {
	event := EventEnvelope{
		ID: id, Scope: run.Scope, Source: run.WakeCondition.EventWait.Source, Type: run.WakeCondition.EventWait.Type,
		Subject: run.WakeCondition.EventWait.Subject, Attributes: attributes, OccurredAt: sqliteEventWaitNow,
		Payload: map[string]interface{}{"text": "Here is the requested response"},
	}
	data, _ := json.Marshal(event)
	digest := sha256.Sum256(data)
	return &RunEventReceipt{Event: event, ReceivedAt: received, Digest: hex.EncodeToString(digest[:])}
}

func claimSQLiteEventWait(t *testing.T, store *SQLiteStore, scope Scope, worker string, now time.Time, want int) []*RunEventWait {
	t.Helper()
	waits, err := store.ClaimRunEventWaits(context.Background(), ClaimRunEventWaitsRequest{
		Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, Limit: 100,
	})
	if err != nil || len(waits) != want {
		t.Fatalf("claim waits=%#v error=%v, want %d", waits, err, want)
	}
	return waits
}

func processSQLiteEventWait(t *testing.T, store *SQLiteStore, run *AgentRun, key, worker string, now time.Time) *RunEventWaitResult {
	t.Helper()
	wait, err := store.GetRunEventWait(context.Background(), run.Scope, run.ID, key)
	if err != nil || wait.LeaseExpiresAt == nil {
		t.Fatalf("expected claimed wait=%#v error=%v", wait, err)
	}
	result, err := store.ProcessRunEventWait(context.Background(), ProcessRunEventWaitRequest{
		Scope: run.Scope, RunID: run.ID, Key: key, WorkerID: worker, Now: now, LeaseExpiresAt: *wait.LeaseExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func updateSQLiteEventWaitTestRun(t *testing.T, store *SQLiteStore, run *AgentRun) {
	t.Helper()
	expected := run.Revision
	run.Revision++
	run.UpdatedAt = run.UpdatedAt.Add(time.Second)
	_, err := store.UpdateAgentRunWithEvent(context.Background(), run, expected, &ActivityEvent{
		ID: fmt.Sprintf("transition:%s:%d", run.ID, run.Revision), Scope: run.Scope, RunID: run.ID,
		EventType: "run.transitioned", Summary: "Updated wait lifecycle", CreatedAt: run.UpdatedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteRunEventWaitBufferedRestartAndLeaseFencing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	run := sqliteEventWaitTestRun("restart", map[string]interface{}{"sender": "person"})
	receipt := sqliteEventWaitTestReceipt(run, "early-response", map[string]interface{}{"sender": "person"}, sqliteEventWaitNow)
	if inserted, err := store.PublishRunEvent(ctx, receipt); err != nil || !inserted {
		t.Fatalf("publish=%v error=%v", inserted, err)
	}
	if inserted, err := store.PublishRunEvent(ctx, receipt); err != nil || inserted {
		t.Fatalf("duplicate publish=%v error=%v", inserted, err)
	}
	conflict := sqliteEventWaitTestReceipt(run, receipt.Event.ID, map[string]interface{}{"sender": "different"}, sqliteEventWaitNow)
	if _, err := store.PublishRunEvent(ctx, conflict); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("identity collision error=%v", err)
	}
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker-before-restart", sqliteEventWaitNow, 1)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claimSQLiteEventWait(t, store, run.Scope, "worker-after-restart", sqliteEventWaitNow.Add(time.Minute), 1)
	request := ProcessRunEventWaitRequest{Scope: run.Scope, RunID: run.ID, Key: "reply", WorkerID: "worker-before-restart", Now: sqliteEventWaitNow.Add(time.Minute), LeaseExpiresAt: sqliteEventWaitNow.Add(time.Minute)}
	if _, err := store.ProcessRunEventWait(ctx, request); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker error=%v", err)
	}
	result := processSQLiteEventWait(t, store, run, "reply", "worker-after-restart", sqliteEventWaitNow.Add(time.Minute))
	if result.Wait.Status != RunEventWaitMatched || result.Run.Status != AgentRunStatusQueued || result.Event.ID != receipt.Event.ID || result.Activity.Sequence != 1 || result.Run.Revision != 2 {
		t.Fatalf("resolution=%#v", result)
	}
	if _, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: run.Scope, RunID: run.ID, Key: "reply", WorkerID: "worker-after-restart", Now: sqliteEventWaitNow.Add(time.Minute), LeaseExpiresAt: sqliteEventWaitNow.Add(2 * time.Minute)}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("double resolution error=%v", err)
	}
	if _, err := store.GetRunEventWait(ctx, Scope{Kind: "tenant", ID: "other"}, run.ID, "reply"); !errors.Is(err, ErrRunEventWaitNotFound) {
		t.Fatalf("cross-tenant wait error=%v", err)
	}
	stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || stored.Checkpoint[runEventWaitCheckpointKey] == nil || stored.Status != AgentRunStatusQueued {
		t.Fatalf("stored run=%#v error=%v", stored, err)
	}
}

func TestSQLiteRunEventWaitIdleDeadlineAndPublicationDuringLease(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("idle", map[string]interface{}{"sender": "person", "approved": true, "count": 1})
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow)
	if result.Run != nil || result.Wait.Status != RunEventWaitPending || !result.Wait.AvailableAt.Equal(run.WakeCondition.EventWait.Deadline) {
		t.Fatalf("idle resolution=%#v", result)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 0)
	wrong := sqliteEventWaitTestReceipt(run, "wrong-scalar", map[string]interface{}{"sender": "person", "approved": 1, "count": 1}, sqliteEventWaitNow.Add(time.Second))
	if _, err := store.PublishRunEvent(context.Background(), wrong); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 0)
	correct := sqliteEventWaitTestReceipt(run, "correct", map[string]interface{}{"sender": "person", "approved": true, "count": 1.0}, sqliteEventWaitNow.Add(2*time.Second))
	if _, err := store.PublishRunEvent(context.Background(), correct); err != nil {
		t.Fatal(err)
	}
	claims := claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(2*time.Second), 1)
	another := sqliteEventWaitTestReceipt(run, "another", correct.Event.Attributes, sqliteEventWaitNow.Add(3*time.Second))
	if _, err := store.PublishRunEvent(context.Background(), another); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetRunEventWait(context.Background(), run.Scope, run.ID, "reply")
	if err != nil || saved.LeaseOwner != "worker" || !saved.LeaseExpiresAt.Equal(*claims[0].LeaseExpiresAt) {
		t.Fatalf("publish stole lease: wait=%#v error=%v", saved, err)
	}
	result = processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(3*time.Second))
	if result.Wait.Status != RunEventWaitMatched || result.Event.ID != "correct" {
		t.Fatalf("resolution=%#v", result)
	}
}

func TestSQLiteRunEventWaitPauseResumeCancellationAndConsumption(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("lifecycle", nil)
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	run.Status, run.PausedWakeCondition, run.WakeCondition = AgentRunStatusPaused, run.WakeCondition, nil
	updateSQLiteEventWaitTestRun(t, store, run)
	wait, err := store.GetRunEventWait(context.Background(), run.Scope, run.ID, "reply")
	if err != nil || wait.Status != RunEventWaitPaused || wait.AvailableAt != nil || wait.LeaseOwner != "" {
		t.Fatalf("paused=%#v error=%v", wait, err)
	}
	run.Status, run.WakeCondition, run.PausedWakeCondition = AgentRunStatusWaitingForEvent, run.PausedWakeCondition, nil
	receipt := sqliteEventWaitTestReceipt(run, "buffered-while-paused", nil, sqliteEventWaitNow.Add(2*time.Second))
	if _, err := store.PublishRunEvent(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(2*time.Second), 0)
	updateSQLiteEventWaitTestRun(t, store, run)
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(2*time.Second), 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(2*time.Second))
	if result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("resume resolution=%#v", result)
	}
	run = result.Run
	run.Status = AgentRunStatusWaitingForEvent
	run.WakeCondition = &WakeCondition{Type: "event", EventWait: cloneRunEventWaitSpec(&wait.Spec)}
	run.WakeCondition.EventWait.Key = "second-reply"
	updateSQLiteEventWaitTestRun(t, store, run)
	claimSQLiteEventWait(t, store, run.Scope, "worker", run.UpdatedAt, 1)
	result = processSQLiteEventWait(t, store, run, "second-reply", "worker", run.UpdatedAt)
	if result.Run != nil || result.Wait.Status != RunEventWaitPending {
		t.Fatalf("same event consumed twice: %#v", result)
	}
	run.Status, run.WakeCondition = AgentRunStatusCanceled, nil
	updateSQLiteEventWaitTestRun(t, store, run)
	wait, err = store.GetRunEventWait(context.Background(), run.Scope, run.ID, "second-reply")
	if err != nil || wait.Status != RunEventWaitCanceled {
		t.Fatalf("canceled=%#v error=%v", wait, err)
	}
}

func TestSQLiteRunEventWaitImmutableKeysAndAtomicRegistration(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("immutable", nil)
	if _, err := store.CreateAgentRunWithEvent(ctx, run, &ActivityEvent{
		ID: "created", Scope: run.Scope, RunID: run.ID, EventType: "run.created", Summary: "Run created", CreatedAt: run.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	changed := cloneAgentRun(run)
	changed.Revision++
	changed.WakeCondition.EventWait.Subject = "different-thread"
	_, err := store.UpdateAgentRunWithEvent(ctx, changed, run.Revision, &ActivityEvent{
		ID: "invalid-update", Scope: run.Scope, RunID: run.ID, EventType: "run.transitioned", Summary: "Attempt to reuse key", CreatedAt: run.UpdatedAt,
	}, nil)
	if !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("changed wait key error=%v", err)
	}
	saved, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || saved.Revision != run.Revision || saved.WakeCondition.EventWait.Subject != "thread:123" {
		t.Fatalf("failed registration changed run: %#v error=%v", saved, err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", run.WakeCondition.EventWait.Deadline, 1)
	resolved := processSQLiteEventWait(t, store, run, "reply", "worker", run.WakeCondition.EventWait.Deadline)
	if resolved.Wait.Status != RunEventWaitTimedOut {
		t.Fatalf("timeout result=%#v", resolved)
	}
	changed = resolved.Run
	changed.Status = AgentRunStatusWaitingForEvent
	changed.WakeCondition = cloneWakeCondition(run.WakeCondition)
	changed.Revision++
	_, err = store.UpdateAgentRunWithEvent(ctx, changed, changed.Revision-1, &ActivityEvent{
		ID: "resolved-key-update", Scope: run.Scope, RunID: run.ID, EventType: "run.transitioned", Summary: "Attempt to reuse completed key", CreatedAt: run.UpdatedAt,
	}, nil)
	if !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("resolved key reuse error=%v", err)
	}
	invalid := sqliteEventWaitTestRun("invalid-create", nil)
	invalid.WakeCondition.EventWait.Deadline = invalid.WakeCondition.EventWait.After
	if err := store.CreateAgentRun(ctx, invalid); !errors.Is(err, ErrInvalidRunEventWait) {
		t.Fatalf("invalid wait error=%v", err)
	}
	if saved, err := store.GetAgentRun(ctx, invalid.Scope, invalid.ID); err != nil || saved != nil {
		t.Fatalf("invalid create persisted run=%#v error=%v", saved, err)
	}
	rolledBack := sqliteEventWaitTestRun("activity-failure", nil)
	if _, err := store.CreateAgentRunWithEvent(ctx, rolledBack, &ActivityEvent{
		ID: "created", Scope: run.Scope, RunID: rolledBack.ID, EventType: "run.created", Summary: "Force duplicate activity failure", CreatedAt: run.CreatedAt,
	}); err == nil {
		t.Fatal("expected duplicate activity failure")
	}
	if saved, err := store.GetAgentRun(ctx, rolledBack.Scope, rolledBack.ID); err != nil || saved != nil {
		t.Fatalf("failed activity persisted run=%#v error=%v", saved, err)
	}
	if _, err := store.GetRunEventWait(ctx, rolledBack.Scope, rolledBack.ID, "reply"); !errors.Is(err, ErrRunEventWaitNotFound) {
		t.Fatalf("failed activity persisted wait, error=%v", err)
	}
}

func TestSQLiteRunEventWaitConcurrentClaimsAreExclusive(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("exclusive", nil)
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	const workers = 12
	var group sync.WaitGroup
	results := make(chan []*RunEventWait, workers)
	errorsSeen := make(chan error, workers)
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			waits, err := store.ClaimRunEventWaits(context.Background(), ClaimRunEventWaitsRequest{
				Scope: run.Scope, WorkerID: fmt.Sprintf("worker-%d", i), Now: sqliteEventWaitNow, LeaseDuration: time.Minute, Limit: 1,
			})
			results <- waits
			errorsSeen <- err
		}(i)
	}
	group.Wait()
	close(results)
	close(errorsSeen)
	claims := 0
	for waits := range results {
		claims += len(waits)
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if claims != 1 {
		t.Fatalf("claims=%d, want 1", claims)
	}
}

func TestSQLiteRunEventWaitExactNumericScalars(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	attributes := map[string]interface{}{"count": json.Number("123456789012345678901234567890.1")}
	run := sqliteEventWaitTestRun("numeric", attributes)
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow)
	wrong := sqliteEventWaitTestReceipt(run, "rounded-collision", map[string]interface{}{"count": json.Number("123456789012345678901234567890.2")}, sqliteEventWaitNow.Add(time.Second))
	if _, err := store.PublishRunEvent(context.Background(), wrong); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 0)
	correct := sqliteEventWaitTestReceipt(run, "exact-number", map[string]interface{}{"count": json.Number("123456789012345678901234567890.10")}, sqliteEventWaitNow.Add(2*time.Second))
	if _, err := store.PublishRunEvent(context.Background(), correct); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(2*time.Second), 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(2*time.Second))
	if result.Run == nil || result.Wait.Status != RunEventWaitMatched || result.Event.ID != correct.Event.ID {
		t.Fatalf("exact numeric resolution=%#v", result)
	}
}

func TestSQLiteRunEventWaitPruneRetainsNeededEventsAndConsumption(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("retention", nil)
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	needed := sqliteEventWaitTestReceipt(run, "needed-buffer", nil, sqliteEventWaitNow)
	if _, err := store.PublishRunEvent(ctx, needed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		unrelated := sqliteEventWaitTestReceipt(run, fmt.Sprintf("unrelated-%d", i), nil, sqliteEventWaitNow)
		unrelated.Event.Subject = "other-thread"
		data, _ := json.Marshal(unrelated.Event)
		digest := sha256.Sum256(data)
		unrelated.Digest = hex.EncodeToString(digest[:])
		if _, err := store.PublishRunEvent(ctx, unrelated); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.PruneRunEvents(ctx, run.Scope, sqliteEventWaitNow.Add(RunEventRetention), 10); err != nil || count != 0 {
		t.Fatalf("premature prune=%d error=%v", count, err)
	}
	if count, err := store.PruneRunEvents(ctx, run.Scope, sqliteEventWaitNow.Add(RunEventRetention+time.Second), 2); err != nil || count != 1 {
		t.Fatalf("bounded prune=%d error=%v", count, err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(RunEventRetention+time.Second), 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(RunEventRetention+time.Second))
	if result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("timely buffered receipt lost: %#v", result)
	}
	if count, err := store.PruneRunEvents(ctx, run.Scope, sqliteEventWaitNow.Add(RunEventRetention+time.Second), 10); err != nil || count != 2 {
		t.Fatalf("remaining prune=%d error=%v", count, err)
	}
	if count, err := store.PruneRunEvents(ctx, run.Scope, sqliteEventWaitNow.Add(RunEventRetention+time.Second), 10); err != nil || count != 1 {
		t.Fatalf("cursor wrap prune=%d error=%v", count, err)
	}
	if inserted, err := store.PublishRunEvent(ctx, needed); err != nil || !inserted {
		t.Fatalf("republish retained identity=%v error=%v", inserted, err)
	}
	run = result.Run
	run.Status = AgentRunStatusWaitingForEvent
	run.WakeCondition = &WakeCondition{Type: "event", EventWait: cloneRunEventWaitSpec(&result.Wait.Spec)}
	run.WakeCondition.EventWait.Key = "new-request"
	updateSQLiteEventWaitTestRun(t, store, run)
	claimSQLiteEventWait(t, store, run.Scope, "worker", run.UpdatedAt, 1)
	result = processSQLiteEventWait(t, store, run, "new-request", "worker", run.UpdatedAt)
	if result.Wait.Status != RunEventWaitTimedOut || result.Event != nil {
		t.Fatalf("pruning allowed consumption replay: %#v", result)
	}
}

func TestSQLiteRunEventWaitResolutionRollsBackEveryWrite(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("resolution-failure", nil)
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishRunEvent(ctx, sqliteEventWaitTestReceipt(run, "response", nil, sqliteEventWaitNow)); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_wait_activity BEFORE INSERT ON run_activity
		WHEN NEW.event_type='run.event_wait_resolved' BEGIN SELECT RAISE(ABORT,'injected activity failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{
		Scope: run.Scope, RunID: run.ID, Key: "reply", WorkerID: "worker", Now: sqliteEventWaitNow, LeaseExpiresAt: sqliteEventWaitNow.Add(time.Minute),
	}); err == nil {
		t.Fatal("expected injected resolution failure")
	}
	stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || stored.Revision != run.Revision || stored.Status != AgentRunStatusWaitingForEvent || stored.Checkpoint[runEventWaitCheckpointKey] != nil {
		t.Fatalf("failure changed run: %#v error=%v", stored, err)
	}
	wait, err := store.GetRunEventWait(ctx, run.Scope, run.ID, "reply")
	if err != nil || wait.Status != RunEventWaitPending || wait.LeaseOwner != "worker" {
		t.Fatalf("failure changed wait: %#v error=%v", wait, err)
	}
	var consumed int
	if err := store.db.QueryRow(`SELECT count(*) FROM run_event_consumptions WHERE run_id=?`, run.ID).Scan(&consumed); err != nil || consumed != 0 {
		t.Fatalf("failure consumed event: count=%d error=%v", consumed, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_wait_activity`); err != nil {
		t.Fatal(err)
	}
	if result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow); result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("retry result=%#v", result)
	}
}

func TestSQLiteRunEventWaitScopeQueueFairnessAndLeases(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	const tenantCount = 4
	for i := 0; i < tenantCount; i++ {
		run := sqliteEventWaitTestRun(fmt.Sprintf("queued-%d", i), nil)
		run.Scope.ID = fmt.Sprintf("tenant-%d", i)
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[Scope]bool)
	for i := 0; i < tenantCount; i++ {
		scopes, err := store.ListRunEventWaitWorkScopes(ctx, sqliteEventWaitNow, 1)
		if err != nil || len(scopes) != 1 || seen[scopes[0]] {
			t.Fatalf("scope discovery=%#v error=%v", scopes, err)
		}
		seen[scopes[0]] = true
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(ctx, sqliteEventWaitNow, 1); err != nil || len(scopes) != 0 {
		t.Fatalf("scope fairness=%#v error=%v", scopes, err)
	}
	run := sqliteEventWaitTestRun("queued-0", nil)
	run.Scope.ID = "tenant-0"
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	scopes, err := store.ListRunEventWaitWorkScopes(ctx, sqliteEventWaitNow.Add(time.Second), tenantCount)
	if err != nil || len(scopes) != tenantCount-1 {
		t.Fatalf("held lease kept scope due: %#v error=%v", scopes, err)
	}
	if _, err := store.PublishRunEvent(ctx, sqliteEventWaitTestReceipt(run, "leased-response", nil, sqliteEventWaitNow.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(ctx, sqliteEventWaitNow.Add(time.Second), tenantCount); err != nil || len(scopes) != 1 || scopes[0] != run.Scope {
		t.Fatalf("publication did not queue notification work: %#v error=%v", scopes, err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "other-worker", sqliteEventWaitNow.Add(time.Second), 0)
	processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(time.Second))
	var queue sql.NullTime
	if err := store.db.QueryRow(`SELECT available_at FROM run_event_wait_scopes WHERE scope_kind=? AND scope_id=?`, run.Scope.Kind, run.Scope.ID).Scan(&queue); err != nil || queue.Valid {
		t.Fatalf("resolved scope remains queued: %#v error=%v", queue, err)
	}
}

func TestSQLiteRunEventWaitNanosecondAndTimezoneBoundaries(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("precision", nil)
	run.WakeCondition.EventWait.After = sqliteEventWaitNow.Add(time.Nanosecond).In(time.FixedZone("offset", 5*3600+30*60))
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
	processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow)
	before := sqliteEventWaitTestReceipt(run, "one-nanosecond-before", nil, sqliteEventWaitNow)
	if _, err := store.PublishRunEvent(ctx, before); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 0)
	exact := sqliteEventWaitTestReceipt(run, "exact-boundary", nil, sqliteEventWaitNow.Add(time.Second))
	exact.Event.OccurredAt = run.WakeCondition.EventWait.After.UTC()
	data, _ := json.Marshal(exact.Event)
	digest := sha256.Sum256(data)
	exact.Digest = hex.EncodeToString(digest[:])
	if _, err := store.PublishRunEvent(ctx, exact); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(time.Second))
	if result.Wait.Status != RunEventWaitMatched || result.Event.ID != exact.Event.ID {
		t.Fatalf("precision boundary result=%#v", result)
	}
}

func TestSQLiteRunEventWaitBindingDeploymentIsolation(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("binding-owner", map[string]interface{}{"sender": "person"})
	run.WakeCondition.EventWait.Source = RunEventBindingSource(run.AssignedAgentID, "binding", "adapter")
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	foreign := cloneAgentRun(run)
	foreign.ID, foreign.RootRunID, foreign.AssignedAgentID = "foreign-agent", "foreign-agent", "another-agent"
	if err := store.CreateAgentRun(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 2)
	processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow)
	processSQLiteEventWait(t, store, foreign, "reply", "worker", sqliteEventWaitNow)
	receipt := sqliteEventWaitTestReceipt(run, "authenticated-response", map[string]interface{}{
		"sender": "person", "deploymentId": run.AssignedAgentID, "bindingId": "binding", "adapterId": "adapter",
	}, sqliteEventWaitNow.Add(time.Second))
	if _, err := store.PublishRunEvent(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	claims := claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow.Add(time.Second), 1)
	if claims[0].RunID != run.ID {
		t.Fatalf("cross-deployment claim=%#v", claims)
	}
	result := processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow.Add(time.Second))
	if result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("binding result=%#v", result)
	}
	storedForeign, err := store.GetAgentRun(ctx, foreign.Scope, foreign.ID)
	if err != nil || storedForeign.Status != AgentRunStatusWaitingForEvent {
		t.Fatalf("foreign run=%#v error=%v", storedForeign, err)
	}
}

func TestSQLiteRunEventWaitHotQueriesUseIndexes(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	queries := []struct {
		name, query, index string
		args               []interface{}
	}{
		{
			name: "scope discovery", index: "idx_run_event_wait_scopes_due",
			query: `SELECT scope_kind,scope_id FROM run_event_wait_scopes WHERE available_at<=? ORDER BY available_at,scope_kind,scope_id LIMIT ?`,
			args:  []interface{}{sqliteEventWaitNow, 100},
		},
		{
			name: "unleased scope refresh", index: "idx_run_event_waits_unleased_due",
			query: `SELECT available_at FROM run_event_waits WHERE scope_kind=? AND scope_id=? AND status=? AND available_at IS NOT NULL AND lease_expires_at IS NULL ORDER BY available_at LIMIT 1`,
			args:  []interface{}{"tenant", "test", RunEventWaitPending},
		},
		{
			name: "leased scope refresh", index: "idx_run_event_waits_leased_due",
			query: `SELECT lease_expires_at FROM run_event_waits WHERE scope_kind=? AND scope_id=? AND status=? AND lease_expires_at IS NOT NULL ORDER BY lease_expires_at LIMIT 1`,
			args:  []interface{}{"tenant", "test", RunEventWaitPending},
		},
		{
			name: "event selection", index: "idx_run_event_inbox_match",
			query: `SELECT payload FROM run_event_inbox WHERE scope_kind=? AND scope_id=? AND source=? AND type=? AND subject=? AND occurred_at>=? AND received_at<=? ORDER BY occurred_at,received_at,event_id LIMIT 1`,
			args:  []interface{}{"tenant", "test", "connection", "message", "thread", sqliteEventWaitNow, sqliteEventWaitNow.Add(time.Hour)},
		},
	}
	for _, test := range queries {
		t.Run(test.name, func(t *testing.T) {
			rows, err := store.db.Query("EXPLAIN QUERY PLAN "+test.query, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			found := false
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, test.index) {
					found = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("query did not use %s", test.index)
			}
		})
	}
}

func TestSQLiteRunEventWaitReusedWorkerIdentityIsFenced(t *testing.T) {
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitTestRun("lease-token", nil)
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	first := claimSQLiteEventWait(t, store, run.Scope, "same-worker", sqliteEventWaitNow, 1)[0]
	second := claimSQLiteEventWait(t, store, run.Scope, "same-worker", sqliteEventWaitNow.Add(time.Minute), 1)[0]
	if _, err := store.ProcessRunEventWait(context.Background(), ProcessRunEventWaitRequest{
		Scope: run.Scope, RunID: run.ID, Key: "reply", WorkerID: "same-worker", Now: sqliteEventWaitNow, LeaseExpiresAt: *first.LeaseExpiresAt,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old claim reused new worker lease: %v", err)
	}
	result, err := store.ProcessRunEventWait(context.Background(), ProcessRunEventWaitRequest{
		Scope: run.Scope, RunID: run.ID, Key: "reply", WorkerID: "same-worker", Now: sqliteEventWaitNow.Add(time.Minute), LeaseExpiresAt: *second.LeaseExpiresAt,
	})
	if err != nil || result.Wait.Status != RunEventWaitPending {
		t.Fatalf("current lease result=%#v error=%v", result, err)
	}
}

func TestSQLiteRunEventWaitReceivedDeadlineNanosecond(t *testing.T) {
	for _, timely := range []bool{false, true} {
		t.Run(fmt.Sprintf("timely=%v", timely), func(t *testing.T) {
			store := newSQLiteEventWaitTestStore(t)
			run := sqliteEventWaitTestRun("deadline-boundary", nil)
			if err := store.CreateAgentRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			claimSQLiteEventWait(t, store, run.Scope, "worker", sqliteEventWaitNow, 1)
			processSQLiteEventWait(t, store, run, "reply", "worker", sqliteEventWaitNow)
			deadline := run.WakeCondition.EventWait.Deadline
			received := deadline
			if !timely {
				received = received.Add(time.Nanosecond)
			}
			if _, err := store.PublishRunEvent(context.Background(), sqliteEventWaitTestReceipt(run, "boundary-response", nil, received)); err != nil {
				t.Fatal(err)
			}
			claimSQLiteEventWait(t, store, run.Scope, "worker", deadline.Add(time.Nanosecond), 1)
			result := processSQLiteEventWait(t, store, run, "reply", "worker", deadline.Add(time.Nanosecond))
			if timely && result.Wait.Status != RunEventWaitMatched {
				t.Fatalf("timely receipt failed to match: %#v", result)
			}
			if !timely && result.Wait.Status != RunEventWaitTimedOut {
				t.Fatalf("late receipt matched: %#v", result)
			}
		})
	}
}

func BenchmarkSQLiteRunEventWaitDueScopeDiscovery(b *testing.B) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "scope-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	conn, err := beginImmediateSQLite(ctx, store.db)
	if err != nil {
		b.Fatal(err)
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	for i := 0; i < 10000; i++ {
		if _, err := conn.ExecContext(ctx, `INSERT INTO run_event_wait_scopes(scope_kind,scope_id,available_at) VALUES(?,?,?)`, "tenant", fmt.Sprintf("idle-%d", i), sqliteEventWaitNow.AddDate(1000, 0, 0)); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO run_event_wait_scopes(scope_kind,scope_id,available_at) VALUES(?,?,?)`, "tenant", "due", sqliteEventWaitNow); err != nil {
		b.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		b.Fatal(err)
	}
	committed = true
	_ = conn.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scopes, err := store.ListRunEventWaitWorkScopes(ctx, sqliteEventWaitNow.Add(time.Duration(i)*time.Second), 100)
		if err != nil || len(scopes) != 1 || scopes[0].ID != "due" {
			b.Fatalf("scopes=%#v error=%v", scopes, err)
		}
	}
}
