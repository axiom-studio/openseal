package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The run_context note (run-loop section 05 §5, CONV-04), on the memory and
// SQLite stores through the catch-up fixture.

func (f *catchUpFixture) noteRunner() *ConversationRunTurnRunner {
	return &ConversationRunTurnRunner{conversations: f.service, portfolio: f.store, runbooks: f.store}
}

func (f *catchUpFixture) noteInput(run *AgentRun, session *AgentSession, trigger *ChannelMessage) conversationContextInput {
	return conversationContextInput{
		Run: run, Session: session, Sessions: f.store, Conversation: f.conversation, Trigger: trigger,
		Viewer: f.viewer(), Step: 1,
	}
}

// note builds the run_context note of in and appends it to the session.
func (f *catchUpFixture) note(runner *ConversationRunTurnRunner, in conversationContextInput) *SessionEntry {
	f.t.Helper()
	note, err := runner.conversationRunContextNote(f.t.Context(), in)
	if err != nil {
		f.t.Fatalf("run context: %v", err)
	}
	if note == nil {
		return nil
	}
	if _, err := f.store.AppendAgentSessionEntries(f.t.Context(), AppendAgentSessionEntriesRequest{
		Scope: in.Session.Scope, SessionID: in.Session.ID, RunID: in.Run.ID, Lease: &AgentRunLeaseGuard{WorkerID: "w1", Now: f.now},
		Entries: []*SessionEntry{note}, Now: f.now,
	}); err != nil {
		f.t.Fatalf("append run context: %v", err)
	}
	return note
}

// nextRun claims another Run of the lane session.
func (f *catchUpFixture) nextRun(session *AgentSession) *AgentRun {
	f.t.Helper()
	return f.run(session.ID, true)
}

func runContextSectionNames(t *testing.T, note *SessionEntry) map[string]bool {
	t.Helper()
	if note == nil {
		return map[string]bool{}
	}
	var data runContextNoteData
	if err := json.Unmarshal(note.Note.Data, &data); err != nil {
		t.Fatalf("note data: %v", err)
	}
	names := make(map[string]bool, len(data.Sections))
	for name := range data.Sections {
		names[name] = true
	}
	return names
}

func TestRunContextNoteAlwaysHasRespondTo(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		runner := f.noteRunner()
		trigger := f.post(PostChannelMessageRequest{IdempotencyKey: "q1", Content: "What is on today?", SenderDisplayName: "Kev"})
		run, session := f.open("")
		first := f.note(runner, f.noteInput(run, session, trigger))
		if first == nil || first.Note.Kind != NoteRunContext || first.SourceKey != RunContextSourceKey(run.ID) ||
			first.ID != SessionEntryID(session.ID, RunContextSourceKey(run.ID)) || first.Step != 1 {
			t.Fatalf("first note = %#v", first)
		}
		want := "<run_context>\nReply to message " + trigger.ID + " from Kev (user user)."
		if !strings.HasPrefix(first.Note.Text, want) || !strings.HasSuffix(first.Note.Text, "\n</run_context>") {
			t.Fatalf("note text = %q", first.Note.Text)
		}
		if names := runContextSectionNames(t, first); !names[contextSectionRespondTo] || !names[contextSectionChannel] || !names[contextSectionActiveRuns] {
			t.Fatalf("first sections = %v", names)
		}
		// The next Run of the lane sees nothing new but still gets respond_to.
		second := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		names := runContextSectionNames(t, second)
		if second == nil || len(names) != 1 || !names[contextSectionRespondTo] {
			t.Fatalf("second note sections = %v (%v)", names, second)
		}
	})
}

func TestRunContextSectionsEmittedOnlyWhenChanged(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		runner := f.noteRunner()
		style := "watercolor"
		runner.config.ImageStyle = func(context.Context, *AgentRun) string { return style }
		trigger := f.say("q1", "Draw me a cat")
		run, session := f.open("")
		first := f.note(runner, f.noteInput(run, session, trigger))
		if names := runContextSectionNames(t, first); !names[contextSectionImageStyle] || !names[contextSectionChannel] {
			t.Fatalf("first sections = %v", names)
		}
		if !strings.Contains(first.Note.Text, "Preferred picture style: watercolor.") || !strings.Contains(first.Note.Text, `Channel "Catch-up" (`+f.conversation.ID+`).`) {
			t.Fatalf("first text = %q", first.Note.Text)
		}

		unchanged := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if names := runContextSectionNames(t, unchanged); names[contextSectionImageStyle] || names[contextSectionChannel] || names[contextSectionActiveRuns] {
			t.Fatalf("unchanged sections re-sent: %v", names)
		}

		style = "ink"
		changed := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if names := runContextSectionNames(t, changed); !names[contextSectionImageStyle] || names[contextSectionChannel] {
			t.Fatalf("changed sections = %v", names)
		}
		if !strings.Contains(changed.Note.Text, "Preferred picture style: ink.") {
			t.Fatalf("changed text = %q", changed.Note.Text)
		}

		// A section that became empty is sent once with its none text.
		style = ""
		cleared := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if !strings.Contains(cleared.Note.Text, "Preferred picture style: none.") {
			t.Fatalf("cleared text = %q", cleared.Note.Text)
		}
		again := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if names := runContextSectionNames(t, again); names[contextSectionImageStyle] {
			t.Fatalf("none text re-sent: %q", again.Note.Text)
		}
	})
}

