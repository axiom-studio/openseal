package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// The run_context note (run-loop section 05 §5, CONV-04), on the
// SQLite store through the catch-up fixture.

func catchUpNoteRunner(f *conversationtest.CatchUpFixture) *ConversationRunTurnRunner {
	return &ConversationRunTurnRunner{conversations: f.Service, portfolio: f.Store, runbooks: f.Store}
}

func catchUpNoteInput(f *conversationtest.CatchUpFixture, run *models.AgentRun, session *models.AgentSession, trigger *models.ChannelMessage) conversation.ConversationContextInput {
	return conversation.ConversationContextInput{
		Run: run, Session: session, Sessions: f.Store, Conversation: f.Conversation, Trigger: trigger,
		Viewer: f.Viewer(), Step: 1,
	}
}

// catchUpNote builds the run_context note of in and appends it to the session.
func catchUpNote(f *conversationtest.CatchUpFixture, runner *ConversationRunTurnRunner, in conversation.ConversationContextInput) *models.SessionEntry {
	f.T.Helper()
	note, err := runner.conversationRunContextNote(f.T.Context(), in)
	if err != nil {
		f.T.Fatalf("run context: %v", err)
	}
	if note == nil {
		return nil
	}
	if _, err := f.Store.AppendAgentSessionEntries(f.T.Context(), models.AppendAgentSessionEntriesRequest{
		Scope: in.Session.Scope, SessionID: in.Session.ID, RunID: in.Run.ID, Lease: &models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.Now},
		Entries: []*models.SessionEntry{note}, Now: f.Now,
	}); err != nil {
		f.T.Fatalf("append run context: %v", err)
	}
	return note
}

// catchUpNextRun claims another Run of the lane session.
func catchUpNextRun(f *conversationtest.CatchUpFixture, session *models.AgentSession) *models.AgentRun {
	f.T.Helper()
	return f.Run(session.ID, true)
}

func runContextSectionNames(t *testing.T, note *models.SessionEntry) map[string]bool {
	t.Helper()
	if note == nil {
		return map[string]bool{}
	}
	var data struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
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
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		runner := catchUpNoteRunner(f)
		trigger := f.Post(conversation.PostChannelMessageRequest{IdempotencyKey: "q1", Content: "What is on today?", SenderDisplayName: "Kev"})
		run, session := f.Open("")
		first := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger))
		if first == nil || first.Note.Kind != models.NoteRunContext || first.SourceKey != models.RunContextSourceKey(run.ID) ||
			first.ID != models.SessionEntryID(session.ID, models.RunContextSourceKey(run.ID)) || first.Step != 1 {
			t.Fatalf("first note = %#v", first)
		}
		want := "<run_context>\nReply to message " + trigger.ID + " from Kev (user user)."
		if !strings.HasPrefix(first.Note.Text, want) || !strings.HasSuffix(first.Note.Text, "\n</run_context>") {
			t.Fatalf("note text = %q", first.Note.Text)
		}
		if names := runContextSectionNames(t, first); !names[conversation.ContextSectionRespondTo] || !names[conversation.ContextSectionChannel] || !names[conversation.ContextSectionActiveRuns] {
			t.Fatalf("first sections = %v", names)
		}
		// The next Run of the lane sees nothing new but still gets respond_to.
		second := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		names := runContextSectionNames(t, second)
		if second == nil || len(names) != 1 || !names[conversation.ContextSectionRespondTo] {
			t.Fatalf("second note sections = %v (%v)", names, second)
		}
	})
}

