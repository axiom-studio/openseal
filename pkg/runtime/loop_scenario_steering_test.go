package runtime

// Steering and supersede journeys (run-loop section 12 §5.3, D8): a message
// that arrives while a chat Run is working is appended to the Run's session
// at its next step, and an answer the model wrote without having seen it is
// never posted. The Runs are driven by the real RunLoop through the loop
// fixture, whose chat bindings carry the real conversation seams
// (bindConversationLoop: the catch-up preparer, the run_context note and the
// chat delivery).

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// steerClaim claims runID and drives one claim with the fixture's binding,
// which binds the production conversation seams for a chat Run.
func steerClaim(f *looptest.Fixture, runID string) (runloop.RunLoopExit, *models.AgentRun) {
	f.T.Helper()
	claimed := f.Claim(runID)
	binding := f.Binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.MustRun(runID)
	}
	exit := f.Loop.Drive(f.Ctx, runloop.RunLoopClaim{Run: claimed, WorkerID: f.WorkerID, Binding: binding})
	return exit, f.MustRun(runID)
}

// steerToRest is driveToRest over steerClaim.
func steerToRest(f *looptest.Fixture, runID string, maxClaims int) *models.AgentRun {
	f.T.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := steerClaim(f, runID)
		if exit.Err != nil && exit.Reason != runloop.RunLoopExitRequeued {
			var events []string
			for _, event := range f.Events(runID) {
				events = append(events, event.EventType)
			}
			f.T.Fatalf("claim %d of %s exited %s: %v (run %s, lease %q until %v at %v, log %s, events %v)", i+1, runID, exit.Reason, exit.Err,
				run.Status, run.LeaseOwner, run.LeaseExpiresAt, f.Clock.Now(), steerShape(f, runID), events)
		}
		if run.Status != models.AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(f.Clock.Now()) {
			f.Clock.Advance(run.AvailableAt.Sub(f.Clock.Now()) + time.Millisecond)
		}
	}
	run := f.MustRun(runID)
	f.T.Fatalf("run %s not at rest after %d claims: %s (cursor %d/%d, %d steps, log %s)", runID, maxClaims, run.Status,
		run.ConsumedInterventions, len(run.PendingInterventions), len(f.Steps(runID)), steerShape(f, runID))
	return run
}

// steerShape is the Run's log shape without notes (run_context and other
// kernel notes are not part of the steering contract).
func steerShape(f *looptest.Fixture, runID string) string {
	f.T.Helper()
	var out []string
	for _, entry := range f.Entries(runID) {
		if entry.RunID != runID {
			continue
		}
		switch entry.Type {
		case models.SessionEntryNote:
			continue
		case models.SessionEntryTool:
			out = append(out, "tool:"+string(entry.State))
		default:
			out = append(out, string(entry.Type))
		}
	}
	return strings.Join(out, ",")
}

// steerUserEntries is the Run's user entries in log order.
func steerUserEntries(f *looptest.Fixture, runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.RunID == runID && entry.Type == models.SessionEntryUser && entry.User != nil {
			out = append(out, entry)
		}
	}
	return out
}

func steerAgentMessages(f *looptest.Fixture, conversationID string) []*models.ChannelMessage {
	f.T.Helper()
	var out []*models.ChannelMessage
	for _, message := range f.Messages(conversationID) {
		if message.Sender.Type == models.ConversationParticipantAgent {
			out = append(out, message)
		}
	}
	return out
}

func steerConversationRuns(f *looptest.Fixture, conversationID string) []*models.AgentRun {
	f.T.Helper()
	runs, err := f.Raw.ListAgentRuns(f.Ctx, models.AgentRunFilter{Scope: f.Scope, Kind: models.RunKindConversation, ConversationID: conversationID})
	if err != nil {
		f.T.Fatal(err)
	}
	return runs
}

// steerLookup answers lookup with the issue's status: 6815 is open, 6816 is
// closed.
func steerLookup(f *looptest.Fixture) {
	f.Dispatch.On("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		id, _ := in.Arguments["id"].(string)
		status := "open"
		if id == "6816" {
			status = "closed"
		}
		return map[string]interface{}{"id": id, "status": status, "title": "Issue " + id}, nil
	})
}

// steerText is a rendered user entry's text without the catch-up's message
// header line ("[ana (user ana) · <id> · <time>]").
func steerText(text string) string {
	if header, body, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(header, "[") && strings.HasSuffix(header, "]") {
		return body
	}
	return text
}

