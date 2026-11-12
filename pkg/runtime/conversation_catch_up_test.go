package runtime

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Catch-up (run-loop section 05 §4, CONV-03), on the SQLite store
// (Postgres with OPENSEAL_TEST_POSTGRES_DSN).

type catchUpTestStore interface {
	conversationThreadStartTestStore
	AgentSessionStore
}

type catchUpFixture struct {
	t            *testing.T
	store        catchUpTestStore
	service      *ConversationService
	conversation *Conversation
	agentID      string
	now          time.Time
	catchUp      *conversationCatchUp
}

func forCatchUpStores(t *testing.T, owner ObjectiveOwner, test func(*testing.T, *catchUpFixture)) {
	t.Helper()
	forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		sessions, ok := store.(catchUpTestStore)
		if !ok {
			t.Fatalf("%T is not an agent session store", store)
		}
		service := NewConversationService(store)
		seedTestSpace(t, store.(SpaceStore), Scope{Kind: "tenant", ID: "catch-up"})
		conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
			Scope: Scope{Kind: "tenant", ID: "catch-up"}, Owner: owner, Title: "Catch-up", IdempotencyKey: "catch-up",
		})
		if err != nil {
			t.Fatal(err)
		}
		test(t, &catchUpFixture{
			t: t, store: sessions, service: service, conversation: conversation, agentID: "agent",
			now:     time.Now().UTC().Truncate(time.Millisecond),
			catchUp: &conversationCatchUp{conversations: service, runs: store},
		})
	})
}

func (f *catchUpFixture) post(req PostChannelMessageRequest) *ChannelMessage {
	f.t.Helper()
	return conversationThreadStartPost(f.t, f.service, f.conversation, req)
}

func (f *catchUpFixture) say(key, content string) *ChannelMessage {
	f.t.Helper()
	return f.post(PostChannelMessageRequest{IdempotencyKey: key, Content: content, Intent: MessageIntentUpdate})
}

func (f *catchUpFixture) answer(key, content string, sender ConversationParticipant, runID string) *ChannelMessage {
	f.t.Helper()
	return f.post(PostChannelMessageRequest{
		IdempotencyKey: key, Content: content, Intent: MessageIntentUpdate, Sender: sender,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: runID}},
	})
}

func (f *catchUpFixture) agent() ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantAgent, ID: f.agentID}
}

func (f *catchUpFixture) viewer() ConversationViewer {
	return ConversationViewer{Participant: f.agent()}
}

func (f *catchUpFixture) laneSessionID(threadRoot string) string {
	return conversationSessionID(f.conversation.Scope, ConversationRunLane(f.conversation.ID, threadRoot, f.agentID))
}

// run stores a Run bound to sessionID and, when claim is set, claims it under
// worker w1. Runs are claimed as they are created, so the claim takes this one.
func (f *catchUpFixture) run(sessionID string, claim bool) *AgentRun {
	f.t.Helper()
	portfolio := NewPortfolioService(f.store, models.WithClock(func() time.Time { return f.now }))
	run, err := portfolio.CreateAgentRun(f.t.Context(), CreateAgentRunRequest{
		Scope: f.conversation.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: f.agentID}, AssignedAgentID: f.agentID,
		Goal: "answer the chat", Source: RunSourceObjective, SessionID: sessionID,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	if !claim {
		return run
	}
	claimed, err := f.store.ClaimNextAgentRun(f.t.Context(), AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: "w1", Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, run.ID)
	}
	return claimed
}