func TestRunContextSectionsEmittedOnlyWhenChanged(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		runner := catchUpNoteRunner(f)
		style := "watercolor"
		runner.config.ImageStyle = func(context.Context, *models.AgentRun) string { return style }
		trigger := f.Say("q1", "Draw me a cat")
		run, session := f.Open("")
		first := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger))
		if names := runContextSectionNames(t, first); !names[conversation.ContextSectionImageStyle] || !names[conversation.ContextSectionChannel] {
			t.Fatalf("first sections = %v", names)
		}
		if !strings.Contains(first.Note.Text, "Preferred picture style: watercolor.") || !strings.Contains(first.Note.Text, `Channel "Catch-up" (`+f.Conversation.ID+`).`) {
			t.Fatalf("first text = %q", first.Note.Text)
		}

		unchanged := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if names := runContextSectionNames(t, unchanged); names[conversation.ContextSectionImageStyle] || names[conversation.ContextSectionChannel] || names[conversation.ContextSectionActiveRuns] {
			t.Fatalf("unchanged sections re-sent: %v", names)
		}

		style = "ink"
		changed := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if names := runContextSectionNames(t, changed); !names[conversation.ContextSectionImageStyle] || names[conversation.ContextSectionChannel] {
			t.Fatalf("changed sections = %v", names)
		}
		if !strings.Contains(changed.Note.Text, "Preferred picture style: ink.") {
			t.Fatalf("changed text = %q", changed.Note.Text)
		}

		// A section that became empty is sent once with its none text.
		style = ""
		cleared := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if !strings.Contains(cleared.Note.Text, "Preferred picture style: none.") {
			t.Fatalf("cleared text = %q", cleared.Note.Text)
		}
		again := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if names := runContextSectionNames(t, again); names[conversation.ContextSectionImageStyle] {
			t.Fatalf("none text re-sent: %q", again.Note.Text)
		}
	})
}

// A person who works on their own computer by default is told so at the
// Run's start; switching back to the VM sends the VM line once.
func TestRunContextNamesTheWorkMachine(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		runner := catchUpNoteRunner(f)
		machine := "Files and commands: the person's computer krypton."
		runner.config.WorkMachine = func(context.Context, *models.AgentRun) string { return machine }
		trigger := f.Say("q1", "can you do ls?")
		run, session := f.Open("")
		first := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger))
		if names := runContextSectionNames(t, first); !names[conversation.ContextSectionWorkMachine] || !strings.Contains(first.Note.Text, machine) {
			t.Fatalf("first = %v %q", names, first.Note.Text)
		}
		machine = ""
		back := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if !strings.Contains(back.Note.Text, "Files and commands: the chat's secure VM.") {
			t.Fatalf("back on the VM = %q", back.Note.Text)
		}
	})
}

func TestRunContextReEmittedAfterCompaction(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		runner := catchUpNoteRunner(f)
		trigger := f.Say("q1", "Hello")
		run, session := f.Open("")
		session = f.Commit(run, session, f.Collect(f.Request(run, session, trigger.Sequence)))
		first := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger))

		next := catchUpNextRun(f, session)
		if _, err := f.Store.AppendAgentSessionEntries(t.Context(), models.AppendAgentSessionEntriesRequest{
			Scope: session.Scope, SessionID: session.ID, RunID: next.ID, Lease: &models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.Now},
			Entries: []*models.SessionEntry{{ID: "compaction-1", Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntry{
				Summary: "They said hello.", FirstKeptEntryID: first.ID, Trigger: models.CompactionTriggerThreshold, Provider: "openai", Model: "gpt-x",
			}}},
			Now: f.Now,
		}); err != nil {
			t.Fatalf("append compaction: %v", err)
		}
		after := catchUpNote(f, runner, catchUpNoteInput(f, next, session, trigger))
		if names := runContextSectionNames(t, after); !names[conversation.ContextSectionChannel] || !names[conversation.ContextSectionActiveRuns] {
			t.Fatalf("sections after compaction = %v", names)
		}
	})
}

func TestActiveRunsBecomesNoneWhenWorkEnds(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		ctx := t.Context()
		runner := catchUpNoteRunner(f)
		trigger := f.Say("q1", "Run the report")
		portfolio := portfolio.NewPortfolioService(f.Store)
		root, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: f.Conversation.Scope, Owner: f.Conversation.Owner, AssignedAgentID: f.AgentID, Kind: models.RunKindConversation,
			Goal: "chat", Source: models.RunSourceChat,
			Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: f.Conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		child, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: f.Conversation.Scope, Owner: f.Conversation.Owner, AssignedAgentID: f.AgentID, ParentRunID: root.ID,
			Goal: "Build the weekly report", Source: models.RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, session := f.Open("")
		first := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger))
		if !strings.Contains(first.Note.Text, "Active work:\n- run "+child.ID+" (") || !strings.Contains(first.Note.Text, `"Build the weekly report"`) {
			t.Fatalf("active work = %q", first.Note.Text)
		}
		current, err := f.Store.GetAgentRun(ctx, child.Scope, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := activity.NewRunActivityService(f.Store, f.Store).TransitionRun(ctx, child.Scope, child.ID, activity.RunTransitionRequest{
			ExpectedRevision: current.Revision, Status: models.AgentRunStatusCanceled, Summary: "Canceled", EventType: "run.canceled",
		}); err != nil {
			t.Fatal(err)
		}
		ended := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if names := runContextSectionNames(t, ended); !names[conversation.ContextSectionActiveRuns] || !strings.Contains(ended.Note.Text, "Active work: none.") {
			t.Fatalf("after the work ended = %q", ended.Note.Text)
		}
	})
}

