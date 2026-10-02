package runtime

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

func assertMemoryRunEventOrderBalanced(t *testing.T, node *memoryRunEventOrderNode, minimum, maximum *memoryRunEventOrderKey) int {
	t.Helper()
	if node == nil {
		return 0
	}
	if minimum != nil && !memoryRunEventOrderLess(*minimum, node.key) || maximum != nil && !memoryRunEventOrderLess(node.key, *maximum) {
		t.Fatal("chronological index lost strict ordering")
	}
	left := assertMemoryRunEventOrderBalanced(t, node.left, minimum, &node.key)
	right := assertMemoryRunEventOrderBalanced(t, node.right, &node.key, maximum)
	if node.height != 1+max(left, right) || left-right > 1 || right-left > 1 {
		t.Fatalf("unbalanced chronological index: height=%d left=%d right=%d", node.height, left, right)
	}
	return node.height
}

func TestMemoryRunEventChronologicalIndexInsertionDeletionAndBoundedSeek(t *testing.T) {
	store := NewMemoryStore()
	start := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	const count = 4096
	keys := make([]memoryRunEventOrderKey, 0, count)
	for i := 0; i < count; i++ {
		scope := Scope{Kind: "tenant", ID: fmt.Sprintf("scope:%d", i%3)}
		receipt := memoryEventWaitReceipt(t, scope, fmt.Sprintf("event:%04d", i), start, start.Add(time.Duration(i%7)*time.Second))
		if _, err := store.PublishRunEvent(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, memoryRunEventOrderKey{ReceivedAt: receipt.ReceivedAt, Identity: memoryRunEventReceiptIdentity(receipt)})
		if i%128 == 0 {
			assertMemoryRunEventOrderBalanced(t, store.runEventReceiptOrder, nil, nil)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return memoryRunEventOrderLess(keys[i], keys[j]) })
	before := start.Add(time.Minute)
	for _, position := range []int{0, 17, 1024, count - 5} {
		after := &keys[position]
		batch := store.memoryRunEventReceiptsAfterLocked(after, before, 7, nil)
		expected := min(7, count-position-1)
		if len(batch) != expected {
			t.Fatalf("bounded seek returned %d rows, expected %d", len(batch), expected)
		}
		for i, key := range batch {
			if key != keys[position+i+1] {
				t.Fatal("bounded seek skipped or repeated an event identity")
			}
		}
	}
	scope := Scope{Kind: "tenant", ID: "scope:1"}
	for _, key := range store.memoryRunEventReceiptsAfterLocked(nil, before, 13, &scope) {
		if key.Identity.Scope != scope {
			t.Fatal("scoped chronological seek crossed tenants")
		}
	}
	permutation := rand.New(rand.NewSource(19)).Perm(count)
	for i, position := range permutation {
		store.deleteMemoryRunEventReceiptLocked(keys[position].Identity)
		if i%128 == 0 {
			assertMemoryRunEventOrderBalanced(t, store.runEventReceiptOrder, nil, nil)
			for _, root := range store.runEventReceiptScopeOrder {
				assertMemoryRunEventOrderBalanced(t, root, nil, nil)
			}
		}
	}
	if store.runEventReceiptOrder != nil || len(store.runEventReceiptScopeOrder) != 0 || len(store.runEventReceipts) != 0 || len(store.runEventReceiptIndex) != 0 {
		t.Fatal("deletion left chronological index or receipt entries behind")
	}
}

func TestMemoryRunEventScopedRetentionBoundsProtectsAndPreservesConsumption(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "retention"}
	start := time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC)
	run := memoryEventWaitRun(scope, "run", "wait", start)
	if err := store.CreateAgentRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	needed := memoryEventWaitReceipt(t, scope, "needed", start, start)
	if _, err := store.PublishRunEvent(t.Context(), needed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		receipt := memoryEventWaitReceipt(t, scope, fmt.Sprintf("unrelated:%d", i), start, start.Add(time.Duration(i+1)*time.Second))
		receipt.Event.Subject = "unrelated"
		refreshMemoryEventWaitDigest(t, receipt)
		if _, err := store.PublishRunEvent(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
	}
	if deleted, err := store.PruneRunEvents(t.Context(), scope, start.Add(RunEventRetention), 1); err != nil || deleted != 0 {
		t.Fatalf("receipt pruned before retention: deleted=%d err=%v", deleted, err)
	}
	now := start.Add(RunEventRetention + time.Minute)
	if deleted, err := store.PruneRunEvents(t.Context(), scope, now, 1); err != nil || deleted != 0 {
		t.Fatalf("protected buffered reply pruned: deleted=%d err=%v", deleted, err)
	}
	if cursor := store.runEventReceiptPruneCursors[scope]; cursor == nil || cursor.Identity.ID != "needed" {
		t.Fatal("bounded maintenance did not advance past a protected receipt")
	}
	for i := 0; i < 2; i++ {
		if deleted, err := store.PruneRunEvents(t.Context(), scope, now, 1); err != nil || deleted != 1 {
			t.Fatalf("protected receipt starved later cleanup: deleted=%d err=%v", deleted, err)
		}
	}
	result := processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, now, "worker"), now)
	if result.Wait.Status != RunEventWaitMatched {
		t.Fatal("timely reply was lost to delayed cleanup")
	}
	deleted := 0
	for i := 0; i < 3; i++ {
		count, err := store.PruneRunEvents(t.Context(), scope, now, 1)
		if err != nil {
			t.Fatal(err)
		}
		deleted += count
	}
	if deleted != 1 || len(store.runEventReceipts) != 0 || len(store.runEventConsumptions) != 1 {
		t.Fatal("cleanup failed to archive payload while retaining consumption identity")
	}
	if inserted, err := store.PublishRunEvent(t.Context(), needed); err != nil || !inserted {
		t.Fatalf("republish archived receipt: inserted=%v err=%v", inserted, err)
	}
	next := memoryEventWaitRun(scope, run.ID, "next", start)
	next.Revision, next.UpdatedAt = result.Run.Revision+1, now
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), next, result.Run.Revision, memoryEventWaitActivity(next), nil); err != nil {
		t.Fatal(err)
	}
	result = processMemoryEventWait(t, store, claimMemoryEventWait(t, store, scope, now, "worker"), now)
	if result.Wait.Status != RunEventWaitTimedOut || result.Event != nil {
		t.Fatal("republished archived event was consumed twice by the same Run")
	}
}
