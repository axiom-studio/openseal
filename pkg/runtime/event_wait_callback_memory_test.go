package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func memoryCallbackEventForObservation(observation *RunEventReceipt) *CallbackEventReceipt {
	return &CallbackEventReceipt{
		ID: "callback:" + observation.Event.ID, Scope: observation.Event.Scope, RegistrationID: "registration", RegistrationRevision: 1,
		Event: observation.Event, Status: CallbackEventPending, AvailableAt: observation.ReceivedAt,
		CreatedAt: observation.ReceivedAt, UpdatedAt: observation.ReceivedAt, Revision: 1,
	}
}

func memoryAtomicCallbackObservation(t *testing.T, scope Scope, id string, occurred, received time.Time) *RunEventReceipt {
	t.Helper()
	receipt := memoryEventWaitReceipt(t, scope, id, occurred, received)
	receipt.Event.Source = RunEventBindingSource("agent", "binding", "adapter")
	receipt.Event.Attributes["deploymentId"] = "agent"
	receipt.Event.Attributes["bindingId"] = "binding"
	receipt.Event.Attributes["adapterId"] = "adapter"
	receipt.Event.Attributes["provider"] = "provider"
	refreshMemoryEventWaitDigest(t, receipt)
	return receipt
}

func TestMemoryRunEventAtomicCallbackRejectsInboxConflictWithoutSavingReceipt(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "atomic-conflict"}
	start := time.Date(2026, 10, 2, 22, 0, 0, 0, time.UTC)
	observation := memoryAtomicCallbackObservation(t, scope, "event", start, start)
	if _, err := store.PublishRunEvent(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	conflicting, err := cloneMemoryRunEventReceipt(observation)
	if err != nil {
		t.Fatal(err)
	}
	conflicting.Event.Payload["changed"] = true
	refreshMemoryEventWaitDigest(t, conflicting)
	callback := memoryCallbackEventForObservation(conflicting)
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(t.Context(), callback, conflicting); !errors.Is(err, ErrRunEventConflict) {
		t.Fatalf("inbox conflict accepted: %v", err)
	}
	if store.callbackEvents[callback.ID] != nil || len(store.runEventReceipts) != 1 {
		t.Fatal("failed atomic intake saved a callback receipt")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fresh := memoryAtomicCallbackObservation(t, scope, "fresh", start, start)
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(ctx, memoryCallbackEventForObservation(fresh), fresh); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled atomic intake: %v", err)
	}
	if len(store.callbackEvents) != 0 || len(store.runEventReceipts) != 1 {
		t.Fatal("canceled intake mutated stores")
	}
	mismatch, err := cloneMemoryRunEventReceipt(fresh)
	if err != nil {
		t.Fatal(err)
	}
	mismatch.Event.Payload["wrong"] = true
	refreshMemoryEventWaitDigest(t, mismatch)
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(t.Context(), memoryCallbackEventForObservation(fresh), mismatch); !errors.Is(err, ErrCallbackRegistrationConflict) {
		t.Fatalf("callback and inbox payload mismatch accepted: %v", err)
	}
	if len(store.callbackEvents) != 0 || len(store.runEventReceipts) != 1 {
		t.Fatal("mismatched observations partially saved")
	}
}

func TestMemoryRunEventAtomicCallbackReplayRepairsOriginalDeadlineTimestamp(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "atomic-replay"}
	start := time.Date(2026, 10, 2, 22, 30, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	run.WakeCondition.EventWait.Source = RunEventBindingSource("agent", "binding", "adapter")
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	observation := memoryAtomicCallbackObservation(t, scope, "event", start.Add(time.Minute), start.Add(time.Minute))
	callback := memoryCallbackEventForObservation(observation)
	// A verified callback saved by an older host has no inbox projection yet.
	if _, replayed, err := store.ReceiveCallbackEvent(t.Context(), callback); err != nil || replayed {
		t.Fatalf("legacy callback intake: replayed=%v err=%v", replayed, err)
	}
	late := start.Add(2 * time.Hour)
	callback.CreatedAt, callback.UpdatedAt, callback.AvailableAt = late, late, late
	observation.ReceivedAt = late
	stored, replayed, err := store.ReceiveCallbackEventWithRunEvent(t.Context(), callback, observation)
	if err != nil || !replayed || !stored.CreatedAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("replay did not retain original intake: receipt=%#v replayed=%v err=%v", stored, replayed, err)
	}
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, late, "worker"), late)
	if result.Wait.Status != RunEventWaitMatched || result.Wait.EventID != observation.Event.ID {
		t.Fatal("callback retry made a timely observation look late")
	}
	// Changed normalized provider facts must not overwrite the original event.
	callback.Event.Payload["changed"] = true
	refreshMemoryEventWaitDigest(t, observation)
	if _, _, err := store.ReceiveCallbackEventWithRunEvent(t.Context(), callback, observation); !errors.Is(err, ErrCallbackRegistrationConflict) {
		t.Fatalf("changed callback body accepted: %v", err)
	}
	stored.Event.Payload["callerMutation"] = true
	if store.callbackEvents[callback.ID].Event.Payload["callerMutation"] != nil {
		t.Fatal("returned callback receipt exposed mutable store state")
	}
}
