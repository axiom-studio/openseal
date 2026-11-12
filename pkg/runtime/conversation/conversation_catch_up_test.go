package conversation_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

// Catch-up (run-loop section 05 §4, CONV-03), on the SQLite store
// (Postgres with OPENSEAL_TEST_POSTGRES_DSN).

func catchUpEntryText(entry *models.SessionEntry) string {
	if entry == nil || entry.User == nil || len(entry.User.Content) == 0 {
		return ""
	}
	return entry.User.Content[0].Text
}

func TestCatchUpAppendsMessagesNewerThanWatermark(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		first := f.Say("m1", "first")
		second := f.Say("m2", "second")
		run, session := f.Open("")
		session.ChannelWatermark = 0
		result := f.Collect(f.Request(run, session, second.Sequence))
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{first.ID, second.ID}) || result.Watermark != second.Sequence || result.HistoryOmitted != nil {
			t.Fatalf("first catch-up = %v watermark %d note %v", got, result.Watermark, result.HistoryOmitted)
		}
		entry := result.Entries[0]
		if entry.SourceKey != models.ChannelMessageSourceKey(first.ID) || entry.User.Origin != models.UserEntryChannelMessage || entry.User.MessageSequence != first.Sequence ||
			!strings.HasPrefix(catchUpEntryText(entry), "[user (user user) · "+first.ID) || !strings.HasSuffix(catchUpEntryText(entry), "\nfirst") {
			t.Fatalf("entry = %#v text %q", entry, catchUpEntryText(entry))
		}
		session = f.Commit(run, session, result)
		if session.ChannelWatermark != second.Sequence {
			t.Fatalf("watermark after append = %d, want %d", session.ChannelWatermark, second.Sequence)
		}

		// Nothing new: an empty result and no write.
		if again := f.Collect(f.Request(run, session, second.Sequence)); len(again.Entries) != 0 || again.Watermark != second.Sequence || again.StepContextAppend(session) != nil {
			t.Fatalf("re-collect at the watermark = %#v", again)
		}
		third := f.Say("m3", "third")
		f.Say("m4", "after the bound")
		next := f.Collect(f.Request(run, session, third.Sequence))
		if got := conversationtest.CatchUpMessageIDs(next.Entries); !reflect.DeepEqual(got, []string{third.ID}) || next.Watermark != third.Sequence {
			t.Fatalf("second catch-up = %v watermark %d", got, next.Watermark)
		}
		session = f.Commit(run, session, next)
		if got := conversationtest.CatchUpMessageIDs(f.SessionEntries(session.ID)); !reflect.DeepEqual(got, []string{first.ID, second.ID, third.ID}) {
			t.Fatalf("session entries = %v", got)
		}
	})
}

func TestCatchUpSkipsOwnSessionAnswersAndReceipts(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		run, session := f.Open("")
		question := f.Say("question", "make the list")
		f.Answer("own-answer", "Here is the list.", f.Agent(), run.ID)
		f.Answer("own-receipt", "Saved guests.csv", models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "artifacts"}, run.ID)
		f.Answer("own-progress", "Working on it", f.Agent(), run.ID)
		// An earlier Run of the same lane session is this session's too.
		earlier := f.Run(session.ID, false)
		f.Answer("earlier-answer", "Earlier answer", f.Agent(), earlier.ID)
		service := f.Post(runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "service-note", Content: "Calendar connected", Intent: models.MessageIntentSystem,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "connections"}})
		// A person's message is never skipped, even when it cites this Run.
		person := f.Answer("person-cites-run", "about that run", models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}, run.ID)
		result := f.Collect(f.Request(run, session, person.Sequence))
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{question.ID, service.ID, person.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		if result.Watermark != person.Sequence {
			t.Fatalf("watermark = %d, want %d", result.Watermark, person.Sequence)
		}
		if text := catchUpEntryText(result.Entries[1]); !strings.HasPrefix(text, "[connections (service connections)") {
			t.Fatalf("service message header = %q", text)
		}

		// Skipped messages at the end still move the watermark.
		last := f.Answer("own-final", "Done.", f.Agent(), run.ID)
		session = f.Commit(run, session, result)
		tail := f.Collect(f.Request(run, session, last.Sequence))
		if len(tail.Entries) != 0 || tail.Watermark != last.Sequence || tail.StepContextAppend(session) == nil {
			t.Fatalf("own-only tail = %#v", tail)
		}
	})
}

