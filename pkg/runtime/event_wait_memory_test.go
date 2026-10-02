package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func memoryEventWaitRun(scope Scope, id, key string, at time.Time) *AgentRun {
	return &AgentRun{
		ID: id, RootRunID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Wait for the requested response", Source: RunSourceEvent, Status: AgentRunStatusWaitingForEvent,
		Revision: 1, CreatedAt: at, UpdatedAt: at, Checkpoint: map[string]interface{}{"saved": "context"},
		WakeCondition: &WakeCondition{Type: "event", EventWait: &RunEventWaitSpec{
			Key: key, Source: "connection:one", Type: "message.received", Subject: "thread:one",
			Attributes: map[string]interface{}{"sender": "teammate"}, After: at, Deadline: at.Add(time.Hour),
		}},
	}
}

func memoryEventWaitActivity(run *AgentRun) *ActivityEvent {
	return &ActivityEvent{ID: "activity:" + run.ID, Scope: run.Scope, RunID: run.ID, EventType: "run.updated", Summary: "Run updated", CreatedAt: run.UpdatedAt}
}

func memoryEventWaitReceipt(t testing.TB, scope Scope, id string, occurred, received time.Time) *RunEventReceipt {
	t.Helper()
	receipt := &RunEventReceipt{Event: EventEnvelope{
		ID: id, Scope: scope, Type: "message.received", Source: "connection:one", Subject: "thread:one", OccurredAt: occurred,
		Attributes: map[string]interface{}{"sender": "teammate"}, Payload: map[string]interface{}{"response": map[string]interface{}{"body": "My tasks"}},
	}, ReceivedAt: received}
	refreshMemoryEventWaitDigest(t, receipt)
	return receipt
}

func refreshMemoryEventWaitDigest(t testing.TB, receipt *RunEventReceipt) {
	t.Helper()
	data, err := json.Marshal(receipt.Event)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	receipt.Digest = hex.EncodeToString(digest[:])
}

func claimMemoryEventWait(t *testing.T, store *MemoryStore, scope Scope, now time.Time, worker string) *RunEventWait {
	t.Helper()
	waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, Limit: 1})
	if err != nil || len(waits) != 1 {
		t.Fatalf("claim: waits=%v err=%v", waits, err)
	}
	return waits[0]
}

func processMemoryEventWait(t *testing.T, store *MemoryStore, wait *RunEventWait, now time.Time) *RunEventWaitResult {
	t.Helper()
	result, err := store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: wait.Scope, RunID: wait.RunID, Key: wait.Spec.Key, WorkerID: wait.LeaseOwner, Now: now, LeaseExpiresAt: *wait.LeaseExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMemoryRunEventWaitRetainsEarlyReplyAndConsumesOncePerRun(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "early"}
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	receipt := memoryEventWaitReceipt(t, scope, "reply:one", start.Add(time.Second), start.Add(2*time.Second))
	if inserted, err := store.PublishRunEvent(t.Context(), receipt); err != nil || !inserted {
		t.Fatalf("publish: inserted=%v err=%v", inserted, err)
	}
	// Caller ownership of nested payloads ends at Publish.
	receipt.Event.Payload["response"].(map[string]interface{})["body"] = "Mutated"
	run := memoryEventWaitRun(scope, "run:one", "wait:one", start)
	if _, err := store.CreateAgentRunWithEvent(t.Context(), run, memoryEventWaitActivity(run)); err != nil {
		t.Fatal(err)
	}
	now := start.Add(3 * time.Second)
	wait := claimMemoryEventWait(t, store, scope, now, "worker:one")
	result := processMemoryEventWait(t, store, wait, now)
	if result.Wait.Status != RunEventWaitMatched || result.Wait.EventID != "reply:one" || result.Run.ID != run.ID || result.Run.Status != AgentRunStatusQueued || result.Run.Revision != 2 {
		t.Fatalf("incorrect resolution: %#v", result)
	}
	if result.Run.Checkpoint["saved"] != "context" || result.Event.Payload["response"].(map[string]interface{})["body"] != "My tasks" {
		t.Fatal("saved context or event payload was lost")
	}
	if _, err := store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: wait.Spec.Key, WorkerID: wait.LeaseOwner, Now: now, LeaseExpiresAt: *wait.LeaseExpiresAt}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("repeated processing: %v", err)
	}
	activity, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(activity) != 2 || activity[1].EventType != "run.event_wait_resolved" {
		t.Fatalf("audit duplicated or missing: events=%v err=%v", activity, err)
	}
	// Re-arm a different step in the same Run. The same event cannot satisfy it.
	next := memoryEventWaitRun(scope, run.ID, "wait:two", start)
	next.Revision, next.UpdatedAt = result.Run.Revision+1, now
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), next, result.Run.Revision, memoryEventWaitActivity(next), nil); err != nil {
		t.Fatal(err)
	}
	second := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, now, "worker:two"), now)
	if second.Run != nil || second.Event != nil || second.Wait.Status != RunEventWaitPending || second.Wait.AvailableAt == nil || !second.Wait.AvailableAt.Equal(next.WakeCondition.EventWait.Deadline) {
		t.Fatalf("event reused or idle wait polling: %#v", second)
	}
	// Consumption belongs to the Run, so an independent Run can observe it.
	other := memoryEventWaitRun(scope, "run:other", "wait:one", start)
	if err := store.CreateAgentRun(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	third := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, now, "worker:three"), now)
	if third.Run.ID != other.ID || third.Wait.Status != RunEventWaitMatched {
		t.Fatal("independent Run could not consume the buffered event")
	}
}

