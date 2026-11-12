package routine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRoutineEventBatchHeader(t *testing.T) {
	records := func(summaries ...string) ([]string, []*models.RoutineEventRecord) {
		ids := make([]string, 0, len(summaries))
		values := make([]*models.RoutineEventRecord, 0, len(summaries))
		for i, summary := range summaries {
			id := string(rune('a' + i))
			ids = append(ids, id)
			values = append(values, &models.RoutineEventRecord{Event: models.RoutineEvent{ID: id, Summary: summary}})
		}
		return ids, values
	}
	ids, values := records("New email from Ana Ruiz: Invoice #4411")
	if got := routineEventBatchHeader(models.RoutineEventGmailMessage, &models.RoutineEventBatch{EventIDs: ids}, values); got != "New email from Ana Ruiz: Invoice #4411" {
		t.Fatalf("one event header = %q", got)
	}
	ids, values = records("a", "b", "c", "d")
	for kind, want := range map[models.RoutineEventKind]string{
		models.RoutineEventGmailMessage: "4 new emails", models.RoutineEventSlackMessage: "4 new messages", models.RoutineEventTelegramMessage: "4 new messages",
		models.RoutineEventGitHub: "4 new events", models.RoutineEventCalendar: "4 new events", models.RoutineEventWebhook: "4 new events",
	} {
		if got := routineEventBatchHeader(kind, &models.RoutineEventBatch{EventIDs: ids}, values); got != want {
			t.Fatalf("%s header = %q, want %q", kind, got, want)
		}
	}
	ids, values = records("Test event")
	if got := routineEventBatchHeader(models.RoutineEventWebhook, &models.RoutineEventBatch{EventIDs: ids, Test: true}, values); got != "Test event" {
		t.Fatalf("one test header = %q", got)
	}
	ids, values = records("Test event", "Test event")
	if got := routineEventBatchHeader(models.RoutineEventWebhook, &models.RoutineEventBatch{EventIDs: ids, Test: true}, values); got != "2 test events" {
		t.Fatalf("two test header = %q", got)
	}
	ids, values = records(strings.Repeat("é", 150))
	got := routineEventBatchHeader(models.RoutineEventWebhook, &models.RoutineEventBatch{EventIDs: ids}, values)
	if len(got) > RoutineEventHeaderMaxBytes || !strings.HasPrefix(strings.Repeat("é", 150), got) || len(got) != 200 {
		t.Fatalf("long header has %d bytes: %q", len(got), got)
	}
}

// passCountingStore counts dispatcher passes on the leader.
type passCountingStore struct {
	*sqlite.Store
	passes atomic.Int64
}

func (s *passCountingStore) ListRoutineEventWorkScopes(ctx context.Context) ([]models.Scope, error) {
	s.passes.Add(1)
	return s.Store.ListRoutineEventWorkScopes(ctx)
}

func TestRoutineEventDispatcherCoalescesNudges(t *testing.T) {
	store := &passCountingStore{Store: kerneltest.NewStore(t)}
	matcher := NewRoutineEventMatcher(store)
	t.Cleanup(matcher.Close)
	dispatcher := NewRoutineEventDispatcher(store, matcher)
	dispatcher.minPassGap = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx, time.Hour, nil) }()
	// A steady stream of inserts for 250 ms.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		matcher.wake()
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if passes := store.passes.Load(); passes < 2 || passes > 4 {
		t.Fatalf("passes = %d, want one per 100 ms gap", passes)
	}
}
