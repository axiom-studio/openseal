package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func notificationContractCreate(t testing.TB, store eventWaitContractStore, scope Scope, id, subject, sender string, at time.Time) *AgentRun {
	t.Helper()
	run := memoryEventWaitRun(scope, id, "reply", at)
	run.WakeCondition.EventWait.Subject = subject
	run.WakeCondition.EventWait.Attributes["sender"] = sender
	if _, err := store.CreateAgentRunWithEvent(context.Background(), run, memoryEventWaitActivity(run)); err != nil {
		t.Fatal(err)
	}
	return run
}

func notificationContractPark(t testing.TB, store eventWaitContractStore, scope Scope, now time.Time, count int) {
	t.Helper()
	for count > 0 {
		waits, err := store.ClaimRunEventWaits(context.Background(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "initial-probe", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) == 0 {
			t.Fatalf("initial inbox probes: count=%d err=%v", len(waits), err)
		}
		for _, wait := range waits {
			result, err := store.ProcessRunEventWait(context.Background(), ProcessRunEventWaitRequest{
				Scope: scope, RunID: wait.RunID, Key: wait.Spec.Key, WorkerID: wait.LeaseOwner,
				Now: now, LeaseExpiresAt: *wait.LeaseExpiresAt,
			})
			if err != nil || result == nil || result.Wait.Status != RunEventWaitPending || result.Wait.AvailableAt == nil || !result.Wait.AvailableAt.Equal(wait.Spec.Deadline) {
				t.Fatalf("idle expectation was not parked until its deadline: %#v %v", result, err)
			}
		}
		count -= len(waits)
	}
}

func notificationContractProcessClaims(t testing.TB, store eventWaitContractStore, scope Scope, now time.Time, waits []*RunEventWait) {
	t.Helper()
	for _, wait := range waits {
		result, err := store.ProcessRunEventWait(context.Background(), ProcessRunEventWaitRequest{
			Scope: scope, RunID: wait.RunID, Key: wait.Spec.Key, WorkerID: wait.LeaseOwner,
			Now: now, LeaseExpiresAt: *wait.LeaseExpiresAt,
		})
		if err != nil || result == nil || result.Wait.Status != RunEventWaitMatched || result.Run == nil || result.Run.Status != AgentRunStatusQueued {
			t.Fatalf("notified execution did not resume from the authenticated inbox: %#v %v", result, err)
		}
	}
}

// Mismatching attributes must not turn a notification page into an unbounded
// scan. An empty claim still leaves durable work until the cursor reaches the
// actual recipient, independently of the worker's wait claim limit.
func TestRunEventNotificationBoundsMismatchingCorrelationPages(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "notification-pages"}
		start := eventWaitContractEpoch
		for index := 0; index < 2*runEventNotificationPageLimit; index++ {
			notificationContractCreate(t, fixture.store, scope, fmt.Sprintf("mismatch-%06d", index), "thread:one", "different-person", start)
		}
		target := notificationContractCreate(t, fixture.store, scope, "target-last", "thread:one", "teammate", start)
		now := start.Add(time.Second)
		notificationContractPark(t, fixture.store, scope, now, 2*runEventNotificationPageLimit+1)
		receipt := memoryEventWaitReceipt(t, scope, "bounded-reply", start.Add(2*time.Second), start.Add(3*time.Second))
		if fresh, err := fixture.store.PublishRunEvent(t.Context(), receipt); err != nil || !fresh {
			t.Fatalf("publication: %v %v", fresh, err)
		}
		now = start.Add(4 * time.Second)
		for page := 0; page < 2; page++ {
			waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
				Scope: scope, WorkerID: fmt.Sprintf("page-%d", page), Now: now, LeaseDuration: time.Minute, Limit: 1,
			})
			if err != nil || len(waits) != 0 {
				t.Fatalf("page %d inspected beyond its bounded mismatching recipients: waits=%v err=%v", page, waits, err)
			}
			due, err := fixture.store.ListRunEventWaitWorkScopes(t.Context(), now, 100)
			if err != nil || len(due) != 1 || due[0] != scope {
				t.Fatalf("unfinished notification lost scope readiness: %v %v", due, err)
			}
			now = now.Add(2 * time.Second)
		}
		waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "recipient-page", Now: now, LeaseDuration: time.Minute, Limit: 1,
		})
		if err != nil || len(waits) != 1 || waits[0].RunID != target.ID {
			t.Fatalf("paged notification missed its correlated recipient: %v %v", waits, err)
		}
		notificationContractProcessClaims(t, fixture.store, scope, now, waits)
	})
}