func TestRunContextReEmittedAfterCompaction(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		runner := f.noteRunner()
		trigger := f.say("q1", "Hello")
		run, session := f.open("")
		session = f.commit(run, session, f.collect(f.request(run, session, trigger.Sequence)))
		first := f.note(runner, f.noteInput(run, session, trigger))

		next := f.nextRun(session)
		if _, err := f.store.AppendAgentSessionEntries(t.Context(), AppendAgentSessionEntriesRequest{
			Scope: session.Scope, SessionID: session.ID, RunID: next.ID, Lease: &AgentRunLeaseGuard{WorkerID: "w1", Now: f.now},
			Entries: []*SessionEntry{{ID: "compaction-1", Type: SessionEntryCompaction, Compaction: &CompactionEntry{
				Summary: "They said hello.", FirstKeptEntryID: first.ID, Trigger: CompactionTriggerThreshold, Provider: "openai", Model: "gpt-x",
			}}},
			Now: f.now,
		}); err != nil {
			t.Fatalf("append compaction: %v", err)
		}
		after := f.note(runner, f.noteInput(next, session, trigger))
		if names := runContextSectionNames(t, after); !names[contextSectionChannel] || !names[contextSectionActiveRuns] {
			t.Fatalf("sections after compaction = %v", names)
		}
	})
}

func TestActiveRunsBecomesNoneWhenWorkEnds(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		ctx := t.Context()
		runner := f.noteRunner()
		trigger := f.say("q1", "Run the report")
		portfolio := NewPortfolioService(f.store)
		root, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: f.conversation.Scope, Owner: f.conversation.Owner, AssignedAgentID: f.agentID, Kind: RunKindConversation,
			Goal: "chat", Source: RunSourceChat,
			Context: map[string]interface{}{conversationRunContextConversationID: f.conversation.ID, conversationRunContextTriggerID: trigger.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		child, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: f.conversation.Scope, Owner: f.conversation.Owner, AssignedAgentID: f.agentID, ParentRunID: root.ID,
			Goal: "Build the weekly report", Source: RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, session := f.open("")
		first := f.note(runner, f.noteInput(run, session, trigger))
		if !strings.Contains(first.Note.Text, "Active work:\n- run "+child.ID+" (") || !strings.Contains(first.Note.Text, `"Build the weekly report"`) {
			t.Fatalf("active work = %q", first.Note.Text)
		}
		current, err := f.store.GetAgentRun(ctx, child.Scope, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := NewRunActivityService(f.store, f.store).TransitionRun(ctx, child.Scope, child.ID, RunTransitionRequest{
			ExpectedRevision: current.Revision, Status: AgentRunStatusCanceled, Summary: "Canceled", EventType: "run.canceled",
		}); err != nil {
			t.Fatal(err)
		}
		ended := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if names := runContextSectionNames(t, ended); !names[contextSectionActiveRuns] || !strings.Contains(ended.Note.Text, "Active work: none.") {
			t.Fatalf("after the work ended = %q", ended.Note.Text)
		}
	})
}