func TestMemoryRunEventWaitIdleUntilDeadlineAndWakeDuringLease(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "idle"}
	start := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	first := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, start, "worker"), start)
	if first.Run != nil || first.Wait.AvailableAt == nil || !first.Wait.AvailableAt.Equal(run.WakeCondition.EventWait.Deadline) {
		t.Fatal("an unmatched wait must sleep until its deadline")
	}
	if waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "worker", Now: start.Add(time.Minute), LeaseDuration: time.Minute, Limit: 10}); err != nil || len(waits) != 0 {
		t.Fatalf("idle wait polled: waits=%v err=%v", waits, err)
	}
	replyAt := start.Add(2 * time.Minute)
	firstReply := memoryEventWaitReceipt(t, scope, "unrelated", replyAt, replyAt)
	firstReply.Event.Attributes["sender"] = "other"
	refreshMemoryEventWaitDigest(t, firstReply)
	if _, err := store.PublishRunEvent(t.Context(), firstReply); err != nil {
		t.Fatal(err)
	}
	wait, _ := store.GetRunEventWait(t.Context(), scope, run.ID, "wait")
	if !wait.AvailableAt.Equal(run.WakeCondition.EventWait.Deadline) {
		t.Fatal("a mismatched event disturbed the idle wait")
	}
	matching := memoryEventWaitReceipt(t, scope, "matching", replyAt, replyAt)
	if _, err := store.PublishRunEvent(t.Context(), matching); err != nil {
		t.Fatal(err)
	}
	claimed := claimMemoryEventWait(t, store, scope, replyAt, "worker:claimed")
	// Another event arrives while the worker owns the wait. Publication must
	// retain both its wake availability and the lease used to resolve it.
	if _, err := store.PublishRunEvent(t.Context(), memoryEventWaitReceipt(t, scope, "second", replyAt, replyAt)); err != nil {
		t.Fatal(err)
	}
	if result := processMemoryEventWait(t, store, claimed, replyAt); result.Wait.Status != RunEventWaitMatched || result.Wait.EventID != matching.Event.ID {
		t.Fatalf("held lease lost its reply: %#v", result)
	}
	timeout := memoryEventWaitRun(scope, "timeout", "wait", start)
	if err := store.CreateAgentRun(t.Context(), timeout); err != nil {
		t.Fatal(err)
	}
	// This Run intentionally expects a different subject from all buffered events.
	timeout.WakeCondition.EventWait.Subject = "thread:missing"
	timeout.Revision++
	// A new key is mandatory when changing the immutable correlation contract.
	timeout.WakeCondition.EventWait.Key = "wait:missing"
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), timeout, 1, memoryEventWaitActivity(timeout), nil); err != nil {
		t.Fatal(err)
	}
	deadline := timeout.WakeCondition.EventWait.Deadline
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, deadline, "worker:deadline"), deadline)
	if result.Wait.Status != RunEventWaitTimedOut || result.Run.ID != timeout.ID || result.Run.Checkpoint["saved"] != "context" {
		t.Fatalf("deadline did not resume the saved Run: %#v", result)
	}
}

