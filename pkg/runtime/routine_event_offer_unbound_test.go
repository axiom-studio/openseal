package runtime

import (
	"context"
	"testing"
)

func unboundConversationMessage() (Scope, ExternalConversationAdapterReference, *NormalizedExternalConversationEvent) {
	event := slackRoutineMessage("173.001", "U1", "Ana Ruiz", "hello", false)
	return Scope{Kind: "tenant", ID: "busy"},
		ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "binding", BindingRevision: 1, AdapterID: "slack"},
		&event
}

// TestConversationRoutineOfferUnboundAllocFree: a Slack message on a
// connection no routine listens to makes no store call and allocates nothing
// in the routine path (SPEC §0a).
func TestConversationRoutineOfferUnboundAllocFree(t *testing.T) {
	matcher, store := newUnboundRoutineEventMatcher(t)
	scope, adapter, event := unboundConversationMessage()
	ctx := context.Background()
	allocs := testing.AllocsPerRun(1000, func() {
		if err := offerConversationRoutineEvent(ctx, matcher, scope, "deployment", adapter, "slack", "installation", event); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("unbound conversation offer allocates %.1f times", allocs)
	}
	if store.calls != 0 {
		t.Fatalf("unbound conversation offer made %d store calls", store.calls)
	}
	if stats := matcher.RoutineEventStats(); stats.In[RoutineEventSlackMessage] != 0 {
		t.Fatalf("unbound message reached the matcher: in=%d", stats.In[RoutineEventSlackMessage])
	}
}

func BenchmarkConversationRoutineOfferUnbound(b *testing.B) {
	matcher, _ := newUnboundRoutineEventMatcher(b)
	scope, adapter, event := unboundConversationMessage()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := offerConversationRoutineEvent(ctx, matcher, scope, "deployment", adapter, "slack", "installation", event); err != nil {
			b.Fatal(err)
		}
	}
}