func TestRunContextBoundedTo32KiB(t *testing.T) {
	session := &AgentSession{ID: "session", Scope: Scope{Kind: "tenant", ID: "bound"}}
	var active, objectives []string
	for index := 0; index < 150; index++ {
		active = append(active, fmt.Sprintf("- run run-%03d (running, chat) %q", index, strings.Repeat("a", 150)))
	}
	for index := 0; index < 50; index++ {
		objectives = append(objectives, fmt.Sprintf("- objective obj-%03d \"Goal\" (active, priority 1, revision 1): %s", index, strings.Repeat("o", 150)))
	}
	sections := []conversationContextSection{
		{Name: contextSectionRespondTo, Always: true, Text: "Reply to message m1 from Kev (user kev)."},
		{Name: contextSectionObjectives, Text: "Objectives:\n" + strings.Join(objectives, "\n")},
		{Name: contextSectionActiveRuns, Text: "Active work:\n" + strings.Join(active, "\n")},
		{Name: contextSectionPlaces, Text: "Places already found in this chat (reuse their ids):\n- Home (place p1, 1,2)"},
	}
	note := conversationContextNote(session, "run-1", sections, nil)
	text := note.Note.Text
	if len(text) > maximumRunContextBytes {
		t.Fatalf("note is %d bytes", len(text))
	}
	if !strings.Contains(text, "Reply to message m1 from Kev (user kev).") || !strings.Contains(text, "- Home (place p1, 1,2)") {
		t.Fatalf("small sections lost: %q", text[:200])
	}
	if !strings.Contains(text, "Active work:\n- run run-000") || !strings.Contains(text, " more)") || note.Note.validate() != nil {
		t.Fatalf("active work was not truncated with a count")
	}
	// Active work is cut first: the objectives that fit stay whole.
	if got := strings.Count(text, "- objective "); got != 50 || strings.Count(text, "- run run-") >= 150 {
		t.Fatalf("objectives %d, active %d", got, strings.Count(text, "- run run-"))
	}
	// The digest is of the whole section, so an unchanged truncated section is not sent again.
	again := conversationContextNote(session, "run-2", sections, latestContextDigests([]*SessionEntry{note}))
	if names := runContextSectionNames(t, again); len(names) != 1 || !names[contextSectionRespondTo] {
		t.Fatalf("re-sent sections = %v", names)
	}
	// Past what active work can give, objectives are cut too.
	for index := 50; index < 300; index++ {
		objectives = append(objectives, fmt.Sprintf("- objective obj-%03d \"Goal\" (active, priority 1, revision 1): %s", index, strings.Repeat("o", 150)))
	}
	sections[1].Text = "Objectives:\n" + strings.Join(objectives, "\n")
	large := conversationContextNote(session, "run-1", sections, nil).Note.Text
	if len(large) > maximumRunContextBytes || !strings.Contains(large, "Active work:\n… (150 more)") || !strings.Contains(large, "- objective obj-000") ||
		strings.Count(large, "- objective ") >= 300 {
		t.Fatalf("large note is %d bytes, %d objectives", len(large), strings.Count(large, "- objective "))
	}
}

func TestTeamRosterSectionAndRoutingReason(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch"}, func(t *testing.T, f *catchUpFixture) {
		participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return []ConversationParticipantBinding{{Participant: f.agent()}, {Participant: teamRoutingAgent("maya")}}, nil
		})
		router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
			return ConversationRouteDecision{AgentID: f.agentID}, nil
		})
		coordinator, err := NewConversationCoordinator(f.service, participants, router, DefaultConversationCoordinatorConfig())
		if err != nil {
			t.Fatal(err)
		}
		runner := f.noteRunner()
		runner.coordinator = coordinator
		trigger := f.say("q1", "Who writes the launch post?")
		run, session := f.open("")
		member := cloneAgentRun(run)
		member.Kind, member.Owner = RunKindConversation, f.conversation.Owner
		member.Context = map[string]interface{}{routingReasonContextKey: "owns the launch copy"}
		note := f.note(runner, f.noteInput(member, session, trigger))
		for _, want := range []string{
			"You were picked to answer because: owns the launch copy.",
			"Team roster:\n- agent (agent), you\n- maya (maya)",
		} {
			if !strings.Contains(note.Note.Text, want) {
				t.Fatalf("note lacks %q: %q", want, note.Note.Text)
			}
		}
		if names := runContextSectionNames(t, note); !names[contextSectionTeam] {
			t.Fatalf("sections = %v", names)
		}
		// A 1:1 Agent Run has no Team section.
		solo := f.note(runner, f.noteInput(f.nextRun(session), session, trigger))
		if strings.Contains(solo.Note.Text, "Team roster:\n") || strings.Contains(solo.Note.Text, "picked to answer") {
			t.Fatalf("non-member note = %q", solo.Note.Text)
		}
	})
}

func TestVoiceCallGreetingInRespondTo(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		runner := f.noteRunner()
		spoken := f.say("q1", "What's the weather?")
		spoken.ResponseMode = "spoken"
		run, session := f.open("")
		text := f.note(runner, f.noteInput(run, session, spoken)).Note.Text
		if !strings.Contains(text, "Spoken reply: answer in words to be spoken.") || strings.Contains(text, "Voice call just started.") {
			t.Fatalf("spoken respond_to = %q", text)
		}

		call := &ChannelMessage{
			ID: "call-start", Scope: f.conversation.Scope, ConversationID: f.conversation.ID, Sequence: spoken.Sequence + 1,
			Sender:    ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID},
			Initiator: &ConversationParticipant{Type: ConversationParticipantUser, ID: "user"},
			Intent:    MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true, Content: "Call started",
		}
		text = f.note(runner, f.noteInput(f.nextRun(session), session, call)).Note.Text
		want := "Reply to message call-start from " + VoiceCallCoordinatorParticipantID + " (service " + VoiceCallCoordinatorParticipantID + ").\n" +
			"Spoken reply: answer in words to be spoken.\nVoice call just started. " + voiceCallGreetingBehaviorInstruction
		if !strings.Contains(text, want) {
			t.Fatalf("voice call respond_to = %q", text)
		}
	})
}