// Only one page is marked ready by a claim. Restart preserves remaining cursor
// progress, while a new execution behind the cursor still probes the inbox.
func TestRunEventNotificationBoundedFanoutSurvivesRestartAndLateArm(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "notification-restart"}
		start := eventWaitContractEpoch
		const count = 2*runEventNotificationPageLimit + 5
		for index := 0; index < count; index++ {
			notificationContractCreate(t, fixture.store, scope, fmt.Sprintf("run-%06d", index), "thread:one", "teammate", start)
		}
		notificationContractPark(t, fixture.store, scope, start.Add(time.Second), count)
		receipt := memoryEventWaitReceipt(t, scope, "fanout-reply", start.Add(2*time.Second), start.Add(3*time.Second))
		if fresh, err := fixture.store.PublishRunEvent(t.Context(), receipt); err != nil || !fresh {
			t.Fatalf("publication: %v %v", fresh, err)
		}
		// Publication itself must not update an unbounded set of executions.
		for index := 0; index < count; index++ {
			wait, err := fixture.store.GetRunEventWait(t.Context(), scope, fmt.Sprintf("run-%06d", index), "reply")
			if err != nil || wait.AvailableAt == nil || !wait.AvailableAt.Equal(wait.Spec.Deadline) {
				t.Fatalf("publication synchronously changed recipient %d: %#v %v", index, wait, err)
			}
		}
		now := start.Add(4 * time.Second)
		waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "first-page", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) != runEventNotificationPageLimit {
			t.Fatalf("first notification page: %d %v", len(waits), err)
		}
		notificationContractProcessClaims(t, fixture.store, scope, now, waits)
		for index := runEventNotificationPageLimit; index < count; index++ {
			wait, err := fixture.store.GetRunEventWait(t.Context(), scope, fmt.Sprintf("run-%06d", index), "reply")
			if err != nil || wait.AvailableAt == nil || !wait.AvailableAt.Equal(wait.Spec.Deadline) {
				t.Fatalf("first page changed later recipient %d: %#v %v", index, wait, err)
			}
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		late := notificationContractCreate(t, fixture.store, scope, "aaa-late-registration", "thread:one", "teammate", start)
		completed := runEventNotificationPageLimit
		for attempt := 0; attempt < 5 && completed < count+1; attempt++ {
			now = now.Add(2 * time.Second)
			waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
				Scope: scope, WorkerID: fmt.Sprintf("remaining-page-%d", attempt), Now: now, LeaseDuration: time.Minute, Limit: 100,
			})
			if err != nil {
				t.Fatal(err)
			}
			notificationContractProcessClaims(t, fixture.store, scope, now, waits)
			completed += len(waits)
		}
		if completed != count+1 {
			t.Fatalf("remaining fanout lost after restart: completed=%d want=%d", completed, count+1)
		}
		result, err := fixture.store.GetAgentRun(t.Context(), scope, late.ID)
		if err != nil || result.Status != AgentRunStatusQueued {
			t.Fatalf("late registration behind cursor lost retained reply: %#v %v", result, err)
		}
	})
}

func TestRunEventNotificationRotatesLargeFanoutWithoutStarvingOtherReplies(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "notification-fairness"}
		start := eventWaitContractEpoch
		for index := 0; index < 3*runEventNotificationPageLimit; index++ {
			notificationContractCreate(t, fixture.store, scope, fmt.Sprintf("busy-%06d", index), "thread:one", "teammate", start)
		}
		quiet := notificationContractCreate(t, fixture.store, scope, "quiet", "thread:two", "teammate", start)
		notificationContractPark(t, fixture.store, scope, start.Add(time.Second), 3*runEventNotificationPageLimit+1)
		busy := memoryEventWaitReceipt(t, scope, "busy-reply", start.Add(2*time.Second), start.Add(3*time.Second))
		if _, err := fixture.store.PublishRunEvent(t.Context(), busy); err != nil {
			t.Fatal(err)
		}
		other := memoryEventWaitReceipt(t, scope, "quiet-reply", start.Add(3*time.Second), start.Add(4*time.Second))
		other.Event.Subject = "thread:two"
		refreshMemoryEventWaitDigest(t, other)
		if _, err := fixture.store.PublishRunEvent(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		now := start.Add(5 * time.Second)
		for page := 0; page < 2; page++ {
			waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
				Scope: scope, WorkerID: fmt.Sprintf("fair-page-%d", page), Now: now, LeaseDuration: time.Minute, Limit: 100,
			})
			if err != nil {
				t.Fatal(err)
			}
			notificationContractProcessClaims(t, fixture.store, scope, now, waits)
			now = now.Add(2 * time.Second)
		}
		result, err := fixture.store.GetAgentRun(t.Context(), scope, quiet.ID)
		if err != nil || result.Status != AgentRunStatusQueued {
			t.Fatalf("large fanout starved unrelated correlated response: %#v %v", result, err)
		}
	})
}

