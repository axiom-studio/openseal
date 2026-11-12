package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// replyGuardFixture is a running Run (actiontest.LoopStore) plus the channel its
// final reply is posted to.
type replyGuardFixture struct {
	*actiontest.LoopStore
	conversations models.ConversationStore
	conversation  *models.Conversation
}

// replyGuardScenario runs scenario on SQLite and (with a DSN) Postgres,
// both of which hold Runs and conversations in one store.
func replyGuardScenario(t *testing.T, scenario func(*testing.T, *replyGuardFixture)) {
	t.Helper()
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) { scenario(t, newReplyGuardFixture(t, store)) })
}

func newReplyGuardFixture(t *testing.T, store models.KernelStore) *replyGuardFixture {
	t.Helper()
	conversations, ok := store.(models.ConversationStore)
	if !ok {
		t.Fatalf("%T is not a ConversationStore", store)
	}
	f := &replyGuardFixture{LoopStore: actiontest.NewLoopStore(t, store), conversations: conversations}
	seedTestSpace(t, store.(models.SpaceStore), f.Scope)
	conversation, _, err := conversation.NewService(conversations).CreateConversation(f.Ctx, conversation.CreateConversationRequest{
		ID: "chat-" + uuid.NewString()[:8], Scope: f.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"},
		Title: "Reply guard", IdempotencyKey: "reply-guard-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	f.conversation = conversation
	return f
}

func (f *replyGuardFixture) guard() *models.ReplyRunGuard {
	return &models.ReplyRunGuard{Scope: f.Scope, RunID: f.Run.ID, WorkerID: actiontest.LoopStoreWorker, ConcurrencyKey: f.Run.ConcurrencyKey,
		InterventionCount: len(f.Run.PendingInterventions)}
}

// reply is the final reply commit for the current conversation revision.
func (f *replyGuardFixture) reply(key string, guard *models.ReplyRunGuard) models.ChannelMessageCommitRecord {
	f.T.Helper()
	current, err := f.conversations.GetConversation(f.Ctx, f.Scope, f.conversation.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	now := time.Now().UTC()
	message := &models.ChannelMessage{
		ID: "message-" + key, Scope: f.Scope, ConversationID: current.ID, Sequence: current.LastSequence + 1,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent"}, Intent: models.MessageIntentAnswer,
		Content: "Here is the answer", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		IdempotencyKey: key, CreatedAt: now,
	}
	next := models.CloneConversation(current)
	next.Revision, next.LastSequence, next.UpdatedAt = current.Revision+1, message.Sequence, now
	return models.ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Message: message, RunGuard: guard}
}

// mutateRun rewrites the Run outside its lease.
func (f *replyGuardFixture) mutateRun(mutate func(*models.AgentRun)) {
	f.T.Helper()
	current := f.CurrentRun()
	updated := models.CloneAgentRun(current)
	mutate(updated)
	updated.Revision++
	updated.UpdatedAt = f.Now
	if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, updated, current.Revision, f.Event("mutate-"+uuid.NewString()[:8], "run.updated"), nil); err != nil {
		f.T.Fatal(err)
	}
	f.Run = f.CurrentRun()
}

func (f *replyGuardFixture) requireNotCommitted(key string, before *models.Conversation) {
	f.T.Helper()
	message, err := f.conversations.FindChannelMessageByIdempotencyKey(f.Ctx, f.Scope, f.conversation.ID, key)
	if err != nil || message != nil {
		f.T.Fatalf("guarded reply committed: %#v, %v", message, err)
	}
	after, err := f.conversations.GetConversation(f.Ctx, f.Scope, f.conversation.ID)
	if err != nil || after.Revision != before.Revision || after.LastSequence != before.LastSequence {
		f.T.Fatalf("conversation changed by refused reply: %#v -> %#v, %v", before, after, err)
	}
}

func TestReplyRunGuardCommitsWhileRunHolds(t *testing.T) {
	replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
		store := f.conversations
		result, err := store.CommitChannelMessage(f.Ctx, f.reply("final", f.guard()))
		if err != nil || result.Replayed || result.Message.IdempotencyKey != "final" {
			t.Fatalf("guarded reply = %#v, %v", result, err)
		}
	})
}

