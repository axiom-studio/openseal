//go:build integration

package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresCallbackRunEventIntakeIsAtomicAndPreservesFirstReceipt(t *testing.T) {
	store, _ := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "callback-atomic-intake"}
	spec := RunEventWaitSpec{Key: "callback", Type: "message.received", Source: "host:callback", Subject: "resource", After: now.Add(-time.Minute), Deadline: now.Add(2 * time.Second)}
	run := postgresRunEventWaitTestCreateRun(t, store, scope, spec, now)
	newObservation := func(id string) *RunEventReceipt {
		receipt := postgresRunEventWaitTestReceipt(scope, spec, id, now)
		receipt.Event.Attributes = map[string]interface{}{"deploymentId": "agent", "bindingId": "callback-binding", "adapterId": "callback-adapter", "provider": "test"}
		return postgresRunEventWaitTestRedigest(receipt)
	}
	observation := newObservation("provider-event")
	callback := &CallbackEventReceipt{ID: uuid.NewString(), Scope: scope, RegistrationID: "registration", RegistrationRevision: 1, Event: observation.Event, Status: CallbackEventPending, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	// Dispatch or retry happens after the deadline; the preserved authenticated
	// acquisition timestamp remains the time the original receipt was captured.
	observation.ReceivedAt = now.Add(10 * time.Second)
	stored, replayed, err := store.ReceiveCallbackEventWithRunEvent(ctx, callback, observation)
	if err != nil || replayed || !stored.CreatedAt.Equal(now) {
		t.Fatalf("atomic intake stored=%#v replayed=%v err=%v", stored, replayed, err)
	}
	copy := *callback
	copy.CreatedAt, copy.UpdatedAt, copy.AvailableAt = observation.ReceivedAt, observation.ReceivedAt, observation.ReceivedAt
	stored, replayed, err = store.ReceiveCallbackEventWithRunEvent(ctx, &copy, observation)
	if err != nil || !replayed || !stored.CreatedAt.Equal(now) {
		t.Fatalf("replay changed original acquisition time: stored=%#v replayed=%v err=%v", stored, replayed, err)
	}
	claim := postgresRunEventWaitTestClaim(t, store, scope, "late-dispatch", observation.ReceivedAt, 1)
	if result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: claim.LeaseOwner, LeaseExpiresAt: *claim.LeaseExpiresAt, Now: observation.ReceivedAt}); err != nil || result.Wait.Status != RunEventWaitMatched {
		t.Fatalf("on-time receipt became late after dispatch: result=%#v err=%v", result, err)
	}
	// A different callback registration observing the same provider event may
	// not create a receipt if immutable run inbox data conflicts.
	conflicting := newObservation("provider-event")
	conflicting.Event.Payload = map[string]interface{}{"text": "conflicting provider body"}
	conflicting = postgresRunEventWaitTestRedigest(conflicting)
	other := *callback
	other.ID, other.RegistrationID, other.Event = uuid.NewString(), "other-registration", conflicting.Event
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, &other, conflicting); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("inbox conflict error=%v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+store.table("callback_events")+` WHERE id=$1`, other.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("callback was committed before inbox conflict: count=%d err=%v", count, err)
	}
	// A notification failure rolls back both records, rather than acknowledging
	// a callback that cannot wake the original workflow.
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE `+store.table("run_event_notifications")+` ADD CONSTRAINT reject_test_notification CHECK(event_id <> 'fail-notice')`); err != nil {
		t.Fatal(err)
	}
	failing := newObservation("fail-notice")
	other.ID, other.Event = uuid.NewString(), failing.Event
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, &other, failing); err == nil {
		t.Fatal("expected notification failure")
	}
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+store.table("callback_events")+` WHERE id=$1)+
		(SELECT COUNT(*) FROM `+store.table("run_event_inbox")+` WHERE event_id=$2)`, other.ID, failing.Event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial callback/inbox survived notification failure: count=%d err=%v", count, err)
	}
}