// steerUserTexts is every rendered user entry's text without its header.
func steerUserTexts(v looptest.StepView) []string {
	texts := v.UserTexts()
	for i := range texts {
		texts[i] = steerText(texts[i])
	}
	return texts
}

func steerHasLookupResult(v looptest.StepView, id string) bool {
	for _, e := range v.ToolResultsSinceUser() {
		if strings.Contains(looptest.PartsText(e.Tool.Result.Content), `"id":"`+id+`"`) {
			return true
		}
	}
	return false
}

// TestLoopSteeringMessageIsAppendedAtTheNextStep: a follow-up posted while
// step 1 runs is a user entry after step 1's tool result, read by step 2 of
// the same Run (D8). Nothing is discarded and no second Run starts.
func TestLoopSteeringMessageIsAppendedAtTheNextStep(t *testing.T) {
	looptest.EachStore(t, "steer_next_step", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var conversation *models.Conversation
		f = looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "first lookup", Times: 1,
				Before: func(context.Context, looptest.StepView) error {
					f.PostUser(conversation.ID, looptest.FixtureUser, "also check 6816")
					return nil
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Looking up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "6815"}))
				}},
			&looptest.ModelRule{Name: "second lookup", Times: 1,
				When: func(v looptest.StepView) bool { return slices.Contains(steerUserTexts(v), "also check 6816") },
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("And 6816.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "6816"}))
				}},
			&looptest.ModelRule{Name: "answer",
				When: func(v looptest.StepView) bool { return steerHasLookupResult(v, "6816") },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Say("6815 is open and 6816 is closed.")
				}},
		))
		steerLookup(f)
		var run *models.AgentRun
		conversation, run = f.StartChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got, want := steerShape(f, run.ID), "user,assistant,tool:completed,user,assistant,tool:completed,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		users := steerUserEntries(f, run.ID)
		steering := users[len(users)-1]
		if text := steerText(looptest.PartsText(steering.User.Content)); text != "also check 6816" {
			t.Fatalf("steering entry text = %q", text)
		}
		if steering.User.Author != (models.SessionAuthor{Type: "user", ID: looptest.FixtureUser}) {
			t.Fatalf("steering author = %#v, want user:%s", steering.User.Author, looptest.FixtureUser)
		}
		if steering.User.Origin != models.UserEntryChannelMessage || steering.Step != 2 {
			t.Fatalf("steering entry = origin %s step %d, want a channel message at step 2", steering.User.Origin, steering.Step)
		}
		// The catch-up renders the sender in the message header, not as the
		// entry's Author (loop_render attributes only interventions).
		if text := looptest.PartsText(steering.User.Content); !strings.Contains(text, looptest.FixtureUser) {
			t.Fatalf("step 2 renders the follow-up without its sender: %q", text)
		}

		if len(f.Events(run.ID, "turn.guidance_changed")) != 0 {
			t.Fatal("a step was discarded for new guidance")
		}
		steps := f.Steps(run.ID)
		if len(steps) != 3 {
			t.Fatalf("steps = %d, want 3", len(steps))
		}
		for _, step := range steps {
			if step.Status != models.AgentTurnStatusCompleted {
				t.Fatalf("step %d status = %s, want completed", step.Sequence, step.Status)
			}
		}
		if done.ConsumedInterventions != 1 || len(done.PendingInterventions) != 1 {
			t.Fatalf("intervention cursor = %d of %d, want 1 of 1", done.ConsumedInterventions, len(done.PendingInterventions))
		}
		if runs := steerConversationRuns(f, conversation.ID); len(runs) != 1 {
			t.Fatalf("conversation Runs = %d, want 1 (the follow-up started its own Run)", len(runs))
		}
		if f.Dispatch.Count("lookup") != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", f.Dispatch.Count("lookup"))
		}
		answers := steerAgentMessages(f, conversation.ID)
		if len(answers) != 1 || !strings.Contains(answers[0].Content, "6816 is closed") {
			t.Fatalf("channel = %s, want one answer", looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
	})
}