// A tenant's ordinary signed traffic may contain no outstanding expectations.
// Clear a bounded batch of those notifications, rather than taking one worker
// tick for every irrelevant event or scanning through an unlimited backlog.
func TestRunEventNotificationDrainsBoundedEmptyTrafficBeforeMatchingReply(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "notification-empty-traffic"}
		start := eventWaitContractEpoch
		target := notificationContractCreate(t, fixture.store, scope, "actual-request", "thread:one", "teammate", start)
		notificationContractPark(t, fixture.store, scope, start.Add(time.Second), 1)
		for index := 0; index < runEventNotificationBatchLimit; index++ {
			received := start.Add(time.Duration(index+3) * time.Second)
			receipt := memoryEventWaitReceipt(t, scope, fmt.Sprintf("unrelated-%06d", index), received.Add(-time.Second), received)
			receipt.Event.Subject = fmt.Sprintf("unrelated-thread-%06d", index)
			refreshMemoryEventWaitDigest(t, receipt)
			if _, err := fixture.store.PublishRunEvent(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
		}
		now := start.Add(time.Duration(runEventNotificationBatchLimit+4) * time.Second)
		reply := memoryEventWaitReceipt(t, scope, "actual-reply", now.Add(-time.Second), now)
		if _, err := fixture.store.PublishRunEvent(t.Context(), reply); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "empty-batch", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) != 0 {
			t.Fatalf("claim read beyond its empty-notification batch budget: %v %v", waits, err)
		}
		due, err := fixture.store.ListRunEventWaitWorkScopes(t.Context(), now, 100)
		if err != nil || len(due) != 1 || due[0] != scope {
			t.Fatalf("bounded empty traffic lost the pending reply's readiness: %v %v", due, err)
		}
		now = now.Add(2 * time.Second)
		waits, err = fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "real-reply", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) != 1 || waits[0].RunID != target.ID {
			t.Fatalf("ordinary traffic delayed a useful reply beyond one bounded batch: %v %v", waits, err)
		}
		notificationContractProcessClaims(t, fixture.store, scope, now, waits)
	})
}

func TestRunEventNotificationSharesCandidateBudgetAcrossDescriptors(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "notification-shared-budget"}
		start := eventWaitContractEpoch
		const recipients = 80
		for index := 0; index < recipients; index++ {
			notificationContractCreate(t, fixture.store, scope, fmt.Sprintf("a-%06d", index), "thread:one", "teammate", start)
			notificationContractCreate(t, fixture.store, scope, fmt.Sprintf("b-%06d", index), "thread:two", "teammate", start)
		}
		notificationContractPark(t, fixture.store, scope, start.Add(time.Second), recipients*2)
		first := memoryEventWaitReceipt(t, scope, "first-resource-reply", start.Add(2*time.Second), start.Add(3*time.Second))
		second := memoryEventWaitReceipt(t, scope, "second-resource-reply", start.Add(3*time.Second), start.Add(4*time.Second))
		second.Event.Subject = "thread:two"
		refreshMemoryEventWaitDigest(t, second)
		for _, receipt := range []*RunEventReceipt{first, second} {
			if _, err := fixture.store.PublishRunEvent(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
		}
		now := start.Add(5 * time.Second)
		waits, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "shared-candidate-budget", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) != runEventNotificationPageLimit {
			t.Fatalf("shared candidate batch: %d %v", len(waits), err)
		}
		notificationContractProcessClaims(t, fixture.store, scope, now, waits)
		for index := runEventNotificationPageLimit - recipients; index < recipients; index++ {
			wait, err := fixture.store.GetRunEventWait(t.Context(), scope, fmt.Sprintf("b-%06d", index), "reply")
			if err != nil || wait.AvailableAt == nil || !wait.AvailableAt.Equal(wait.Spec.Deadline) {
				t.Fatalf("notification descriptors multiplied the total candidate budget at %d: %#v %v", index, wait, err)
			}
		}
		now = now.Add(2 * time.Second)
		waits, err = fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{
			Scope: scope, WorkerID: "remaining-candidate-budget", Now: now, LeaseDuration: time.Minute, Limit: 100,
		})
		if err != nil || len(waits) != recipients*2-runEventNotificationPageLimit {
			t.Fatalf("partial second descriptor lost remaining cursor: %d %v", len(waits), err)
		}
		notificationContractProcessClaims(t, fixture.store, scope, now, waits)
	})
}
