package runtime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// promotionSessionStore is a continuation test store that also holds the
// session log (every backend implements both).
type promotionSessionStore interface {
	conversationContinuationTestStore
	models.AgentSessionStore
}

const promotionTestWorker = "same-worker"

func promotionLease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: promotionTestWorker, Now: time.Now()}
}

// promotionSessionFixture is a running chat Run bound to its lane session.
// With entries > 0 the chat session row exists and holds that many notes of
// the Run; entries == 0 opens an empty row; entries < 0 leaves no row.
func promotionSessionFixture(t *testing.T, base conversationContinuationTestStore, entries int) (promotionSessionStore, *models.AgentRun, *models.Conversation, *models.ChannelMessage, *models.AgentSession) {
	t.Helper()
	store := base.(promotionSessionStore)
	run, conversation, trigger := conversationContinuationFixture(t, store, models.AgentRunStatusRunning, false)
	previous := run.Revision
	run.SessionID = models.ConversationSessionID(run.Scope, run.ConcurrencyKey)
	run.Revision++
	updateConversationTaskSQLRun(t, store, run, previous)
	run, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entries < 0 {
		return store, run, conversation, trigger, nil
	}
	opened, err := store.OpenConversationAgentSession(t.Context(), models.OpenConversationAgentSessionRequest{Scope: run.Scope, ConversationID: conversation.ID,
		AgentID: run.AssignedAgentID, RunID: run.ID, Lease: promotionLease(), Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	chat := opened.Session
	if entries > 0 {
		batch := make([]*models.SessionEntry, entries)
		for i := range batch {
			batch[i] = storetest.NoteSessionEntry(models.SessionEntryID(chat.ID, "chat-note:"+string(rune('a'+i))), models.NoteHost)
		}
		lease := promotionLease()
		appended, err := store.AppendAgentSessionEntries(t.Context(), models.AppendAgentSessionEntriesRequest{Scope: run.Scope, SessionID: chat.ID, RunID: run.ID,
			Lease: &lease, Entries: batch, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		chat = appended.Session
	}
	return store, run, conversation, trigger, chat
}

func promoteAtDeadline(t *testing.T, store promotionSessionStore, run *models.AgentRun) *models.ConversationTaskResult {
	t.Helper()
	result, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID,
		Now: run.CreatedAt.Add(models.ConversationTaskForegroundTimeout)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func promotionSessionEntries(t *testing.T, store promotionSessionStore, scope models.Scope, sessionID string) []*models.SessionEntry {
	t.Helper()
	entries, err := store.ListAgentSessionEntries(t.Context(), models.SessionEntryFilter{Scope: scope, SessionID: sessionID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestPromotionRebindsRunToChildSession(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, reopen func(*testing.T) conversationContinuationTestStore) {
		store, run, _, trigger, chat := promotionSessionFixture(t, base, 2)
		parentEntries := promotionSessionEntries(t, store, run.Scope, chat.ID)
		accepted := promoteAtDeadline(t, store, run)
		if accepted == nil {
			t.Fatal("Run was not promoted")
		}
		childID := models.RunAgentSessionID(run.Scope, run.ID)
		stored, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if accepted.WorkRun.SessionID != childID || stored.SessionID != childID || stored.Revision != run.Revision+1 ||
			stored.ConcurrencyKey != "task:"+accepted.Task.ID || stored.LeaseOwner != run.LeaseOwner || stored.Status != run.Status {
			t.Fatalf("promotion did not rebind only the session and lane: %#v", stored)
		}
		child, err := store.GetAgentSession(t.Context(), run.Scope, childID)
		if err != nil {
			t.Fatal(err)
		}
		if child.Kind != models.AgentSessionKindRun || child.OwnerRunID != run.ID || child.AgentID != run.AssignedAgentID ||
			child.ParentSessionID != chat.ID || child.ForkSequence != 2 || child.ForkEntryID != parentEntries[1].ID ||
			child.ChannelWatermark != chat.ChannelWatermark || child.NextSequence != 1 {
			t.Fatalf("child session is not a fork at the chat leaf: %#v", child)
		}
		chatEntries := promotionSessionEntries(t, store, run.Scope, chat.ID)
		if len(chatEntries) != 3 {
			t.Fatalf("chat session entries = %d, want 3", len(chatEntries))
		}
		note := chatEntries[2]
		if note.Type != models.SessionEntryNote || note.Note == nil || note.Note.Kind != models.NoteTaskPromoted || note.RunID != run.ID ||
			note.SourceKey != models.TaskPromotedSourceKey(run.ID) || note.ID != models.SessionEntryID(chat.ID, models.TaskPromotedSourceKey(run.ID)) ||
			note.Sequence != 3 || note.Note.Text != "Your work on message "+trigger.ID+" continues in background task "+accepted.Task.ID+
			"; its result will be posted in this chat when it finishes." {
			t.Fatalf("task_promoted note: %#v", note)
		}
		var data map[string]string
		if err := json.Unmarshal(note.Note.Data, &data); err != nil || data["taskId"] != accepted.Task.ID || data["runId"] != run.ID || data["messageId"] != trigger.ID {
			t.Fatalf("task_promoted data: %s %v", note.Note.Data, err)
		}
		path, err := store.LoadAgentSessionPath(t.Context(), run.Scope, childID)
		if err != nil {
			t.Fatal(err)
		}
		if storetest.EntryIDs(path.Entries) != storetest.EntryIDs(parentEntries) {
			t.Fatalf("child path = %s, want the parent up to the fork %s", storetest.EntryIDs(path.Entries), storetest.EntryIDs(parentEntries))
		}

		restarted := reopen(t).(promotionSessionStore)
		again := promoteAtDeadline(t, restarted, run)
		if again == nil || !again.Replayed || again.WorkRun.SessionID != childID {
			t.Fatalf("replay: %#v", again)
		}
		if got := promotionSessionEntries(t, restarted, run.Scope, chat.ID); len(got) != 3 {
			t.Fatalf("replay appended to the chat session: %d entries", len(got))
		}
	})
}

func TestLoopAppendAfterPromotionGetsSessionMoved(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		store, run, _, _, chat := promotionSessionFixture(t, base, 1)
		if promoteAtDeadline(t, store, run) == nil {
			t.Fatal("Run was not promoted")
		}
		entry := storetest.NoteSessionEntry(models.SessionEntryID(chat.ID, "after-promotion"), models.NoteHost)
		lease := promotionLease()
		_, err := store.AppendAgentSessionEntries(t.Context(), models.AppendAgentSessionEntriesRequest{Scope: run.Scope, SessionID: chat.ID, RunID: run.ID,
			Lease: &lease, Entries: []*models.SessionEntry{entry}, Now: time.Now()})
		if !errors.Is(err, models.ErrSessionMoved) {
			t.Fatalf("append to the chat session after promotion = %v, want ErrSessionMoved", err)
		}
		reloaded, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		retry := storetest.NoteSessionEntry(models.SessionEntryID(reloaded.SessionID, "after-promotion"), models.NoteHost)
		appended, err := store.AppendAgentSessionEntries(t.Context(), models.AppendAgentSessionEntriesRequest{Scope: run.Scope, SessionID: reloaded.SessionID, RunID: run.ID,
			Lease: &lease, Entries: []*models.SessionEntry{retry}, Now: time.Now()})
		if err != nil {
			t.Fatalf("retry into the child session: %v", err)
		}
		if appended.Session.ID != models.RunAgentSessionID(run.Scope, run.ID) || len(appended.Entries) != 1 || appended.Entries[0].Sequence != 1 {
			t.Fatalf("retry landed elsewhere: %#v", appended)
		}
		path, err := store.LoadAgentSessionPath(t.Context(), run.Scope, reloaded.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(path.Entries) != 2 || path.Entries[1].ID != retry.ID {
			t.Fatalf("child path = %s", storetest.EntryIDs(path.Entries))
		}
		for _, e := range promotionSessionEntries(t, store, run.Scope, chat.ID) {
			if e.ID == entry.ID {
				t.Fatal("fenced append reached the chat session")
			}
		}
	})
}

func TestPromotionSkippedWhenLatestStepIsFinal(t *testing.T) {
	step := func(run *models.AgentRun, sequence int64, status models.AgentTurnStatus, final bool) *models.AgentTurn {
		now := time.Now().UTC()
		turn := &models.AgentTurn{ID: run.ID + "-step-" + string(rune('0'+sequence)), Scope: run.Scope, RunID: run.ID, Sequence: sequence,
			Status: status, Final: final, Revision: 1, CreatedAt: run.CreatedAt, UpdatedAt: now, StartedAt: run.CreatedAt}
		if status == models.AgentTurnStatusCompleted {
			turn.CompletedAt = &now
		}
		return turn
	}
	t.Run("final", func(t *testing.T) {
		forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
			store, run, conversation, _, chat := promotionSessionFixture(t, base, 1)
			for _, turn := range []*models.AgentTurn{step(run, 1, models.AgentTurnStatusCompleted, false), step(run, 2, models.AgentTurnStatusRunning, true)} {
				if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
			}
			if result := promoteAtDeadline(t, store, run); result != nil {
				t.Fatalf("Run posting its final answer was promoted: %#v", result)
			}
			current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
			if err != nil || current.Revision != run.Revision || current.SessionID != run.SessionID {
				t.Fatalf("skipped promotion changed the Run: %#v %v", current, err)
			}
			if _, err := store.GetAgentSession(t.Context(), run.Scope, models.RunAgentSessionID(run.Scope, run.ID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
				t.Fatalf("skipped promotion forked a session: %v", err)
			}
			if got := promotionSessionEntries(t, store, run.Scope, chat.ID); len(got) != 1 {
				t.Fatalf("skipped promotion wrote a note: %d entries", len(got))
			}
			messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: run.Scope, ConversationID: conversation.ID, Limit: 100})
			if err != nil || len(messages) != 1 {
				t.Fatalf("skipped promotion posted an acknowledgment: %d %v", len(messages), err)
			}
		})
	})
	t.Run("steering started a new step", func(t *testing.T) {
		forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
			store, run, _, _, _ := promotionSessionFixture(t, base, 1)
			for _, turn := range []*models.AgentTurn{step(run, 1, models.AgentTurnStatusCompleted, true), step(run, 2, models.AgentTurnStatusRunning, false)} {
				if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
			}
			if result := promoteAtDeadline(t, store, run); result == nil || result.WorkRun.SessionID != models.RunAgentSessionID(run.Scope, run.ID) {
				t.Fatalf("Run with a newer non-final step was not promoted: %#v", result)
			}
		})
	})
}