// open claims a lane Run and opens (creates) its session.
func (f *catchUpFixture) open(threadRoot string) (*AgentRun, *AgentSession) {
	f.t.Helper()
	run := f.run(f.laneSessionID(threadRoot), true)
	opened, err := f.store.OpenConversationAgentSession(f.t.Context(), OpenConversationAgentSessionRequest{
		Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ThreadRootID: threadRoot, AgentID: f.agentID,
		RunID: run.ID, Lease: AgentRunLeaseGuard{WorkerID: "w1", Now: f.now}, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("open session: %v", err)
	}
	return run, opened.Session
}

func (f *catchUpFixture) request(run *AgentRun, session *AgentSession, through int64) catchUpRequest {
	return catchUpRequest{Run: run, Session: session, Conversation: f.conversation, Viewer: f.viewer(), Through: through, Step: 1}
}

func (f *catchUpFixture) collect(req catchUpRequest) *catchUpResult {
	f.t.Helper()
	result, err := f.catchUp.collect(f.t.Context(), req)
	if err != nil {
		f.t.Fatalf("collect: %v", err)
	}
	return result
}

// commit appends result as the step context append and returns the session.
func (f *catchUpFixture) commit(run *AgentRun, session *AgentSession, result *catchUpResult) *AgentSession {
	f.t.Helper()
	part := result.stepContextAppend(session)
	if part == nil {
		return session
	}
	appended, err := f.store.AppendAgentSessionEntries(f.t.Context(), AppendAgentSessionEntriesRequest{
		Scope: session.Scope, SessionID: session.ID, RunID: run.ID, Lease: &AgentRunLeaseGuard{WorkerID: "w1", Now: f.now},
		Entries: part.Entries, ExpectedChannelWatermark: part.ExpectedChannelWatermark, ChannelWatermark: part.ChannelWatermark, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append catch-up: %v", err)
	}
	return appended.Session
}

func (f *catchUpFixture) sessionEntries(sessionID string) []*SessionEntry {
	f.t.Helper()
	entries, err := f.store.ListAgentSessionEntries(f.t.Context(), SessionEntryFilter{Scope: f.conversation.Scope, SessionID: sessionID})
	if err != nil {
		f.t.Fatal(err)
	}
	return entries
}

func catchUpMessageIDs(entries []*SessionEntry) []string {
	ids := []string{}
	for _, entry := range entries {
		if entry.Type == SessionEntryUser && entry.User != nil {
			ids = append(ids, entry.User.MessageID)
		}
	}
	return ids
}

func catchUpEntryText(entry *SessionEntry) string {
	if entry == nil || entry.User == nil || len(entry.User.Content) == 0 {
		return ""
	}
	return entry.User.Content[0].Text
}

func TestCatchUpAppendsMessagesNewerThanWatermark(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		first := f.say("m1", "first")
		second := f.say("m2", "second")
		run, session := f.open("")
		session.ChannelWatermark = 0
		result := f.collect(f.request(run, session, second.Sequence))
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{first.ID, second.ID}) || result.Watermark != second.Sequence || result.HistoryOmitted != nil {
			t.Fatalf("first catch-up = %v watermark %d note %v", got, result.Watermark, result.HistoryOmitted)
		}
		entry := result.Entries[0]
		if entry.SourceKey != ChannelMessageSourceKey(first.ID) || entry.User.Origin != UserEntryChannelMessage || entry.User.MessageSequence != first.Sequence ||
			!strings.HasPrefix(catchUpEntryText(entry), "[user (user user) · "+first.ID) || !strings.HasSuffix(catchUpEntryText(entry), "\nfirst") {
			t.Fatalf("entry = %#v text %q", entry, catchUpEntryText(entry))
		}
		session = f.commit(run, session, result)
		if session.ChannelWatermark != second.Sequence {
			t.Fatalf("watermark after append = %d, want %d", session.ChannelWatermark, second.Sequence)
		}

		// Nothing new: an empty result and no write.
		if again := f.collect(f.request(run, session, second.Sequence)); len(again.Entries) != 0 || again.Watermark != second.Sequence || again.stepContextAppend(session) != nil {
			t.Fatalf("re-collect at the watermark = %#v", again)
		}
		third := f.say("m3", "third")
		f.say("m4", "after the bound")
		next := f.collect(f.request(run, session, third.Sequence))
		if got := catchUpMessageIDs(next.Entries); !reflect.DeepEqual(got, []string{third.ID}) || next.Watermark != third.Sequence {
			t.Fatalf("second catch-up = %v watermark %d", got, next.Watermark)
		}
		session = f.commit(run, session, next)
		if got := catchUpMessageIDs(f.sessionEntries(session.ID)); !reflect.DeepEqual(got, []string{first.ID, second.ID, third.ID}) {
			t.Fatalf("session entries = %v", got)
		}
	})
}