func TestCatchUpKeepsTaskReportFromChildSessionAsYouElsewhere(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		run, session := f.Open("")
		// The task work Run writes to its own (child) session.
		work := f.Run(models.RunAgentSessionID(f.Conversation.Scope, "task-work"), false)
		report := f.Answer("task-report", "The background task finished: 12 guests confirmed.", f.Agent(), work.ID)
		// A Run that no longer exists is not this session's either.
		gone := f.Answer("gone-run", "From a removed run", f.Agent(), "missing-run")
		result := f.Collect(f.Request(run, session, gone.Sequence))
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{report.ID, gone.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		text := catchUpEntryText(result.Entries[0])
		if !strings.HasPrefix(text, "[agent (you, elsewhere agent) · "+report.ID) || !strings.Contains(text, "12 guests confirmed") {
			t.Fatalf("task report text = %q", text)
		}
		if author := result.Entries[0].User.Author; author.Type != "agent" || author.ID != f.AgentID {
			t.Fatalf("task report author = %#v", author)
		}
	})
}

func TestCatchUpAttributesTeammateAnswerAsUser(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		run, session := f.Open("")
		teammateRun := f.Run(models.ConversationSessionID(f.Conversation.Scope, models.ConversationRunLane(f.Conversation.ID, "", "rio")), false)
		question := f.Say("question", "who is coming?")
		answer := f.Post(runtimeconversation.PostChannelMessageRequest{
			IdempotencyKey: "rio-answer", Content: "Kim and Lee.", Intent: models.MessageIntentUpdate, SenderDisplayName: "Rio",
			Sender:     models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "rio"},
			References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: teammateRun.ID}},
		})
		result := f.Collect(f.Request(run, session, answer.Sequence))
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{question.ID, answer.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		entry := result.Entries[1]
		if entry.Type != models.SessionEntryUser || !strings.HasPrefix(catchUpEntryText(entry), "[Rio (agent, teammate rio) · "+answer.ID) {
			t.Fatalf("teammate answer = %#v text %q", entry, catchUpEntryText(entry))
		}
	})
}

func TestSeedNewSessionWithLast30AndHistoryOmitted(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		messages := make([]*models.ChannelMessage, 0, 40)
		for i := 1; i <= 40; i++ {
			messages = append(messages, f.Say(fmt.Sprintf("m%02d", i), fmt.Sprintf("message %d", i)))
		}
		run, session := f.Open("")
		trigger := messages[len(messages)-1]
		req := f.Request(run, session, trigger.Sequence)
		req.Seed = true
		result := f.Collect(req)
		want := conversationtest.ThreadStartMessageIDs(messages[10:])
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed kept %v, want %v", got, want)
		}
		if !result.OlderExists || result.Omitted != 0 || result.SeedFrom != messages[10].Sequence || result.Watermark != trigger.Sequence {
			t.Fatalf("seed result = olderExists %t omitted %d seedFrom %d watermark %d", result.OlderExists, result.Omitted, result.SeedFrom, result.Watermark)
		}
		note := result.HistoryOmitted
		if note == nil || note.Type != models.SessionEntryNote || note.Note.Kind != models.NoteHistoryOmitted || !strings.Contains(note.Note.Text, "read_conversation_history") {
			t.Fatalf("history_omitted note = %#v", note)
		}
		session = f.Commit(run, session, result)
		stored := f.SessionEntries(session.ID)
		if len(stored) != 31 || stored[0].Type != models.SessionEntryNote || stored[0].Note.Kind != models.NoteHistoryOmitted || session.ChannelWatermark != trigger.Sequence {
			t.Fatalf("seeded session holds %d entries (first %s), watermark %d", len(stored), stored[0].Type, session.ChannelWatermark)
		}

		// The next catch-up is not a seed: no note.
		next := f.Say("m41", "message 41")
		later := f.Collect(f.Request(run, session, next.Sequence))
		if got := conversationtest.CatchUpMessageIDs(later.Entries); !reflect.DeepEqual(got, []string{next.ID}) || later.HistoryOmitted != nil || later.OlderExists {
			t.Fatalf("later catch-up = %v note %v", got, later.HistoryOmitted)
		}

		// A short chat seeds whole, with no note.
		short := f.Collect(runtimeconversation.CatchUpRequest{Run: run, Session: &models.AgentSession{ID: "short", Scope: session.Scope, ConversationID: f.Conversation.ID},
			Conversation: f.Conversation, Viewer: f.Viewer(), Through: messages[4].Sequence, Seed: true})
		if len(short.Entries) != 5 || short.OlderExists || short.HistoryOmitted != nil || short.SeedFrom != messages[0].Sequence {
			t.Fatalf("short seed = %d entries olderExists %t note %v", len(short.Entries), short.OlderExists, short.HistoryOmitted)
		}
	})
}