func TestMemoryRunEventWaitPauseResumeBuffersReply(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "pause"}
	start := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if _, err := store.CreateAgentRunWithEvent(t.Context(), run, memoryEventWaitActivity(run)); err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(store)
	commands.now = func() time.Time { return start.Add(time.Minute) }
	paused, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: run.ID, Kind: AgentRunCommandPause, ExpectedRevision: run.Revision})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := store.GetRunEventWait(t.Context(), scope, run.ID, "wait")
	if err != nil || wait.Status != RunEventWaitPaused || wait.AvailableAt != nil {
		t.Fatalf("pause projection: wait=%v err=%v", wait, err)
	}
	replyAt := start.Add(2 * time.Minute)
	if _, err := store.PublishRunEvent(t.Context(), memoryEventWaitReceipt(t, scope, "reply", replyAt, replyAt)); err != nil {
		t.Fatal(err)
	}
	if waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "worker", Now: replyAt, LeaseDuration: time.Minute, Limit: 10}); err != nil || len(waits) != 0 {
		t.Fatal("paused Run was scheduled")
	}
	commands.now = func() time.Time { return start.Add(3 * time.Minute) }
	resumed, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: run.ID, Kind: AgentRunCommandResume, ExpectedRevision: paused.Run.Revision})
	if err != nil {
		t.Fatal(err)
	}
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, resumed.Run.UpdatedAt, "worker"), resumed.Run.UpdatedAt)
	if result.Wait.Status != RunEventWaitMatched || result.Run.ID != run.ID || result.Wait.EventID != "reply" {
		t.Fatal("resume lost the reply received during pause")
	}
}

func TestMemoryRunEventWaitImmutableContractAndInvalidProjectionAtomicity(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "immutable"}
	start := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if _, err := store.CreateAgentRunWithEvent(t.Context(), run, memoryEventWaitActivity(run)); err != nil {
		t.Fatal(err)
	}
	invalid := cloneAgentRun(run)
	invalid.Revision++
	invalid.WakeCondition.EventWait.Subject = "thread:other"
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), invalid, run.Revision, memoryEventWaitActivity(invalid), nil); !errors.Is(err, ErrInvalidRunEventWait) {
		t.Fatalf("mutable wait accepted: %v", err)
	}
	current, _ := store.GetAgentRun(t.Context(), scope, run.ID)
	activity, _ := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: run.ID})
	if current.Revision != run.Revision || current.WakeCondition.EventWait.Subject != "thread:one" || len(activity) != 1 {
		t.Fatal("rejected wait update mutated canonical state or audit")
	}
	invalidCreate := memoryEventWaitRun(scope, "invalid", "wait", start)
	invalidCreate.WakeCondition.EventWait.Deadline = start
	if _, err := store.CreateAgentRunWithEvent(t.Context(), invalidCreate, memoryEventWaitActivity(invalidCreate)); !errors.Is(err, ErrInvalidRunEventWait) {
		t.Fatalf("invalid wait creation accepted: %v", err)
	}
	if current, _ := store.GetAgentRun(t.Context(), scope, invalidCreate.ID); current != nil {
		t.Fatal("invalid creation saved a Run")
	}
	canceled := cloneAgentRun(run)
	canceled.Status, canceled.WakeCondition, canceled.Revision = AgentRunStatusCanceled, nil, run.Revision+1
	canceled.UpdatedAt = start.Add(time.Minute)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), canceled, run.Revision, memoryEventWaitActivity(canceled), nil); err != nil {
		t.Fatal(err)
	}
	if wait, _ := store.GetRunEventWait(t.Context(), scope, run.ID, "wait"); wait.Status != RunEventWaitCanceled || wait.AvailableAt != nil {
		t.Fatal("cancellation did not retire the wait")
	}
	rearmed := cloneAgentRun(run)
	rearmed.Revision = canceled.Revision + 1
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), rearmed, canceled.Revision, memoryEventWaitActivity(rearmed), nil); !errors.Is(err, ErrInvalidRunEventWait) {
		t.Fatalf("resolved key reused: %v", err)
	}
}