// TestLoopSupersedeBeforePostAppendsAndContinues: an answer written without
// the follow-up is never posted. It stays in the log so the model knows what
// it was about to say, and the next step answers the follow-up.
func TestLoopSupersedeBeforePostAppendsAndContinues(t *testing.T) {
	looptest.EachStore(t, "steer_supersede", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var conversation *models.Conversation
		f = looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "stale answer", Times: 1,
				Before: func(context.Context, looptest.StepView) error {
					f.PostUser(conversation.ID, looptest.FixtureUser, "wait, use 6816 instead")
					return nil
				},
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("6815 is open.") }},
			&looptest.ModelRule{Name: "fresh answer", Times: 1,
				When:  func(v looptest.StepView) bool { return steerText(v.LastUserText()) == "wait, use 6816 instead" },
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("6816 is closed.") }},
		))
		var run *models.AgentRun
		conversation, run = f.StartChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		messages := f.Messages(conversation.ID)
		if len(messages) != 3 || messages[0].Content != "is 6815 open?" || messages[1].Content != "wait, use 6816 instead" ||
			!strings.Contains(messages[2].Content, "6816 is closed.") {
			t.Fatalf("channel = %s, want the trigger, the follow-up and the fresh answer", looptest.DescribeMessages(messages))
		}
		for _, message := range messages {
			if strings.Contains(message.Content, "6815 is open.") {
				t.Fatalf("the superseded answer was posted: %s", looptest.DescribeMessages(messages))
			}
		}
		if got, want := steerShape(f, run.ID), "user,assistant,user,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		var assistants []string
		for _, entry := range f.Entries(run.ID) {
			if entry.RunID == run.ID && entry.Type == models.SessionEntryAssistant {
				assistants = append(assistants, entry.Assistant.Text)
			}
		}
		if len(assistants) != 2 || assistants[0] != "6815 is open." || assistants[1] != "6816 is closed." {
			t.Fatalf("assistant entries = %q", assistants)
		}
		if second := (looptest.StepView{Req: f.Model.Calls(models.ModelStepPurposeStep)[1]}); !strings.Contains(second.Describe(), "assistant") {
			t.Fatalf("step 2 does not render the unposted answer: %s", second.Describe())
		}
		if runs := steerConversationRuns(f, conversation.ID); len(runs) != 1 {
			t.Fatalf("conversation Runs = %d, want 1", len(runs))
		}
		if done.ConsumedInterventions != 1 || len(done.PendingInterventions) != 1 {
			t.Fatalf("intervention cursor = %d of %d, want 1 of 1", done.ConsumedInterventions, len(done.PendingInterventions))
		}
	})
}

// TestLoopSupersedeRaceNeverLosesAMessage races a follow-up against the
// re-check and the post: inserted from another goroutine at a random offset
// around the model's return, it is either read by the Run before the posted
// answer or it is the trigger of a later Run. It is never lost.
func TestLoopSupersedeRaceNeverLosesAMessage(t *testing.T) {
	const rounds = 20
	looptest.EachStore(t, "steer_race", func(t *testing.T, store models.KernelStore) {
		var mu sync.Mutex
		rng := rand.New(rand.NewSource(7))
		jitter := func() time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return time.Duration(rng.Intn(400)) * time.Microsecond
		}
		entered := make(chan struct{}, rounds*4)
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer",
			Before: func(context.Context, looptest.StepView) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				time.Sleep(jitter())
				return nil
			},
			Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say("Answer to: " + steerText(v.LastUserText()))
			}}))

		for round := 0; round < rounds; round++ {
			f.Clock.Advance(time.Second)
			for len(entered) > 0 {
				<-entered
			}
			conversation, run := f.StartChat(fmt.Sprintf("round %d question", round))
			followUp := fmt.Sprintf("round %d follow-up", round)
			posted := make(chan error, 1)
			go func() {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					posted <- errors.New("the model was never called")
					return
				}
				time.Sleep(jitter())
				posted <- steerPostConcurrently(f, conversation.ID, followUp)
			}()
			steerToRest(f, run.ID, 6)
			select {
			case err := <-posted:
				if err != nil {
					t.Fatalf("round %d: post follow-up: %v", round, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: follow-up post did not return", round)
			}
			// A follow-up that landed after the answer started its own Run.
			for _, later := range steerConversationRuns(f, conversation.ID) {
				if later.ID != run.ID && !models.IsTerminalAgentRunStatus(later.Status) {
					steerToRest(f, later.ID, 6)
				}
			}
			steerCheckNoMessageLost(t, f, conversation.ID, round)
		}
	})
}

// steerPostConcurrently posts text as ana and schedules it, retrying the
// conversation revision race with the Run's own post. It runs off the test
// goroutine, so it returns errors instead of failing the test.
func steerPostConcurrently(f *looptest.Fixture, conversationID, text string) error {
	for attempt := 0; attempt < 50; attempt++ {
		conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
		if err != nil {
			return err
		}
		posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: f.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: looptest.FixtureUser}, Intent: models.MessageIntentQuestion,
			Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			IdempotencyKey: "race-" + text,
		})
		if errors.Is(err, models.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return err
		}
		for try := 0; try < 50; try++ {
			_, _, err = f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversationID, posted.Message.ID)
			if !errors.Is(err, models.ErrRevisionConflict) {
				return err
			}
		}
		return err
	}
	return errors.New("conversation revision kept moving")
}