func TestTeamRosterSectionAndRoutingReason(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		participants := conversation.ConversationParticipantSourceFunc(func(context.Context, conversation.ConversationParticipantQuery) ([]conversation.ConversationParticipantBinding, error) {
			return []conversation.ConversationParticipantBinding{{Participant: f.Agent()}, {Participant: conversationtest.AgentParticipant("maya")}}, nil
		})
		router := conversation.ConversationRouterFunc(func(context.Context, conversation.ConversationRouteRequest) (conversation.ConversationRouteDecision, error) {
			return conversation.ConversationRouteDecision{AgentID: f.AgentID}, nil
		})
		coordinator, err := conversation.NewConversationCoordinator(f.Service, participants, router, conversation.DefaultConversationCoordinatorConfig())
		if err != nil {
			t.Fatal(err)
		}
		runner := catchUpNoteRunner(f)
		runner.coordinator = coordinator
		trigger := f.Say("q1", "Who writes the launch post?")
		run, session := f.Open("")
		member := models.CloneAgentRun(run)
		member.Kind, member.Owner = models.RunKindConversation, f.Conversation.Owner
		member.Context = map[string]interface{}{models.RoutingReasonContextKey: "owns the launch copy"}
		note := catchUpNote(f, runner, catchUpNoteInput(f, member, session, trigger))
		for _, want := range []string{
			"You were picked to answer because: owns the launch copy.",
			"Team roster:\n- agent (agent), you\n- maya (maya)",
		} {
			if !strings.Contains(note.Note.Text, want) {
				t.Fatalf("note lacks %q: %q", want, note.Note.Text)
			}
		}
		if names := runContextSectionNames(t, note); !names[conversation.ContextSectionTeam] {
			t.Fatalf("sections = %v", names)
		}
		// A 1:1 Agent Run has no Team section.
		solo := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger))
		if strings.Contains(solo.Note.Text, "Team roster:\n") || strings.Contains(solo.Note.Text, "picked to answer") {
			t.Fatalf("non-member note = %q", solo.Note.Text)
		}
	})
}

func TestVoiceCallGreetingInRespondTo(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		runner := catchUpNoteRunner(f)
		spoken := f.Say("q1", "What's the weather?")
		spoken.ResponseMode = "spoken"
		run, session := f.Open("")
		text := catchUpNote(f, runner, catchUpNoteInput(f, run, session, spoken)).Note.Text
		if !strings.Contains(text, "Spoken reply: answer in words to be spoken.") || strings.Contains(text, "Voice call just started.") {
			t.Fatalf("spoken respond_to = %q", text)
		}

		call := &models.ChannelMessage{
			ID: "call-start", Scope: f.Conversation.Scope, ConversationID: f.Conversation.ID, Sequence: spoken.Sequence + 1,
			Sender:    models.ConversationParticipant{Type: models.ConversationParticipantService, ID: models.VoiceCallCoordinatorParticipantID},
			Initiator: &models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"},
			Intent:    models.MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true, Content: "Call started",
		}
		text = catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, call)).Note.Text
		want := "Reply to message call-start from " + models.VoiceCallCoordinatorParticipantID + " (service " + models.VoiceCallCoordinatorParticipantID + ").\n" +
			"Spoken reply: answer in words to be spoken.\nVoice call just started. " + models.VoiceCallGreetingBehaviorInstruction
		if !strings.Contains(text, want) {
			t.Fatalf("voice call respond_to = %q", text)
		}
	})
}

// conversationTaskListStore returns fixed task rows.
type conversationTaskListStore struct {
	conversationtest.CatchUpStore
	rows   []*models.ConversationTaskResult
	filter models.ConversationTaskFilter
}

func (s *conversationTaskListStore) ListConversationTasks(_ context.Context, filter models.ConversationTaskFilter) ([]*models.ConversationTaskResult, error) {
	s.filter = filter
	return s.rows, nil
}

