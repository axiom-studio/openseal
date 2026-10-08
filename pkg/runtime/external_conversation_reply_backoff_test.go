package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

func appliedReplyItem(endpoint *ExternalConversationEndpoint, id, endpointID, runID string, now time.Time) *ExternalConversationInboxItem {
	return &ExternalConversationInboxItem{
		ID: id, Scope: endpoint.Scope, EndpointID: endpointID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "ev-" + id, Type: capability.ConversationEventMessageReceived, ExternalConversationID: "C1",
			ExternalMessageID: "m-" + id, ExternalParticipantID: "U1", Text: "hi", OrderingKey: "C1:" + id, OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: "conversation", ChannelMessageID: "message-" + id, RunID: runID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
}

func TestExternalConversationReplyWorkerBacksOffAndStopsFailingItems(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend,
	})
	runs := NewRunCommandService(store)
	activity := NewRunActivityService(store, store)
	newRun := func(key string, finish bool) *AgentRun {
		created, err := runs.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
			Goal: "Respond", Source: RunSourceEvent, IdempotencyKey: key, Actor: ActivityActor{Type: "service", ID: "test"},
		})
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := activity.TransitionRun(ctx, endpoint.Scope, created.Run.ID, RunTransitionRequest{
			ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning, Summary: "Started", Actor: ActivityActor{Type: "worker", ID: "w"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if finish {
			run, _, err = activity.TransitionRun(ctx, endpoint.Scope, run.ID, RunTransitionRequest{
				ExpectedRevision: run.Revision, Status: AgentRunStatusCompleted, Summary: "Done", Actor: ActivityActor{Type: "worker", ID: "w"},
				Output: map[string]interface{}{"reply": "ok"},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	// "broken" can never be projected (its conversation does not exist);
	// "pending" waits for a Run that is still running.
	broken := appliedReplyItem(endpoint, "broken", endpoint.ID, newRun("broken-run", true).ID, now)
	pending := appliedReplyItem(endpoint, "pending", endpoint.ID, newRun("pending-run", false).ID, now)
	for _, item := range []*ExternalConversationInboxItem{broken, pending} {
		if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := NewExternalConversationReplyWorker(store, catalog)
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	worker.now = func() time.Time { return clock }
	load := func(id string) *ExternalConversationInboxItem {
		item, err := store.GetExternalConversationInboxItem(ctx, endpoint.Scope, id)
		if err != nil || item == nil {
			t.Fatal(err)
		}
		return item
	}

	failures := 0
	for pass := 0; pass < 1000 && load("broken").ReplyState == ""; pass++ {
		if _, err := worker.ProcessScope(ctx, endpoint.Scope, 100); err != nil {
			failures++
			item := load("broken")
			backedOff := item.ReplyState == "" && item.ReplyAvailableAt != nil &&
				item.ReplyAvailableAt.Equal(clock.Add(externalConversationReplyRetryDelay(failures)))
			terminal := item.ReplyState == ExternalConversationReplyFailed && item.ReplyAvailableAt == nil
			if item.ReplyAttempts != failures || !backedOff && !terminal || item.ReplyError == "" {
				t.Fatalf("failure %d recorded %+v", failures, item)
			}
		}
		clock = clock.Add(time.Second)
	}
	if failures != externalConversationReplyMaximumAttempts || load("broken").ReplyState != ExternalConversationReplyFailed {
		t.Fatalf("expected %d backed-off failures then a terminal state, got %d and %q", externalConversationReplyMaximumAttempts, failures, load("broken").ReplyState)
	}
	// Backoff means far fewer projections than one per pass (hundreds of passes).
	if failures > 10 {
		t.Fatal("failing items must back off")
	}
	if _, err := worker.ProcessScope(ctx, endpoint.Scope, 100); err != nil {
		t.Fatalf("a terminally failed item must not be retried: %v", err)
	}
	if item := load("pending"); item.ReplyState != "" || item.ReplyAttempts != 0 {
		t.Fatalf("an item waiting for its Run must stay due without penalty: %+v", item)
	}
	if item := load("broken"); item.Status != ExternalConversationInboxApplied {
		t.Fatal("reply bookkeeping must not change the inbox status other readers rely on")
	}
}

func TestExternalConversationReplyProgressIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	store, _, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend,
	})
	now := time.Now().UTC()
	item := appliedReplyItem(endpoint, "cas", endpoint.ID, "run", now)
	if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.GetExternalConversationInboxItem(ctx, endpoint.Scope, "cas")
	done := ExternalConversationReplyProgress{State: ExternalConversationReplyProjected, At: now}
	if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "cas", stored.Revision+1, done); err == nil {
		t.Fatal("a stale revision must conflict")
	}
	if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "cas", stored.Revision, done); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "cas", stored.Revision+1, done); err == nil {
		t.Fatal("a terminal reply state is final")
	}
	due, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: endpoint.Scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, ReplyDueAt: &now,
	})
	if err != nil || len(due) != 0 {
		t.Fatalf("a projected item must leave the due list: %v %v", due, err)
	}
	all, _ := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: endpoint.Scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied},
	})
	if len(all) != 1 {
		t.Fatal("other readers still see the applied item")
	}
}

