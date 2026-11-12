package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ConversationTaskDueStore is what the due benchmark reads and writes.
type ConversationTaskDueStore interface {
	ConversationTaskStore
	models.ConversationTaskDueStore
}

// ConversationTaskDueExplain checks the backend's plan for the due query of
// filter. Only the backend knows its query, so each passes its own.
type ConversationTaskDueExplain func(b *testing.B, filter models.ConversationTaskDueFilter)

// BenchmarkConversationTaskDue times the production due query at its page
// limit, also after a keyset cursor, over 100 and 10,000 completed Runs.
// Only fixture construction writes history. open returns a fresh store and
// the plan check for it.
func BenchmarkConversationTaskDue(b *testing.B, open func(*testing.B) (ConversationTaskDueStore, ConversationTaskDueExplain)) {
	for _, history := range []int{100, 10000} {
		b.Run(fmt.Sprintf("completed_%d", history), func(b *testing.B) {
			store, explain := open(b)
			scope := models.Scope{Kind: "tenant", ID: "continuation-benchmark"}
			base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
			active := make([]*models.AgentRun, 0, 5)
			// Completed records are older than the live records. A full history
			// scan cannot be hidden by an early LIMIT at the live frontier.
			for index := 0; index < history; index++ {
				conversationTaskDueRun(b, store, scope, fmt.Sprintf("completed-%08d", index), base.Add(time.Duration(index)*time.Microsecond), true)
			}
			for index := 0; index < 5; index++ {
				active = append(active, conversationTaskDueRun(b, store, scope, fmt.Sprintf("active-%08d", index), base.Add(time.Hour+time.Duration(index)*time.Microsecond), false))
			}
			filter := models.ConversationTaskDueFilter{Scope: scope, BeforeCreatedAt: time.Now().UTC().Add(-models.ConversationTaskForegroundTimeout), Limit: 100}
			cursor := filter
			cursor.AfterCreatedAt = &active[1].CreatedAt
			cursor.AfterID = active[1].ID
			explain(b, filter)
			explain(b, cursor)
			b.Run("five_active", func(b *testing.B) { conversationTaskDuePage(b, store, filter, 5) })
			b.Run("three_after_cursor", func(b *testing.B) { conversationTaskDuePage(b, store, cursor, 3) })
			for _, run := range active {
				CompleteRun(b, store, run)
			}
			explain(b, filter)
			b.Run("none_same_history", func(b *testing.B) { conversationTaskDuePage(b, store, filter, 0) })
		})
	}
}

func conversationTaskDueRun(b *testing.B, store ConversationTaskDueStore, scope models.Scope, id string, created time.Time, completed bool) *models.AgentRun {
	b.Helper()
	run := &models.AgentRun{ID: id, Scope: scope, Kind: models.RunKindConversation, RootRunID: id, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "benchmark-agent"}, AssignedAgentID: "benchmark-agent",
		ConcurrencyKey: "benchmark-conversation", Goal: "Continue the accepted conversation request", Source: models.RunSourceChat, Status: models.AgentRunStatusQueued,
		AvailableAt: created, QueueEnteredAt: created, CreatedAt: created, UpdatedAt: created, Revision: 1,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "benchmark-conversation", models.ConversationRunTriggerIDContextKey: "benchmark-message-" + id}}
	if completed {
		run.Status = models.AgentRunStatusCompleted
		run.CompletedAt = &created
	}
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		b.Fatal(err)
	}
	return run
}

func conversationTaskDuePage(b *testing.B, store ConversationTaskDueStore, filter models.ConversationTaskDueFilter, want int) {
	b.Helper()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		page, err := store.ListDueConversationTaskRuns(context.Background(), filter)
		if err != nil || len(page) != want || len(page) > 100 {
			b.Fatalf("due page=%d want=%d err=%v", len(page), want, err)
		}
	}
	b.ReportMetric(float64(want), "runs/op")
}