func TestCatchUpSkipsOwnSessionAnswersAndReceipts(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		run, session := f.open("")
		question := f.say("question", "make the list")
		f.answer("own-answer", "Here is the list.", f.agent(), run.ID)
		f.answer("own-receipt", "Saved guests.csv", ConversationParticipant{Type: ConversationParticipantService, ID: "artifacts"}, run.ID)
		f.answer("own-progress", "Working on it", f.agent(), run.ID)
		// An earlier Run of the same lane session is this session's too.
		earlier := f.run(session.ID, false)
		f.answer("earlier-answer", "Earlier answer", f.agent(), earlier.ID)
		service := f.post(PostChannelMessageRequest{IdempotencyKey: "service-note", Content: "Calendar connected", Intent: MessageIntentSystem,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "connections"}})
		// A person's message is never skipped, even when it cites this Run.
		person := f.answer("person-cites-run", "about that run", ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, run.ID)
		result := f.collect(f.request(run, session, person.Sequence))
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{question.ID, service.ID, person.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		if result.Watermark != person.Sequence {
			t.Fatalf("watermark = %d, want %d", result.Watermark, person.Sequence)
		}
		if text := catchUpEntryText(result.Entries[1]); !strings.HasPrefix(text, "[connections (service connections)") {
			t.Fatalf("service message header = %q", text)
		}

		// Skipped messages at the end still move the watermark.
		last := f.answer("own-final", "Done.", f.agent(), run.ID)
		session = f.commit(run, session, result)
		tail := f.collect(f.request(run, session, last.Sequence))
		if len(tail.Entries) != 0 || tail.Watermark != last.Sequence || tail.stepContextAppend(session) == nil {
			t.Fatalf("own-only tail = %#v", tail)
		}
	})
}

func TestCatchUpKeepsTaskReportFromChildSessionAsYouElsewhere(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		run, session := f.open("")
		// The task work Run writes to its own (child) session.
		work := f.run(RunAgentSessionID(f.conversation.Scope, "task-work"), false)
		report := f.answer("task-report", "The background task finished: 12 guests confirmed.", f.agent(), work.ID)
		// A Run that no longer exists is not this session's either.
		gone := f.answer("gone-run", "From a removed run", f.agent(), "missing-run")
		result := f.collect(f.request(run, session, gone.Sequence))
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{report.ID, gone.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		text := catchUpEntryText(result.Entries[0])
		if !strings.HasPrefix(text, "[agent (you, elsewhere agent) · "+report.ID) || !strings.Contains(text, "12 guests confirmed") {
			t.Fatalf("task report text = %q", text)
		}
		if author := result.Entries[0].User.Author; author.Type != "agent" || author.ID != f.agentID {
			t.Fatalf("task report author = %#v", author)
		}
	})
}

func TestCatchUpAttributesTeammateAnswerAsUser(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, func(t *testing.T, f *catchUpFixture) {
		run, session := f.open("")
		teammateRun := f.run(conversationSessionID(f.conversation.Scope, ConversationRunLane(f.conversation.ID, "", "rio")), false)
		question := f.say("question", "who is coming?")
		answer := f.post(PostChannelMessageRequest{
			IdempotencyKey: "rio-answer", Content: "Kim and Lee.", Intent: MessageIntentUpdate, SenderDisplayName: "Rio",
			Sender:     ConversationParticipant{Type: ConversationParticipantAgent, ID: "rio"},
			References: []ConversationReference{{Kind: ConversationReferenceRun, ID: teammateRun.ID}},
		})
		result := f.collect(f.request(run, session, answer.Sequence))
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{question.ID, answer.ID}) {
			t.Fatalf("catch-up kept %v", got)
		}
		entry := result.Entries[1]
		if entry.Type != SessionEntryUser || !strings.HasPrefix(catchUpEntryText(entry), "[Rio (agent, teammate rio) · "+answer.ID) {
			t.Fatalf("teammate answer = %#v text %q", entry, catchUpEntryText(entry))
		}
	})
}