func TestSeedThreadSessionIncludesBackdropOnce(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		root, expected := conversationThreadBackdropFixture(t, f.Service, f.Conversation, false)
		run, session := f.Open(root.ID)
		req := f.Request(run, session, root.Sequence)
		req.Seed, req.Trigger = true, root
		seed := f.Collect(req)
		if got := conversationtest.ThreadStartMessageIDs(seed.Backdrop); !reflect.DeepEqual(got, conversationtest.ThreadStartMessageIDs(expected)) {
			t.Fatalf("seed backdrop = %v, want %v", got, conversationtest.ThreadStartMessageIDs(expected))
		}
		// The thread session holds only its thread; the backdrop never becomes entries.
		if got := conversationtest.CatchUpMessageIDs(seed.Entries); !reflect.DeepEqual(got, []string{root.ID}) {
			t.Fatalf("thread seed entries = %v", got)
		}
		for _, entry := range seed.Entries {
			for _, forbidden := range []string{"AUTHORIZED_", "PRIVATE_CONTEXT_MUST_NOT_APPEAR", "OTHER_THREAD_CONTEXT_MUST_NOT_APPEAR"} {
				if strings.Contains(catchUpEntryText(entry), forbidden) {
					t.Fatalf("thread session ingested %s: %q", forbidden, catchUpEntryText(entry))
				}
			}
		}
		session = f.Commit(run, session, seed)

		f.Say("later-main", "LATER_MAIN_MUST_NOT_APPEAR")
		utterance := f.Post(runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "thread-utterance", ReplyToMessageID: root.ID, RequiresResponse: true})
		next := f.Request(run, session, utterance.Sequence)
		next.Trigger = utterance
		later := f.Collect(next)
		if later.Backdrop != nil || !reflect.DeepEqual(conversationtest.CatchUpMessageIDs(later.Entries), []string{utterance.ID}) {
			t.Fatalf("later thread catch-up = backdrop %v entries %v", conversationtest.ThreadStartMessageIDs(later.Backdrop), conversationtest.CatchUpMessageIDs(later.Entries))
		}
		history, err := runtimeconversation.ReadConversationHistory(t.Context(), f.Service, f.Conversation.Scope, f.Conversation.Owner, f.Conversation.ID, f.Viewer(), runtimeconversation.HistoryReadRequest{}, root.ID)
		if err != nil || !reflect.DeepEqual(conversationtest.ThreadStartHistoryIDs(history), []string{utterance.ID, root.ID}) {
			t.Fatalf("backdrop widened thread history: page=%#v err=%v", history, err)
		}
	})
}