func TestPromotionBeforeFirstStepCreatesSeededChild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries int
	}{{"no chat row", -1}, {"empty chat row", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
				store, run, _, _, chat := promotionSessionFixture(t, base, tc.entries)
				accepted := promoteAtDeadline(t, store, run)
				if accepted == nil {
					t.Fatal("Run was not promoted")
				}
				childID := models.RunAgentSessionID(run.Scope, run.ID)
				child, err := store.GetAgentSession(t.Context(), run.Scope, childID)
				if err != nil {
					t.Fatal(err)
				}
				wantParent := ""
				if chat != nil {
					wantParent = chat.ID
				}
				if accepted.WorkRun.SessionID != childID || child.ParentSessionID != wantParent || child.ForkSequence != 0 || child.ForkEntryID != "" {
					t.Fatalf("seeded child: %#v", child)
				}
				entries := promotionSessionEntries(t, store, run.Scope, childID)
				if len(entries) != 1 || entries[0].User == nil || entries[0].User.Origin != models.UserEntryGoal || entries[0].User.GoalKind != "task" ||
					entries[0].SourceKey != models.GoalSourceKey(run.ID) {
					t.Fatalf("child seed: %#v", entries)
				}
				if chat == nil {
					if _, err := store.GetAgentSession(t.Context(), run.Scope, run.SessionID); !errors.Is(err, models.ErrAgentSessionNotFound) {
						t.Fatalf("promotion created the chat session: %v", err)
					}
					return
				}
				chatEntries := promotionSessionEntries(t, store, run.Scope, chat.ID)
				if len(chatEntries) != 1 || chatEntries[0].Note == nil || chatEntries[0].Note.Kind != models.NoteTaskPromoted {
					t.Fatalf("chat session note: %#v", chatEntries)
				}
			})
		})
	}
}

func TestPromotionWithoutSessionDoesNotFork(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, base conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		store := base.(promotionSessionStore)
		run, _, _ := conversationContinuationFixture(t, store, models.AgentRunStatusRunning, false)
		accepted := promoteAtDeadline(t, store, run)
		if accepted == nil || accepted.WorkRun.SessionID != "" {
			t.Fatalf("routing-style Run promotion: %#v", accepted)
		}
		if _, err := store.GetAgentSession(t.Context(), run.Scope, models.RunAgentSessionID(run.Scope, run.ID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
			t.Fatalf("promotion without a session forked one: %v", err)
		}
	})
}