func TestSeedNewSessionWithLast30AndHistoryOmitted(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		messages := make([]*ChannelMessage, 0, 40)
		for i := 1; i <= 40; i++ {
			messages = append(messages, f.say(fmt.Sprintf("m%02d", i), fmt.Sprintf("message %d", i)))
		}
		run, session := f.open("")
		trigger := messages[len(messages)-1]
		req := f.request(run, session, trigger.Sequence)
		req.Seed = true
		result := f.collect(req)
		want := conversationThreadStartMessageIDs(messages[10:])
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed kept %v, want %v", got, want)
		}
		if !result.OlderExists || result.Omitted != 0 || result.SeedFrom != messages[10].Sequence || result.Watermark != trigger.Sequence {
			t.Fatalf("seed result = olderExists %t omitted %d seedFrom %d watermark %d", result.OlderExists, result.Omitted, result.SeedFrom, result.Watermark)
		}
		note := result.HistoryOmitted
		if note == nil || note.Type != SessionEntryNote || note.Note.Kind != NoteHistoryOmitted || !strings.Contains(note.Note.Text, "read_conversation_history") {
			t.Fatalf("history_omitted note = %#v", note)
		}
		session = f.commit(run, session, result)
		stored := f.sessionEntries(session.ID)
		if len(stored) != 31 || stored[0].Type != SessionEntryNote || stored[0].Note.Kind != NoteHistoryOmitted || session.ChannelWatermark != trigger.Sequence {
			t.Fatalf("seeded session holds %d entries (first %s), watermark %d", len(stored), stored[0].Type, session.ChannelWatermark)
		}

		// The next catch-up is not a seed: no note.
		next := f.say("m41", "message 41")
		later := f.collect(f.request(run, session, next.Sequence))
		if got := catchUpMessageIDs(later.Entries); !reflect.DeepEqual(got, []string{next.ID}) || later.HistoryOmitted != nil || later.OlderExists {
			t.Fatalf("later catch-up = %v note %v", got, later.HistoryOmitted)
		}

		// A short chat seeds whole, with no note.
		short := f.collect(catchUpRequest{Run: run, Session: &AgentSession{ID: "short", Scope: session.Scope, ConversationID: f.conversation.ID},
			Conversation: f.conversation, Viewer: f.viewer(), Through: messages[4].Sequence, Seed: true})
		if len(short.Entries) != 5 || short.OlderExists || short.HistoryOmitted != nil || short.SeedFrom != messages[0].Sequence {
			t.Fatalf("short seed = %d entries olderExists %t note %v", len(short.Entries), short.OlderExists, short.HistoryOmitted)
		}
	})
}

func TestSeedThreadSessionIncludesBackdropOnce(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		root, expected := conversationThreadBackdropFixture(t, f.service, f.conversation, false)
		run, session := f.open(root.ID)
		req := f.request(run, session, root.Sequence)
		req.Seed, req.Trigger = true, root
		seed := f.collect(req)
		if got := conversationThreadStartMessageIDs(seed.Backdrop); !reflect.DeepEqual(got, conversationThreadStartMessageIDs(expected)) {
			t.Fatalf("seed backdrop = %v, want %v", got, conversationThreadStartMessageIDs(expected))
		}
		// The thread session holds only its thread; the backdrop never becomes entries.
		if got := catchUpMessageIDs(seed.Entries); !reflect.DeepEqual(got, []string{root.ID}) {
			t.Fatalf("thread seed entries = %v", got)
		}
		for _, entry := range seed.Entries {
			for _, forbidden := range []string{"AUTHORIZED_", "PRIVATE_CONTEXT_MUST_NOT_APPEAR", "OTHER_THREAD_CONTEXT_MUST_NOT_APPEAR"} {
				if strings.Contains(catchUpEntryText(entry), forbidden) {
					t.Fatalf("thread session ingested %s: %q", forbidden, catchUpEntryText(entry))
				}
			}
		}
		session = f.commit(run, session, seed)

		f.say("later-main", "LATER_MAIN_MUST_NOT_APPEAR")
		utterance := f.post(PostChannelMessageRequest{IdempotencyKey: "thread-utterance", ReplyToMessageID: root.ID, RequiresResponse: true})
		next := f.request(run, session, utterance.Sequence)
		next.Trigger = utterance
		later := f.collect(next)
		if later.Backdrop != nil || !reflect.DeepEqual(catchUpMessageIDs(later.Entries), []string{utterance.ID}) {
			t.Fatalf("later thread catch-up = backdrop %v entries %v", conversationThreadStartMessageIDs(later.Backdrop), catchUpMessageIDs(later.Entries))
		}
		history, err := ReadConversationHistory(t.Context(), f.service, f.conversation.Scope, f.conversation.Owner, f.conversation.ID, f.viewer(), ConversationHistoryReadRequest{}, root.ID)
		if err != nil || !reflect.DeepEqual(conversationThreadStartHistoryIDs(history), []string{utterance.ID, root.ID}) {
			t.Fatalf("backdrop widened thread history: page=%#v err=%v", history, err)
		}
	})
}