// conversationTaskListStore returns fixed task rows.
type conversationTaskListStore struct {
	catchUpTestStore
	rows   []*ConversationTaskResult
	filter ConversationTaskFilter
}

func (s *conversationTaskListStore) ListConversationTasks(_ context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	s.filter = filter
	return s.rows, nil
}

func runContextTestTask(t *testing.T, f *catchUpFixture, trigger *ChannelMessage, key, goal string, status AgentRunStatus, created time.Time) *ConversationTaskResult {
	t.Helper()
	task := &ConversationTask{
		Mode: ConversationTaskModeIndependent, Scope: f.conversation.Scope, Owner: f.conversation.Owner, ConversationID: f.conversation.ID,
		SourceMessageID: trigger.ID, ThreadRootID: trigger.ID, SourceRunID: "source-" + key, SourceTurnID: "turn-" + key, SourceTurnNumber: 1,
		AuthenticatedActor: trigger.Sender, TargetAgentID: f.agentID, WorkRunID: "work-" + key, TaskKey: key, Goal: goal,
		Acknowledgment: "On it.", Revision: 1, CreatedAt: created,
	}
	task.ID = conversationTaskID(task.Scope, task.SourceRunID, task.TaskKey)
	digest, err := conversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	task.RequestDigest = digest
	work := &AgentRun{
		ID: task.WorkRunID, Scope: task.Scope, Owner: task.Owner, AssignedAgentID: f.agentID, Kind: RunKindAgentWork, Goal: goal,
		Source: RunSourceChat, RootRunID: task.WorkRunID, ConcurrencyKey: "task:" + task.ID, Status: status, Revision: 3,
		Context: map[string]interface{}{ConversationTaskContextKey: task.ID, conversationRunContextConversationID: task.ConversationID,
			conversationRunContextTriggerID: task.SourceMessageID},
	}
	if !ConversationTaskMatchesWorkRun(task, work) {
		t.Fatalf("fixture task does not match its work run")
	}
	return &ConversationTaskResult{Task: task, WorkRun: work}
}

func TestRunContextListsRecentConversationTasks(t *testing.T) {
	forCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *catchUpFixture) {
		trigger := f.say("q1", "How is the research going?")
		running := runContextTestTask(t, f, trigger, "research", "Research the three vendors\nand compare prices", AgentRunStatusRunning, f.now)
		done := runContextTestTask(t, f, trigger, "inbox", "Read the inbox", AgentRunStatusCompleted, f.now.Add(-time.Minute))
		forged := runContextTestTask(t, f, trigger, "forged", "Forged", AgentRunStatusRunning, f.now)
		forged.WorkRun.Goal = "something else"
		other := runContextTestTask(t, f, trigger, "other", "Someone else's", AgentRunStatusRunning, f.now)
		other.Task.AuthenticatedActor = ConversationParticipant{Type: ConversationParticipantUser, ID: "stranger"}
		tasks := &conversationTaskListStore{catchUpTestStore: f.store, rows: []*ConversationTaskResult{running, done, forged, other}}
		runner := f.noteRunner()
		runner.portfolio = tasks

		run, session := f.open("")
		text := f.note(runner, f.noteInput(run, session, trigger)).Note.Text
		want := "Conversation tasks you started for this person here, newest first:\n" +
			"- task " + running.Task.ID + " (independent, running) workRunId work-research revision 3, started " + conversationMessageTime(f.now) +
			`: "Research the three vendors and compare prices"`
		if !strings.Contains(text, want) || !strings.Contains(text, "- task "+done.Task.ID+" (independent, completed) workRunId work-inbox") {
			t.Fatalf("tasks section = %q", text)
		}
		if strings.Contains(text, forged.Task.ID) || strings.Contains(text, other.Task.ID) {
			t.Fatalf("unverified tasks listed: %q", text)
		}
		if tasks.filter.ActiveOnly || tasks.filter.Limit != runContextMaximumTasks || tasks.filter.AuthenticatedActor != trigger.Sender {
			t.Fatalf("task filter = %#v", tasks.filter)
		}

		// When the tasks are gone the section says so once.
		tasks.rows = nil
		gone := f.note(runner, f.noteInput(f.nextRun(session), session, trigger)).Note.Text
		if !strings.Contains(gone, "Conversation tasks: none.") {
			t.Fatalf("after tasks = %q", gone)
		}
	})
}