// steerCheckNoMessageLost checks the invariant: every person's message is
// either a user entry of a Run logged before that Run's posted answer, or the
// trigger of a Run that answered it.
func steerCheckNoMessageLost(t *testing.T, f *looptest.Fixture, conversationID string, round int) {
	t.Helper()
	runs := steerConversationRuns(f, conversationID)
	triggers := map[string]*models.AgentRun{}
	for _, run := range runs {
		if !models.IsTerminalAgentRunStatus(run.Status) {
			t.Fatalf("round %d: run %s is still %s", round, run.ID, run.Status)
		}
		if trigger, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string); trigger != "" {
			triggers[trigger] = run
		}
	}
	for _, message := range f.Messages(conversationID) {
		if message.Sender.Type != models.ConversationParticipantUser {
			continue
		}
		if run := triggers[message.ID]; run != nil && run.Status == models.AgentRunStatusCompleted && steerReadBeforeAnswer(f, run, message.ID) {
			continue
		}
		read := false
		for _, run := range runs {
			if steerReadBeforeAnswer(f, run, message.ID) {
				read = true
				break
			}
		}
		if !read {
			var shapes []string
			for _, run := range runs {
				shapes = append(shapes, fmt.Sprintf("%s(%s, cursor %d/%d): %s", run.ID, run.Status, run.ConsumedInterventions, len(run.PendingInterventions), steerShape(f, run.ID)))
			}
			t.Fatalf("round %d: message %q was lost\nchannel: %s\nruns: %s", round, message.Content,
				looptest.DescribeMessages(f.Messages(conversationID)), strings.Join(shapes, "\n  "))
		}
	}
}

// steerReadBeforeAnswer reports whether run logged messageID as a user entry
// before the assistant entry of its posted answer (its last one).
func steerReadBeforeAnswer(f *looptest.Fixture, run *models.AgentRun, messageID string) bool {
	if run.Status != models.AgentRunStatusCompleted {
		return false
	}
	seen := false
	answered := false
	for _, entry := range f.Entries(run.ID) {
		if entry.RunID != run.ID {
			continue
		}
		switch {
		case entry.Type == models.SessionEntryUser && entry.User != nil && entry.User.MessageID == messageID:
			seen = true
			answered = false
		case entry.Type == models.SessionEntryAssistant && seen:
			answered = true
		}
	}
	return seen && answered
}

// TestLoopSteeringIsCatchUpOfVisibleMessagesOnly: the steering catch-up reads
// what the Agent can see. A message addressed to someone else and the Run's
// own progress message, both posted mid-Run, are not appended; the visible
// follow-up posted after them is.
func TestLoopSteeringIsCatchUpOfVisibleMessagesOnly(t *testing.T) {
	looptest.EachStore(t, "steer_visible", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var conversation *models.Conversation
		var run *models.AgentRun
		f = looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "first", Times: 1,
				Before: func(context.Context, looptest.StepView) error {
					steerPostAs(f, conversation.ID, models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: looptest.FixtureUser},
						"private note for bob", models.ConversationAudience{Kind: models.ConversationAudienceParticipants,
							Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "bob"}}}, nil)
					steerPostAs(f, conversation.ID, models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: looptest.FixtureAgent},
						"Working on it.", models.ConversationAudience{Kind: models.ConversationAudienceChannel},
						[]models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}})
					f.PostUser(conversation.ID, looptest.FixtureUser, "visible follow-up")
					return nil
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Looking up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "6815"}))
				}},
			&looptest.ModelRule{Name: "answer",
				When:  func(v looptest.StepView) bool { return steerText(v.LastUserText()) == "visible follow-up" },
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Done.") }},
		))
		conversation, run = f.StartChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		var texts []string
		for _, entry := range steerUserEntries(f, run.ID) {
			texts = append(texts, steerText(looptest.PartsText(entry.User.Content)))
		}
		if strings.Join(texts, "|") != "is 6815 open?|visible follow-up" {
			t.Fatalf("user entries = %q, want the trigger and the visible follow-up", texts)
		}
		for _, req := range f.Model.Calls(models.ModelStepPurposeStep) {
			for _, entry := range req.Entries {
				rendered := ""
				switch {
				case entry.User != nil:
					rendered = looptest.PartsText(entry.User.Content)
				case entry.Note != nil:
					rendered = entry.Note.Text
				}
				if strings.Contains(rendered, "private note for bob") || strings.Contains(rendered, "Working on it.") {
					t.Fatalf("step %s renders a message the catch-up must skip: %q", req.StepID, rendered)
				}
			}
		}
		if got, want := steerShape(f, run.ID), "user,assistant,tool:completed,user,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
	})
}