func TestCatchUpCapsToOneAppendWithOmittedNote(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		run, session := f.open("")
		total := maximumCatchUpMessages + 30
		messages := make([]*ChannelMessage, 0, total)
		for i := 0; i < total; i++ {
			messages = append(messages, f.say(fmt.Sprintf("m%03d", i), fmt.Sprintf("message %d", i)))
		}
		last := messages[len(messages)-1]
		result := f.collect(f.request(run, session, last.Sequence))
		want := conversationThreadStartMessageIDs(messages[30:])
		if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, want) {
			t.Fatalf("capped catch-up kept %d entries starting %v, want the newest %d", len(got), got[:1], len(want))
		}
		if result.Omitted != 30 || result.Watermark != last.Sequence || result.HistoryOmitted == nil || result.HistoryOmitted.Note.Kind != NoteHistoryOmitted {
			t.Fatalf("capped result = omitted %d watermark %d note %v", result.Omitted, result.Watermark, result.HistoryOmitted)
		}
		// The whole catch-up, run_context note and steering included, fits one append.
		part := result.stepContextAppend(session)
		if len(part.Entries)+catchUpReservedAppendEntries-1 > MaxSessionAppendEntries {
			t.Fatalf("catch-up append has %d entries", len(part.Entries))
		}
		session = f.commit(run, session, result)
		if stored := f.sessionEntries(session.ID); len(stored) != maximumCatchUpMessages+1 || session.ChannelWatermark != last.Sequence {
			t.Fatalf("stored %d entries, watermark %d", len(stored), session.ChannelWatermark)
		}
	})
}

func TestCatchUpCapsByAppendBytes(t *testing.T) {
	store := newTestKernelStore(t)
	service := NewConversationService(store)
	conversation := conversationThreadStartConversation(t, store)
	f := &catchUpFixture{t: t, store: store, service: service, conversation: conversation, agentID: conversation.Owner.ID,
		now: time.Now().UTC().Truncate(time.Millisecond), catchUp: &conversationCatchUp{conversations: service, runs: store}}
	run, session := f.open("")
	big := strings.Repeat("x", 60<<10)
	var last *ChannelMessage
	for i := 0; i < 110; i++ {
		last = f.say(fmt.Sprintf("big-%03d", i), big)
	}
	result := f.collect(f.request(run, session, last.Sequence))
	if result.Omitted == 0 || len(result.Entries)+result.Omitted != 110 || result.HistoryOmitted == nil {
		t.Fatalf("byte cap: %d entries, %d omitted", len(result.Entries), result.Omitted)
	}
	if got := catchUpMessageIDs(result.Entries); got[len(got)-1] != last.ID {
		t.Fatalf("byte cap dropped the newest message")
	}
	f.commit(run, session, result)
}

func TestCatchUpRespectsViewerVisibility(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		run, session := f.open("")
		public := f.say("public", "for everyone")
		hidden := f.post(PostChannelMessageRequest{
			IdempotencyKey: "hidden", Content: "HIDDEN_MUST_NOT_APPEAR", Intent: MessageIntentUpdate,
			Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private-user"}}},
		})
		quote := f.post(PostChannelMessageRequest{IdempotencyKey: "quotes-hidden", Content: "see above", Intent: MessageIntentUpdate, QuotedMessageID: hidden.ID})
		for _, seed := range []bool{false, true} {
			req := f.request(run, session, quote.Sequence)
			req.Seed = seed
			result := f.collect(req)
			if got := catchUpMessageIDs(result.Entries); !reflect.DeepEqual(got, []string{public.ID, quote.ID}) {
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