func TestMemoryRunEventWaitClaimsFenceConcurrentAndExpiredWorkers(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "leases"}
	start := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	claims := make(chan *RunEventWait, 20)
	errorsFound := make(chan error, 20)
	for i := 0; i < cap(claims); i++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			waits, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: fmt.Sprintf("worker:%d", worker), Now: start, LeaseDuration: time.Minute, Limit: 1})
			if err != nil {
				errorsFound <- err
			}
			for _, wait := range waits {
				claims <- wait
			}
		}(i)
	}
	group.Wait()
	close(claims)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("%d workers claimed one wait", len(claims))
	}
	original := <-claims
	reclaimed := claimMemoryEventWait(t, store, scope, start.Add(time.Minute), "worker:reclaimed")
	if _, err := store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: "wait", WorkerID: original.LeaseOwner, Now: start.Add(time.Minute), LeaseExpiresAt: *original.LeaseExpiresAt}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker processed reclaimed lease: %v", err)
	}
	if _, err := store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: Scope{Kind: "tenant", ID: "other"}, RunID: run.ID, Key: "wait", WorkerID: reclaimed.LeaseOwner, Now: start.Add(time.Minute), LeaseExpiresAt: *reclaimed.LeaseExpiresAt}); !errors.Is(err, ErrRunEventWaitNotFound) {
		t.Fatalf("foreign tenant processed wait: %v", err)
	}
	// Simulate an authoritative Run update through a separate store path.
	store.mu.Lock()
	store.agentRuns[portfolioKey(scope, run.ID)].Status = AgentRunStatusCanceled
	store.agentRuns[portfolioKey(scope, run.ID)].WakeCondition = nil
	store.mu.Unlock()
	result := processMemoryEventWait(t, store, reclaimed, start.Add(time.Minute))
	if result.Wait.Status != RunEventWaitCanceled || result.Run != nil || result.Activity != nil {
		t.Fatal("stale projection overrode authoritative cancellation")
	}
}

func TestMemoryRunEventWaitExactLeaseFencesReclaimedSameWorker(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "same-worker"}
	start := time.Date(2026, 10, 2, 16, 30, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	first := claimMemoryEventWait(t, store, scope, start, "worker")
	second := claimMemoryEventWait(t, store, scope, start.Add(time.Minute), "worker")
	_, err := store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: "wait", WorkerID: "worker", Now: start.Add(time.Minute), LeaseExpiresAt: *first.LeaseExpiresAt})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old attempt reused replacement lease: %v", err)
	}
	if result := processMemoryEventWait(t, store, second, start.Add(time.Minute)); result.Wait.Status != RunEventWaitPending || result.Run != nil {
		t.Fatal("current attempt could not release its unmatched wait")
	}
}

func TestMemoryRunEventWaitDuplicateConflictCloneAndScopedPrune(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "prune"}
	start := time.Date(2026, 10, 2, 17, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		receipt := memoryEventWaitReceipt(t, scope, fmt.Sprintf("reply:%d", i), start, start.Add(time.Duration(i)*time.Second))
		if inserted, err := store.PublishRunEvent(t.Context(), receipt); err != nil || !inserted {
			t.Fatalf("publish: %v %v", inserted, err)
		}
		if inserted, err := store.PublishRunEvent(t.Context(), receipt); err != nil || inserted {
			t.Fatalf("replay: %v %v", inserted, err)
		}
		receipt.Event.Payload["changed"] = true
		refreshMemoryEventWaitDigest(t, receipt)
		if _, err := store.PublishRunEvent(t.Context(), receipt); !errors.Is(err, ErrRunEventConflict) {
			t.Fatalf("conflicting identity accepted: %v", err)
		}
	}
	other := Scope{Kind: "tenant", ID: "other"}
	if _, err := store.PublishRunEvent(t.Context(), memoryEventWaitReceipt(t, other, "reply:other", start, start)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.PruneRunEvents(t.Context(), scope, start.Add(RunEventRetention+time.Minute), 2); err != nil || count != 2 {
		t.Fatalf("bounded prune: count=%d err=%v", count, err)
	}
	if count, err := store.PruneRunEvents(t.Context(), scope, start.Add(RunEventRetention+time.Minute), 2); err != nil || count != 1 {
		t.Fatalf("remaining prune: count=%d err=%v", count, err)
	}
	if _, err := store.PublishRunEvent(t.Context(), memoryEventWaitReceipt(t, other, "reply:other", start, start)); err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	if len(store.runEventReceipts) != 1 || len(store.runEventReceiptIndex) != 1 {
		store.mu.RUnlock()
		t.Fatal("prune crossed scopes or left index entries behind")
	}
	store.mu.RUnlock()
}