func runContextTestTask(t *testing.T, f *conversationtest.CatchUpFixture, trigger *models.ChannelMessage, key, goal string, status models.AgentRunStatus, created time.Time) *models.ConversationTaskResult {
	t.Helper()
	task := &models.ConversationTask{
		Mode: models.ConversationTaskModeIndependent, Scope: f.Conversation.Scope, Owner: f.Conversation.Owner, ConversationID: f.Conversation.ID,
		SourceMessageID: trigger.ID, ThreadRootID: trigger.ID, SourceRunID: "source-" + key, SourceTurnID: "turn-" + key, SourceTurnNumber: 1,
		AuthenticatedActor: trigger.Sender, TargetAgentID: f.AgentID, WorkRunID: "work-" + key, TaskKey: key, Goal: goal,
		Acknowledgment: "On it.", Revision: 1, CreatedAt: created,
	}
	task.ID = models.ConversationTaskID(task.Scope, task.SourceRunID, task.TaskKey)
	digest, err := models.ConversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	task.RequestDigest = digest
	work := &models.AgentRun{
		ID: task.WorkRunID, Scope: task.Scope, Owner: task.Owner, AssignedAgentID: f.AgentID, Kind: models.RunKindAgentWork, Goal: goal,
		Source: models.RunSourceChat, RootRunID: task.WorkRunID, ConcurrencyKey: "task:" + task.ID, Status: status, Revision: 3,
		Context: map[string]interface{}{models.ConversationTaskContextKey: task.ID, models.ConversationRunConversationIDContextKey: task.ConversationID,
			models.ConversationRunTriggerIDContextKey: task.SourceMessageID},
	}
	if !models.ConversationTaskMatchesWorkRun(task, work) {
		t.Fatalf("fixture task does not match its work run")
	}
	return &models.ConversationTaskResult{Task: task, WorkRun: work}
}

func TestRunContextListsRecentConversationTasks(t *testing.T) {
	conversationtest.ForCatchUpStores(t, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *conversationtest.CatchUpFixture) {
		trigger := f.Say("q1", "How is the research going?")
		running := runContextTestTask(t, f, trigger, "research", "Research the three vendors\nand compare prices", models.AgentRunStatusRunning, f.Now)
		done := runContextTestTask(t, f, trigger, "inbox", "Read the inbox", models.AgentRunStatusCompleted, f.Now.Add(-time.Minute))
		forged := runContextTestTask(t, f, trigger, "forged", "Forged", models.AgentRunStatusRunning, f.Now)
		forged.WorkRun.Goal = "something else"
		other := runContextTestTask(t, f, trigger, "other", "Someone else's", models.AgentRunStatusRunning, f.Now)
		other.Task.AuthenticatedActor = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "stranger"}
		tasks := &conversationTaskListStore{CatchUpStore: f.Store, rows: []*models.ConversationTaskResult{running, done, forged, other}}
		runner := catchUpNoteRunner(f)
		runner.portfolio = tasks

		run, session := f.Open("")
		text := catchUpNote(f, runner, catchUpNoteInput(f, run, session, trigger)).Note.Text
		want := "Conversation tasks you started for this person here, newest first:\n" +
			"- task " + running.Task.ID + " (independent, running) workRunId work-research revision 3, started " + conversation.ConversationMessageTime(f.Now) +
			`: "Research the three vendors and compare prices"`
		if !strings.Contains(text, want) || !strings.Contains(text, "- task "+done.Task.ID+" (independent, completed) workRunId work-inbox") {
			t.Fatalf("tasks section = %q", text)
		}
		if strings.Contains(text, forged.Task.ID) || strings.Contains(text, other.Task.ID) {
			t.Fatalf("unverified tasks listed: %q", text)
		}
		if tasks.filter.ActiveOnly || tasks.filter.Limit != conversation.RunContextMaximumTasks || tasks.filter.AuthenticatedActor != trigger.Sender {
			t.Fatalf("task filter = %#v", tasks.filter)
		}

		// When the tasks are gone the section says so once.
		tasks.rows = nil
		gone := catchUpNote(f, runner, catchUpNoteInput(f, catchUpNextRun(f, session), session, trigger)).Note.Text
		if !strings.Contains(gone, "Conversation tasks: none.") {
			t.Fatalf("after tasks = %q", gone)
		}
	})
}
