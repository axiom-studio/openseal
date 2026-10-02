package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func sqliteEventWaitCallbackReceipt(receipt *RunEventReceipt, at time.Time) *CallbackEventReceipt {
	receipt.Event.Attributes = cloneMap(receipt.Event.Attributes)
	if receipt.Event.Attributes == nil {
		receipt.Event.Attributes = make(map[string]interface{})
	}
	receipt.Event.Attributes["deploymentId"] = "agent"
	receipt.Event.Attributes["bindingId"] = "binding"
	receipt.Event.Attributes["adapterId"] = "adapter"
	receipt.Event.Attributes["provider"] = "provider"
	data, _ := json.Marshal(receipt.Event)
	digest := sha256.Sum256(data)
	receipt.Digest = hex.EncodeToString(digest[:])
	return &CallbackEventReceipt{
		ID: "callback:" + receipt.Event.ID, Scope: receipt.Event.Scope, RegistrationID: "registration", RegistrationRevision: 1,
		Event: receipt.Event, Status: CallbackEventPending, Revision: 1, CreatedAt: at, UpdatedAt: at, AvailableAt: at,
	}
}

func sqliteEventWaitCallbackTestRun(id string, attributes map[string]interface{}) *AgentRun {
	run := sqliteEventWaitTestRun(id, attributes)
	run.WakeCondition.EventWait.Source = RunEventBindingSource("agent", "binding", "adapter")
	return run
}

func TestSQLiteCallbackRunEventIntakeRollsBackAllReceipts(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitCallbackTestRun("callback-atomic", nil)
	receipt := sqliteEventWaitTestReceipt(run, "provider-response", nil, sqliteEventWaitNow)
	callback := sqliteEventWaitCallbackReceipt(receipt, sqliteEventWaitNow)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_callback_wake BEFORE INSERT ON run_event_notifications
		BEGIN SELECT RAISE(ABORT,'injected notification failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, callback, receipt); err == nil {
		t.Fatal("expected notification persistence failure")
	}
	var callbacks, observations int
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM callback_events),(SELECT count(*) FROM run_event_inbox)`).Scan(&callbacks, &observations); err != nil || callbacks != 0 || observations != 0 {
		t.Fatalf("partial receipt persisted: callbacks=%d observations=%d error=%v", callbacks, observations, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_callback_wake`); err != nil {
		t.Fatal(err)
	}
	stored, replayed, err := store.ReceiveCallbackEventWithRunEvent(ctx, callback, receipt)
	if err != nil || replayed || stored.ID != callback.ID {
		t.Fatalf("retry callback=%#v replay=%v error=%v", stored, replayed, err)
	}
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM callback_events),(SELECT count(*) FROM run_event_inbox)`).Scan(&callbacks, &observations); err != nil || callbacks != 1 || observations != 1 {
		t.Fatalf("committed receipts: callbacks=%d observations=%d error=%v", callbacks, observations, err)
	}
}

func TestSQLiteCallbackRunEventIntakeConflictsDoNotCommitCallback(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitCallbackTestRun("callback-conflict", nil)
	receipt := sqliteEventWaitTestReceipt(run, "provider-response", nil, sqliteEventWaitNow)
	_ = sqliteEventWaitCallbackReceipt(receipt, sqliteEventWaitNow)
	if _, err := store.PublishRunEvent(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	changed := sqliteEventWaitTestReceipt(run, receipt.Event.ID, nil, sqliteEventWaitNow)
	changed.Event.Payload = map[string]interface{}{"text": "Different response for the same identity"}
	data, _ := json.Marshal(changed.Event)
	digest := sha256.Sum256(data)
	changed.Digest = hex.EncodeToString(digest[:])
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, sqliteEventWaitCallbackReceipt(changed, sqliteEventWaitNow), changed); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("identity collision error=%v", err)
	}
	var callbacks int
	if err := store.db.QueryRow(`SELECT count(*) FROM callback_events`).Scan(&callbacks); err != nil || callbacks != 0 {
		t.Fatalf("conflicting intake committed callback: count=%d error=%v", callbacks, err)
	}
}

func TestSQLiteCallbackRunEventIntakeLegacyReplayPreservesDeadline(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitCallbackTestRun("callback-repair", map[string]interface{}{"sender": "person"})
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	receipt := sqliteEventWaitTestReceipt(run, "timely-provider-response", map[string]interface{}{"sender": "person"}, sqliteEventWaitNow)
	callback := sqliteEventWaitCallbackReceipt(receipt, sqliteEventWaitNow)
	if _, replayed, err := store.ReceiveCallbackEvent(ctx, callback); err != nil || replayed {
		t.Fatalf("legacy callback persistence replay=%v error=%v", replayed, err)
	}
	late := run.WakeCondition.EventWait.Deadline.Add(time.Minute)
	receipt.ReceivedAt = late
	replay := sqliteEventWaitCallbackReceipt(receipt, late)
	stored, replayed, err := store.ReceiveCallbackEventWithRunEvent(ctx, replay, receipt)
	if err != nil || !replayed || !stored.CreatedAt.Equal(callback.CreatedAt) {
		t.Fatalf("repaired callback=%#v replay=%v error=%v", stored, replayed, err)
	}
	claimSQLiteEventWait(t, store, run.Scope, "worker", late, 1)
	result := processSQLiteEventWait(t, store, run, "reply", "worker", late)
	if result.Wait.Status != RunEventWaitMatched || result.Event.ID != receipt.Event.ID {
		t.Fatalf("callback replay lost timely response: %#v", result)
	}
}

func TestSQLiteCallbackRunEventIntakeRejectsAlteredProviderFact(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteEventWaitTestStore(t)
	run := sqliteEventWaitCallbackTestRun("callback-fact", nil)
	receipt := sqliteEventWaitTestReceipt(run, "provider-response", nil, sqliteEventWaitNow)
	callback := sqliteEventWaitCallbackReceipt(receipt, sqliteEventWaitNow)
	changed := *receipt
	changed.Event.Payload = map[string]interface{}{"text": "A fact that the signed callback did not contain"}
	data, _ := json.Marshal(changed.Event)
	digest := sha256.Sum256(data)
	changed.Digest = hex.EncodeToString(digest[:])
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, callback, &changed); !errors.Is(err, ErrCallbackRegistrationConflict) {
		t.Fatalf("altered observation error=%v", err)
	}
	var callbacks, observations int
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM callback_events),(SELECT count(*) FROM run_event_inbox)`).Scan(&callbacks, &observations); err != nil || callbacks != 0 || observations != 0 {
		t.Fatalf("altered fact wrote state: callbacks=%d observations=%d error=%v", callbacks, observations, err)
	}
}