type replyProgressTestStore interface {
	ExternalConversationEndpointStore
	ExternalConversationStore
	ExternalConversationReplyStateStore
}

func TestExternalConversationReplyProgressDurableStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) replyProgressTestStore
	}{
		{name: "sqlite", open: func(t *testing.T) replyProgressTestStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{name: "postgres", open: func(t *testing.T) replyProgressTestStore {
			dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL regressions")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_reply_progress_"+uuid.NewString()[:8]))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
				_ = store.Close()
			})
			return store
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := t.Context()
			store := testCase.open(t)
			_, endpoint := externalConversationEndpointFixtureOn(t, ctx, store, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, id := range []string{"done", "backoff", "due"} {
				if _, _, err := store.ReceiveExternalConversationEvent(ctx, appliedReplyItem(endpoint, id, endpoint.ID, "run-"+id, now)); err != nil {
					t.Fatal(err)
				}
			}
			load := func(id string) *ExternalConversationInboxItem {
				items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{Scope: endpoint.Scope, Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if item.ID == id {
						return item
					}
				}
				t.Fatalf("item %s missing", id)
				return nil
			}
			if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "done", load("done").Revision,
				ExternalConversationReplyProgress{State: ExternalConversationReplyProjected, At: now}); err != nil {
				t.Fatal(err)
			}
			retryAt := now.Add(time.Minute)
			if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "backoff", load("backoff").Revision,
				ExternalConversationReplyProgress{Attempts: 1, AvailableAt: &retryAt, Error: "boom", At: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveExternalConversationReplyProgress(ctx, endpoint.Scope, "done", 1,
				ExternalConversationReplyProgress{State: ExternalConversationReplyProjected, At: now}); err == nil {
				t.Fatal("stale revision must conflict")
			}
			due := func(at time.Time) []string {
				items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
					Scope: endpoint.Scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, ReplyDueAt: &at, Limit: 100,
				})
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]string, 0, len(items))
				for _, item := range items {
					ids = append(ids, item.ID)
				}
				sort.Strings(ids)
				return ids
			}
			if got := due(now); len(got) != 1 || got[0] != "due" {
				t.Fatalf("due now = %v", got)
			}
			if got := due(now.Add(2 * time.Minute)); len(got) != 2 || got[0] != "backoff" || got[1] != "due" {
				t.Fatalf("due after backoff = %v", got)
			}
			if item := load("backoff"); item.ReplyAttempts != 1 || item.ReplyError != "boom" || item.Status != ExternalConversationInboxApplied {
				t.Fatalf("progress not persisted: %+v", item)
			}
		})
	}
}