func TestReplyRunGuardRefusesWithoutCommit(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*replyGuardFixture, *models.ReplyRunGuard)
	}{
		{"intervention arrived", func(f *replyGuardFixture, _ *models.ReplyRunGuard) {
			f.mutateRun(func(run *models.AgentRun) {
				run.PendingInterventions = append(run.PendingInterventions, models.AgentRunIntervention{
					ID: "steer-1", Actor: models.ActivityActor{Type: "user", ID: "user"}, Instruction: "Use the other file", CreatedAt: f.Now,
				})
			})
		}},
		{"other worker", func(_ *replyGuardFixture, guard *models.ReplyRunGuard) { guard.WorkerID = "other-worker" }},
		{"lease expired", func(f *replyGuardFixture, _ *models.ReplyRunGuard) {
			f.mutateRun(func(run *models.AgentRun) {
				expired := time.Now().UTC().Add(-time.Minute)
				run.LeaseExpiresAt = &expired
			})
		}},
		{"lane moved", func(f *replyGuardFixture, _ *models.ReplyRunGuard) {
			f.mutateRun(func(run *models.AgentRun) { run.ConcurrencyKey = "conversation-task:promoted" })
		}},
		{"run parked", func(f *replyGuardFixture, _ *models.ReplyRunGuard) {
			f.SetRunStatus(models.AgentRunStatusWaitingForEvent)
		}},
		{"run canceled", func(f *replyGuardFixture, _ *models.ReplyRunGuard) { f.SetRunStatus(models.AgentRunStatusCanceled) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
				store := f.conversations
				guard := f.guard()
				tc.setup(f, guard)
				before, err := store.GetConversation(f.Ctx, f.Scope, f.conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.CommitChannelMessage(f.Ctx, f.reply("final", guard)); !errors.Is(err, models.ErrSteeringArrived) {
					t.Fatalf("guarded reply error = %v, want ErrSteeringArrived", err)
				}
				f.requireNotCommitted("final", before)
			})
		})
	}
}

func TestReplyRunGuardReplayReturnsStoredMessageWithoutGuard(t *testing.T) {
	replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
		store := f.conversations
		record := f.reply("final", f.guard())
		first, err := store.CommitChannelMessage(f.Ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		// The Run has since moved on: the guard would now fail, but the
		// reply is already posted, so the replay returns it.
		f.SetRunStatus(models.AgentRunStatusCompleted)
		replayed, err := store.CommitChannelMessage(f.Ctx, record)
		if err != nil || !replayed.Replayed || replayed.Message.ID != first.Message.ID {
			t.Fatalf("replay = %#v, %v", replayed, err)
		}
	})
}

func TestReplyRunGuardMissingRunAndNilGuard(t *testing.T) {
	replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
		store := f.conversations
		missing := f.guard()
		missing.RunID = "no-such-run"
		if _, err := store.CommitChannelMessage(f.Ctx, f.reply("missing", missing)); !errors.Is(err, models.ErrRunNotFound) {
			t.Fatalf("missing Run error = %v", err)
		}
		if _, err := store.CommitChannelMessage(f.Ctx, f.reply("invalid", &models.ReplyRunGuard{Scope: f.Scope, RunID: f.Run.ID})); !errors.Is(err, models.ErrInvalidConversation) {
			t.Fatalf("guard without worker error = %v", err)
		}
		// Without a guard the Run is not consulted at all.
		f.SetRunStatus(models.AgentRunStatusCanceled)
		if result, err := store.CommitChannelMessage(f.Ctx, f.reply("unguarded", nil)); err != nil || result.Replayed {
			t.Fatalf("unguarded commit = %#v, %v", result, err)
		}
	})
}

// TestGuardedReplyLocksRunBeforeConversation holds the Run row and checks
// that the guarded commit waits on it without having taken the
// conversation's advisory lock or row (SPEC §4.1 lock order).
func TestGuardedReplyLocksRunBeforeConversation(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_reply_guard_")
	dsn := pgSchema.DSN
	store, err := postgres.New(context.Background(), dsn, postgres.WithSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	f := newReplyGuardFixture(t, store)
	ctx, cancel := context.WithTimeout(f.Ctx, 20*time.Second)
	defer cancel()
	record := f.reply("final", f.guard())

	blocker, err := pgSchema.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var revision int64
	if err := blocker.QueryRowContext(ctx, `SELECT revision FROM `+pgSchema.Table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`,
		f.Scope.Kind, f.Scope.ID, f.Run.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.CommitChannelMessage(ctx, record)
		done <- err
	}()
	for {
		var blocked int
		if err := pgSchema.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND strpos(query,$1)>0 AND strpos(query,'FOR UPDATE')>0 AND pid<>pg_backend_pid()`,
			pgSchema.Table("agent_runs")).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("guarded commit finished while the Run was locked: %v", err)
		case <-ctx.Done():
			t.Fatal("guarded commit never waited on the Run row")
		case <-time.After(10 * time.Millisecond):
		}
	}
	probe, err := pgSchema.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var free bool
	if err := probe.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`,
		"openseal:conversation:"+f.Scope.Key()+":message:"+record.Message.ConversationID+":"+record.Message.IdempotencyKey).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("guarded commit took the conversation advisory lock before the Run row")
	}
	if _, err := probe.ExecContext(ctx, `SELECT 1 FROM `+pgSchema.Table("conversations")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE NOWAIT`,
		f.Scope.Kind, f.Scope.ID, f.conversation.ID); err != nil {
		t.Fatalf("guarded commit locked the conversation row before the Run row: %v", err)
	}
	_ = probe.Rollback()
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("guarded commit after the Run was released: %v", err)
	}
}