func TestMemoryRunEventWaitWorkScopeDiscoveryFairAndIdle(t *testing.T) {
	store := NewMemoryStore()
	start := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b", "c"} {
		run := memoryEventWaitRun(Scope{Kind: "tenant", ID: id}, "run", "wait", start)
		if err := store.CreateAgentRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []string{"a", "b", "c"} {
		scopes, err := store.ListRunEventWaitWorkScopes(t.Context(), start, 1)
		if err != nil || len(scopes) != 1 || scopes[0].ID != expected {
			t.Fatalf("fair discovery: scopes=%v err=%v expected=%s", scopes, err, expected)
		}
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(t.Context(), start, 1); err != nil || len(scopes) != 0 {
		t.Fatalf("discovery repeatedly chose the same scope: scopes=%v err=%v", scopes, err)
	}
	scope := Scope{Kind: "tenant", ID: "a"}
	processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, start, "worker"), start)
	scopes, err := store.ListRunEventWaitWorkScopes(t.Context(), start.Add(time.Second), 100)
	if err != nil || len(scopes) != 2 || scopes[0].ID != "b" || scopes[1].ID != "c" {
		t.Fatalf("idle scope remained available: scopes=%v err=%v", scopes, err)
	}
	replyAt := start.Add(time.Minute)
	if _, err := store.PublishRunEvent(t.Context(), memoryEventWaitReceipt(t, scope, "reply", replyAt, replyAt)); err != nil {
		t.Fatal(err)
	}
	scopes, err = store.ListRunEventWaitWorkScopes(t.Context(), replyAt, 100)
	if err != nil || len(scopes) != 3 {
		t.Fatalf("publication did not wake the idle scope: scopes=%v err=%v", scopes, err)
	}
	for _, id := range []string{"a", "b", "c"} {
		scope := Scope{Kind: "tenant", ID: id}
		run, _ := store.GetAgentRun(t.Context(), scope, "run")
		canceled := cloneAgentRun(run)
		canceled.Status, canceled.WakeCondition, canceled.Revision = AgentRunStatusCanceled, nil, run.Revision+1
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), canceled, run.Revision, memoryEventWaitActivity(canceled), nil); err != nil {
			t.Fatal(err)
		}
	}
	// A queued observation may outlive the canceled waits it originally
	// targeted. A bounded notification pass retires it without claiming Runs.
	if claimed, err := store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "cleanup", Now: start.Add(24 * time.Hour), LeaseDuration: time.Minute, Limit: 100}); err != nil || len(claimed) != 0 {
		t.Fatalf("canceled Run claimed during notification cleanup: claimed=%v err=%v", claimed, err)
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(t.Context(), start.Add(24*time.Hour), 100); err != nil || len(scopes) != 0 {
		t.Fatalf("canceled scopes retained work: scopes=%v err=%v", scopes, err)
	}
}