// steerPostAs posts a message without scheduling it.
func steerPostAs(f *looptest.Fixture, conversationID string, sender models.ConversationParticipant, text string, audience models.ConversationAudience, refs []models.ConversationReference) {
	f.T.Helper()
	conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
	if err != nil {
		f.T.Fatal(err)
	}
	intent := models.MessageIntentQuestion
	if sender.Type == models.ConversationParticipantAgent {
		intent = models.MessageIntentUpdate
	}
	if _, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: sender, Intent: intent, Content: text, Audience: audience, References: refs,
		IdempotencyKey: "steer-" + text,
	}); err != nil {
		f.T.Fatalf("post %q: %v", text, err)
	}
}

// TestRapidMessagesShareOneRunThatSeesAll: three messages from one person
// before the Run's first step make one Run. Its first step renders all three
// as user entries in order, and it posts one answer to the first.
func TestRapidMessagesShareOneRunThatSeesAll(t *testing.T) {
	looptest.EachStore(t, "steer_rapid", func(t *testing.T, store models.KernelStore) {
		first, second, third := "Find the cheapest USB-C hub", "It must have HDMI", "Use my card ending with 1001 to buy it"
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer",
			When: func(v looptest.StepView) bool {
				texts := steerUserTexts(v)
				return len(texts) >= 3 && slices.Equal(texts[len(texts)-3:], []string{first, second, third})
			},
			Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("Bought the Anker hub with HDMI using the card ending 1001.")
			}}))
		conversation, run := f.StartChat(first)
		for _, text := range []string{second, third} {
			message := f.PostUser(conversation.ID, looptest.FixtureUser, text)
			if runs := steerConversationRuns(f, conversation.ID); len(runs) != 1 {
				t.Fatalf("message %s started a parallel Run (%d Runs)", message.ID, len(runs))
			}
		}
		queued := f.MustRun(run.ID)
		if followUps := models.ConversationRunFollowUps(queued); queued.Status != models.AgentRunStatusQueued || len(followUps) != 2 {
			t.Fatalf("follow-ups were not delivered to the queued Run: %#v", queued.PendingInterventions)
		}
		reconciled, err := f.Scheduler.ReconcileScope(f.Ctx, f.Scope)
		if err != nil || reconciled.Scheduled != 0 {
			t.Fatalf("reconciliation = %#v, %v", reconciled, err)
		}
		if replayed := f.MustRun(run.ID); len(replayed.PendingInterventions) != 2 {
			t.Fatalf("replay duplicated delivery: %d interventions", len(replayed.PendingInterventions))
		}

		// The scheduler's follow-up delivery stamps the Run on the wall clock.
		f.Clock.Advance(time.Second)
		done := steerToRest(f, run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if steps := f.Model.Calls(models.ModelStepPurposeStep); len(steps) != 1 {
			t.Fatalf("model steps = %d, want 1", len(steps))
		}
		if got, want := steerShape(f, run.ID), "user,user,user,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		if done.ConsumedInterventions != 2 {
			t.Fatalf("intervention cursor = %d, want 2", done.ConsumedInterventions)
		}
		messages := f.Messages(conversation.ID)
		answers := steerAgentMessages(f, conversation.ID)
		if len(answers) != 1 || answers[0].ReplyToMessageID != messages[0].ID || !strings.Contains(answers[0].Content, "Anker") {
			t.Fatalf("channel = %s, want one answer replying to the first message", looptest.DescribeMessages(messages))
		}

		// A terminal Run never absorbs: the next message starts a new Run.
		fourth := f.PostUser(conversation.ID, looptest.FixtureUser, "Now track the delivery")
		runs := steerConversationRuns(f, conversation.ID)
		if len(runs) != 2 {
			t.Fatalf("conversation Runs = %d, want 2", len(runs))
		}
		for _, next := range runs {
			if next.ID != run.ID && next.Context[models.ConversationRunTriggerIDContextKey] != fourth.ID {
				t.Fatalf("the new Run answers %v, want %s", next.Context[models.ConversationRunTriggerIDContextKey], fourth.ID)
			}
		}
		if current := f.MustRun(run.ID); len(current.PendingInterventions) != 2 {
			t.Fatalf("terminal Run changed: %d interventions", len(current.PendingInterventions))
		}
	})
}