func TestCatchUpCapsToOneAppendWithOmittedNote(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		run, session := f.Open("")
		total := runtimeconversation.MaximumCatchUpMessages + 30
		messages := make([]*models.ChannelMessage, 0, total)
		for i := 0; i < total; i++ {
			messages = append(messages, f.Say(fmt.Sprintf("m%03d", i), fmt.Sprintf("message %d", i)))
		}
		last := messages[len(messages)-1]
		result := f.Collect(f.Request(run, session, last.Sequence))
		want := conversationtest.ThreadStartMessageIDs(messages[30:])
		if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, want) {
			t.Fatalf("capped catch-up kept %d entries starting %v, want the newest %d", len(got), got[:1], len(want))
		}
		if result.Omitted != 30 || result.Watermark != last.Sequence || result.HistoryOmitted == nil || result.HistoryOmitted.Note.Kind != models.NoteHistoryOmitted {
			t.Fatalf("capped result = omitted %d watermark %d note %v", result.Omitted, result.Watermark, result.HistoryOmitted)
		}
		// The whole catch-up, run_context note and steering included, fits one append.
		part := result.StepContextAppend(session)
		if len(part.Entries)+runtimeconversation.CatchUpReservedAppendEntries-1 > models.MaxSessionAppendEntries {
			t.Fatalf("catch-up append has %d entries", len(part.Entries))
		}
		session = f.Commit(run, session, result)
		if stored := f.SessionEntries(session.ID); len(stored) != runtimeconversation.MaximumCatchUpMessages+1 || session.ChannelWatermark != last.Sequence {
			t.Fatalf("stored %d entries, watermark %d", len(stored), session.ChannelWatermark)
		}
	})
}

func TestCatchUpCapsByAppendBytes(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := runtimeconversation.NewService(store)
	conversation := conversationtest.ThreadStartConversation(t, store)
	f := &conversationtest.CatchUpFixture{T: t, Store: store, Service: service, Conversation: conversation, AgentID: conversation.Owner.ID,
		Now: time.Now().UTC().Truncate(time.Millisecond), CatchUp: runtimeconversation.NewCatchUp(service, store, nil)}
	run, session := f.Open("")
	big := strings.Repeat("x", 60<<10)
	var last *models.ChannelMessage
	for i := 0; i < 110; i++ {
		last = f.Say(fmt.Sprintf("big-%03d", i), big)
	}
	result := f.Collect(f.Request(run, session, last.Sequence))
	if result.Omitted == 0 || len(result.Entries)+result.Omitted != 110 || result.HistoryOmitted == nil {
		t.Fatalf("byte cap: %d entries, %d omitted", len(result.Entries), result.Omitted)
	}
	if got := conversationtest.CatchUpMessageIDs(result.Entries); got[len(got)-1] != last.ID {
		t.Fatalf("byte cap dropped the newest message")
	}
	f.Commit(run, session, result)
}

func TestCatchUpRespectsViewerVisibility(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		run, session := f.Open("")
		public := f.Say("public", "for everyone")
		hidden := f.Post(runtimeconversation.PostChannelMessageRequest{
			IdempotencyKey: "hidden", Content: "HIDDEN_MUST_NOT_APPEAR", Intent: models.MessageIntentUpdate,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "private-user"}}},
		})
		quote := f.Post(runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "quotes-hidden", Content: "see above", Intent: models.MessageIntentUpdate, QuotedMessageID: hidden.ID})
		for _, seed := range []bool{false, true} {
			req := f.Request(run, session, quote.Sequence)
			req.Seed = seed
			result := f.Collect(req)
			if got := conversationtest.CatchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{public.ID, quote.ID}) {
				t.Fatalf("seed=%t catch-up kept %v", seed, got)
			}
			for _, entry := range result.Entries {
				if strings.Contains(catchUpEntryText(entry), "HIDDEN_MUST_NOT_APPEAR") {
					t.Fatalf("seed=%t hidden message reached the session: %q", seed, catchUpEntryText(entry))
				}
			}
			if result.Watermark != quote.Sequence {
				t.Fatalf("watermark = %d, want %d", result.Watermark, quote.Sequence)
			}
		}
	})
}