func TestMemoryRunEventWaitBindingAuthorityAndCloneIsolation(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "authority"}
	start := time.Date(2026, 10, 2, 19, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	run.WakeCondition.EventWait.Source = RunEventBindingSource("agent", "binding", "adapter")
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, start, "worker"), start)
	receipt := memoryEventWaitReceipt(t, scope, "foreign", start.Add(time.Minute), start.Add(time.Minute))
	receipt.Event.Source = run.WakeCondition.EventWait.Source
	receipt.Event.Attributes["deploymentId"] = "other-agent"
	receipt.Event.Attributes["bindingId"] = "binding"
	receipt.Event.Attributes["adapterId"] = "adapter"
	refreshMemoryEventWaitDigest(t, receipt)
	if _, err := store.PublishRunEvent(t.Context(), receipt); !errors.Is(err, ErrInvalidRunEventWait) {
		t.Fatalf("forged binding identity accepted: %v", err)
	}
	receipt.Event.Source = RunEventBindingSource("other-agent", "binding", "adapter")
	refreshMemoryEventWaitDigest(t, receipt)
	if _, err := store.PublishRunEvent(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	wait, _ := store.GetRunEventWait(t.Context(), scope, "run", "wait")
	if !wait.AvailableAt.Equal(run.WakeCondition.EventWait.Deadline) {
		t.Fatal("a foreign deployment's event woke the wait")
	}
	// Get must not return aliases into mutable store state.
	wait.Spec.Attributes["sender"] = "mutated"
	*wait.AvailableAt = start
	stored, _ := store.GetRunEventWait(t.Context(), scope, "run", "wait")
	if stored.Spec.Attributes["sender"] != "teammate" || !stored.AvailableAt.Equal(run.WakeCondition.EventWait.Deadline) {
		t.Fatal("returned wait exposed mutable store state")
	}
	receipt.Event.ID = "authorized"
	receipt.Event.Source = run.WakeCondition.EventWait.Source
	receipt.Event.Attributes["deploymentId"] = "agent"
	refreshMemoryEventWaitDigest(t, receipt)
	if _, err := store.PublishRunEvent(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, receipt.ReceivedAt, "worker"), receipt.ReceivedAt)
	if result.Wait.Status != RunEventWaitMatched || result.Wait.EventID != "authorized" {
		t.Fatal("authenticated binding identity did not resolve the wait")
	}
}

func TestMemoryRunEventWaitNumericCorrelationSurvivesCanonicalClone(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "numeric"}
	start := time.Date(2026, 10, 2, 19, 30, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	run.WakeCondition.EventWait.Attributes["sequence"] = json.Number("9007199254740993")
	run.WakeCondition.EventWait.Attributes["revision"] = 1
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	receipt := memoryEventWaitReceipt(t, scope, "reply", start.Add(time.Minute), start.Add(time.Minute))
	receipt.Event.Attributes["sequence"] = json.Number("9007199254740993")
	receipt.Event.Attributes["revision"] = json.Number("1.0")
	refreshMemoryEventWaitDigest(t, receipt)
	if _, err := store.PublishRunEvent(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, receipt.ReceivedAt, "worker"), receipt.ReceivedAt)
	if result.Wait.Status != RunEventWaitMatched || result.Event == nil || !runEventScalarEqual(result.Event.Attributes["sequence"], json.Number("9007199254740993")) {
		t.Fatalf("exact numeric correlation was rounded or mismatched: %#v", result)
	}
}

func BenchmarkMemoryRunEventPublishIndexedWaits(b *testing.B) {
	for _, fixture := range []struct {
		waitCount int
		fanout    bool
	}{{1, false}, {10000, false}, {10000, true}} {
		b.Run(fmt.Sprintf("waits=%d/fanout=%v", fixture.waitCount, fixture.fanout), func(b *testing.B) {
			store := NewMemoryStore()
			scope := Scope{Kind: "tenant", ID: "benchmark"}
			start := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
			for i := 0; i < fixture.waitCount; i++ {
				run := memoryEventWaitRun(scope, fmt.Sprintf("run:%d", i), "wait", start)
				run.WakeCondition.EventWait.Subject = fmt.Sprintf("thread:%d", i)
				if fixture.fanout {
					run.WakeCondition.EventWait.Subject = "thread:0"
				}
				if err := store.CreateAgentRun(context.Background(), run); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				receipt := memoryEventWaitReceipt(b, scope, fmt.Sprintf("event:%d", i), start, start)
				receipt.Event.Subject = "thread:0"
				refreshMemoryEventWaitDigest(b, receipt)
				if _, err := store.PublishRunEvent(context.Background(), receipt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
