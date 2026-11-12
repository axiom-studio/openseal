package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// replyGuardFixture is a running Run (loopStoreFixture) plus the channel its
// final reply is posted to.
type replyGuardFixture struct {
	*loopStoreFixture
	conversations ConversationStore
	conversation  *Conversation
}

// replyGuardScenario runs scenario on SQLite and (with a DSN) Postgres,
// both of which hold Runs and conversations in one store.
func replyGuardScenario(t *testing.T, scenario func(*testing.T, *replyGuardFixture)) {
	t.Helper()
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) { scenario(t, newReplyGuardFixture(t, store)) })
}

func newReplyGuardFixture(t *testing.T, store KernelStore) *replyGuardFixture {
	t.Helper()
	conversations, ok := store.(ConversationStore)
	if !ok {
		t.Fatalf("%T is not a ConversationStore", store)
	}
	f := &replyGuardFixture{loopStoreFixture: newLoopStoreFixture(t, store), conversations: conversations}
	seedTestSpace(t, store.(SpaceStore), f.scope)
	conversation, _, err := NewConversationService(conversations).CreateConversation(f.ctx, CreateConversationRequest{
		ID: "chat-" + uuid.NewString()[:8], Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"},
		Title: "Reply guard", IdempotencyKey: "reply-guard-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	f.conversation = conversation
	return f
}

func (f *replyGuardFixture) guard() *ReplyRunGuard {
	return &ReplyRunGuard{Scope: f.scope, RunID: f.run.ID, WorkerID: loopStoreWorker, ConcurrencyKey: f.run.ConcurrencyKey,
		InterventionCount: len(f.run.PendingInterventions)}
}

// reply is the final reply commit for the current conversation revision.
func (f *replyGuardFixture) reply(key string, guard *ReplyRunGuard) ChannelMessageCommitRecord {
	f.t.Helper()
	current, err := f.conversations.GetConversation(f.ctx, f.scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now().UTC()
	message := &ChannelMessage{
		ID: "message-" + key, Scope: f.scope, ConversationID: current.ID, Sequence: current.LastSequence + 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}, Intent: MessageIntentAnswer,
		Content: "Here is the answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: key, CreatedAt: now,
	}
	next := cloneConversation(current)
	next.Revision, next.LastSequence, next.UpdatedAt = current.Revision+1, message.Sequence, now
	return ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Message: message, RunGuard: guard}
}

// mutateRun rewrites the Run outside its lease.
func (f *replyGuardFixture) mutateRun(mutate func(*AgentRun)) {
	f.t.Helper()
	current := f.currentRun()
	updated := cloneAgentRun(current)
	mutate(updated)
	updated.Revision++
	updated.UpdatedAt = f.now
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, f.event("mutate-"+uuid.NewString()[:8], "run.updated"), nil); err != nil {
		f.t.Fatal(err)
	}
	f.run = f.currentRun()
}

func (f *replyGuardFixture) requireNotCommitted(key string, before *Conversation) {
	f.t.Helper()
	message, err := f.conversations.FindChannelMessageByIdempotencyKey(f.ctx, f.scope, f.conversation.ID, key)
	if err != nil || message != nil {
		f.t.Fatalf("guarded reply committed: %#v, %v", message, err)
	}
	after, err := f.conversations.GetConversation(f.ctx, f.scope, f.conversation.ID)
	if err != nil || after.Revision != before.Revision || after.LastSequence != before.LastSequence {
		f.t.Fatalf("conversation changed by refused reply: %#v -> %#v, %v", before, after, err)
	}
}

func TestReplyRunGuardCommitsWhileRunHolds(t *testing.T) {
	replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
		store := f.conversations
		result, err := store.CommitChannelMessage(f.ctx, f.reply("final", f.guard()))
		if err != nil || result.Replayed || result.Message.IdempotencyKey != "final" {
			t.Fatalf("guarded reply = %#v, %v", result, err)
		}
	})
}

func TestReplyRunGuardRefusesWithoutCommit(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*replyGuardFixture, *ReplyRunGuard)
	}{
		{"intervention arrived", func(f *replyGuardFixture, _ *ReplyRunGuard) {
			f.mutateRun(func(run *AgentRun) {
				run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{
					ID: "steer-1", Actor: ActivityActor{Type: "user", ID: "user"}, Instruction: "Use the other file", CreatedAt: f.now,
				})
			})
		}},
		{"other worker", func(_ *replyGuardFixture, guard *ReplyRunGuard) { guard.WorkerID = "other-worker" }},
		{"lease expired", func(f *replyGuardFixture, _ *ReplyRunGuard) {
			f.mutateRun(func(run *AgentRun) {
				expired := time.Now().UTC().Add(-time.Minute)
				run.LeaseExpiresAt = &expired
			})
		}},
		{"lane moved", func(f *replyGuardFixture, _ *ReplyRunGuard) {
			f.mutateRun(func(run *AgentRun) { run.ConcurrencyKey = "conversation-task:promoted" })
		}},
		{"run parked", func(f *replyGuardFixture, _ *ReplyRunGuard) { f.setRunStatus(AgentRunStatusWaitingForEvent) }},
		{"run canceled", func(f *replyGuardFixture, _ *ReplyRunGuard) { f.setRunStatus(AgentRunStatusCanceled) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replyGuardScenario(t, func(t *testing.T, f *replyGuardFixture) {
				store := f.conversations
				guard := f.guard()
				tc.setup(f, guard)
				before, err := store.GetConversation(f.ctx, f.scope, f.conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.CommitChannelMessage(f.ctx, f.reply("final", guard)); !errors.Is(err, ErrSteeringArrived) {
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
		first, err := store.CommitChannelMessage(f.ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		// The Run has since moved on: the guard would now fail, but the
		// reply is already posted, so the replay returns it.
		f.setRunStatus(AgentRunStatusCompleted)
		replayed, err := store.CommitChannelMessage(f.ctx, record)
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
		if _, err := store.CommitChannelMessage(f.ctx, f.reply("missing", missing)); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("missing Run error = %v", err)
		}
		if _, err := store.CommitChannelMessage(f.ctx, f.reply("invalid", &ReplyRunGuard{Scope: f.scope, RunID: f.run.ID})); !errors.Is(err, ErrInvalidConversation) {
			t.Fatalf("guard without worker error = %v", err)
		}
		// Without a guard the Run is not consulted at all.
		f.setRunStatus(AgentRunStatusCanceled)
		if result, err := store.CommitChannelMessage(f.ctx, f.reply("unguarded", nil)); err != nil || result.Replayed {
			t.Fatalf("unguarded commit = %#v, %v", result, err)
		}
	})
}

// TestGuardedReplyLocksRunBeforeConversation holds the Run row and checks
// that the guarded commit waits on it without having taken the
// conversation's advisory lock or row (SPEC §4.1 lock order).
func TestGuardedReplyLocksRunBeforeConversation(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OPENSEAL_TEST_POSTGRES_DSN is not set")
	}
	store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema("openseal_reply_guard_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	f := newReplyGuardFixture(t, store)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	record := f.reply("final", f.guard())

	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var revision int64
	if err := blocker.QueryRowContext(ctx, `SELECT revision FROM `+store.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`,
		f.scope.Kind, f.scope.ID, f.run.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.CommitChannelMessage(ctx, record)
		done <- err
	}()
	for {
		var blocked int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND strpos(query,$1)>0 AND strpos(query,'FOR UPDATE')>0 AND pid<>pg_backend_pid()`,
			store.table("agent_runs")).Scan(&blocked); err != nil {
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
	probe, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var free bool
	if err := probe.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`,
		"openseal:conversation:"+f.scope.Key()+":message:"+record.Message.ConversationID+":"+record.Message.IdempotencyKey).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("guarded commit took the conversation advisory lock before the Run row")
	}
	if _, err := probe.ExecContext(ctx, `SELECT 1 FROM `+store.table("conversations")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE NOWAIT`,
		f.scope.Kind, f.scope.ID, f.conversation.ID); err != nil {
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
