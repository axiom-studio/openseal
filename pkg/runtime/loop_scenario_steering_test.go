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

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// steerClaim claims runID and drives one claim with the fixture's binding,
// which binds the production conversation seams for a chat Run.
func steerClaim(f *loopFixture, runID string) (LoopExit, *AgentRun) {
	f.t.Helper()
	claimed := f.claim(runID)
	binding := f.binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.mustRun(runID)
	}
	exit := f.loop.Drive(f.ctx, RunLoopClaim{Run: claimed, WorkerID: f.workerID, Binding: binding})
	return exit, f.mustRun(runID)
}

// steerToRest is driveToRest over steerClaim.
func steerToRest(f *loopFixture, runID string, maxClaims int) *AgentRun {
	f.t.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := steerClaim(f, runID)
		if exit.Err != nil && exit.Reason != RunLoopExitRequeued {
			var events []string
			for _, event := range f.events(runID) {
				events = append(events, event.EventType)
			}
			f.t.Fatalf("claim %d of %s exited %s: %v (run %s, lease %q until %v at %v, log %s, events %v)", i+1, runID, exit.Reason, exit.Err,
				run.Status, run.LeaseOwner, run.LeaseExpiresAt, f.clock.Now(), steerShape(f, runID), events)
		}
		if run.Status != AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(f.clock.Now()) {
			f.clock.Advance(run.AvailableAt.Sub(f.clock.Now()) + time.Millisecond)
		}
	}
	run := f.mustRun(runID)
	f.t.Fatalf("run %s not at rest after %d claims: %s (cursor %d/%d, %d steps, log %s)", runID, maxClaims, run.Status,
		run.ConsumedInterventions, len(run.PendingInterventions), len(f.steps(runID)), steerShape(f, runID))
	return run
}

// steerShape is the Run's log shape without notes (run_context and other
// kernel notes are not part of the steering contract).
func steerShape(f *loopFixture, runID string) string {
	f.t.Helper()
	var out []string
	for _, entry := range f.entries(runID) {
		if entry.RunID != runID {
			continue
		}
		switch entry.Type {
		case SessionEntryNote:
			continue
		case SessionEntryTool:
			out = append(out, "tool:"+string(entry.State))
		default:
			out = append(out, string(entry.Type))
		}
	}
	return strings.Join(out, ",")
}

// steerUserEntries is the Run's user entries in log order.
func steerUserEntries(f *loopFixture, runID string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range f.entries(runID) {
		if entry.RunID == runID && entry.Type == SessionEntryUser && entry.User != nil {
			out = append(out, entry)
		}
	}
	return out
}

func steerAgentMessages(f *loopFixture, conversationID string) []*ChannelMessage {
	f.t.Helper()
	var out []*ChannelMessage
	for _, message := range f.messages(conversationID) {
		if message.Sender.Type == ConversationParticipantAgent {
			out = append(out, message)
		}
	}
	return out
}

func steerConversationRuns(f *loopFixture, conversationID string) []*AgentRun {
	f.t.Helper()
	runs, err := f.raw.ListAgentRuns(f.ctx, AgentRunFilter{Scope: f.scope, Kind: RunKindConversation, ConversationID: conversationID})
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

// steerLookup answers lookup with the issue's status: 6815 is open, 6816 is
// closed.
func steerLookup(f *loopFixture) {
	f.dispatch.on("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
func steerUserTexts(v stepView) []string {
	texts := v.UserTexts()
	for i := range texts {
		texts[i] = steerText(texts[i])
	}
	return texts
}

func steerHasLookupResult(v stepView, id string) bool {
	for _, e := range v.ToolResultsSinceUser() {
		if strings.Contains(kitPartsText(e.Tool.Result.Content), `"id":"`+id+`"`) {
			return true
		}
	}
	return false
}

// TestLoopSteeringMessageIsAppendedAtTheNextStep: a follow-up posted while
// step 1 runs is a user entry after step 1's tool result, read by step 2 of
// the same Run (D8). Nothing is discarded and no second Run starts.
func TestLoopSteeringMessageIsAppendedAtTheNextStep(t *testing.T) {
	eachStore(t, "steer_next_step", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		var conversation *Conversation
		f = newLoopFixture(t, store, withRules(
			&modelRule{name: "first lookup", times: 1,
				before: func(context.Context, stepView) error {
					f.postUser(conversation.ID, loopFixtureUser, "also check 6816")
					return nil
				},
				reply: func(v stepView) ModelStepResult {
					return act("Looking up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "6815"}))
				}},
			&modelRule{name: "second lookup", times: 1,
				when: func(v stepView) bool { return slices.Contains(steerUserTexts(v), "also check 6816") },
				reply: func(v stepView) ModelStepResult {
					return act("And 6816.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "6816"}))
				}},
			&modelRule{name: "answer",
				when:  func(v stepView) bool { return steerHasLookupResult(v, "6816") },
				reply: func(stepView) ModelStepResult { return say("6815 is open and 6816 is closed.") }},
		))
		steerLookup(f)
		var run *AgentRun
		conversation, run = f.startChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.mustStatus(done, AgentRunStatusCompleted)

		if got, want := steerShape(f, run.ID), "user,assistant,tool:completed,user,assistant,tool:completed,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		users := steerUserEntries(f, run.ID)
		steering := users[len(users)-1]
		if text := steerText(kitPartsText(steering.User.Content)); text != "also check 6816" {
			t.Fatalf("steering entry text = %q", text)
		}
		if steering.User.Author != (SessionAuthor{Type: "user", ID: loopFixtureUser}) {
			t.Fatalf("steering author = %#v, want user:%s", steering.User.Author, loopFixtureUser)
		}
		if steering.User.Origin != UserEntryChannelMessage || steering.Step != 2 {
			t.Fatalf("steering entry = origin %s step %d, want a channel message at step 2", steering.User.Origin, steering.Step)
		}
		// The catch-up renders the sender in the message header, not as the
		// entry's Author (loop_render attributes only interventions).
		if text := kitPartsText(steering.User.Content); !strings.Contains(text, loopFixtureUser) {
			t.Fatalf("step 2 renders the follow-up without its sender: %q", text)
		}

		if len(f.events(run.ID, "turn.guidance_changed")) != 0 {
			t.Fatal("a step was discarded for new guidance")
		}
		steps := f.steps(run.ID)
		if len(steps) != 3 {
			t.Fatalf("steps = %d, want 3", len(steps))
		}
		for _, step := range steps {
			if step.Status != AgentTurnStatusCompleted {
				t.Fatalf("step %d status = %s, want completed", step.Sequence, step.Status)
			}
		}
		if done.ConsumedInterventions != 1 || len(done.PendingInterventions) != 1 {
			t.Fatalf("intervention cursor = %d of %d, want 1 of 1", done.ConsumedInterventions, len(done.PendingInterventions))
		}
		if runs := steerConversationRuns(f, conversation.ID); len(runs) != 1 {
			t.Fatalf("conversation Runs = %d, want 1 (the follow-up started its own Run)", len(runs))
		}
		if f.dispatch.count("lookup") != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", f.dispatch.count("lookup"))
		}
		answers := steerAgentMessages(f, conversation.ID)
		if len(answers) != 1 || !strings.Contains(answers[0].Content, "6816 is closed") {
			t.Fatalf("channel = %s, want one answer", kitDescribeMessages(f.messages(conversation.ID)))
		}
	})
}

// TestLoopSupersedeBeforePostAppendsAndContinues: an answer written without
// the follow-up is never posted. It stays in the log so the model knows what
// it was about to say, and the next step answers the follow-up.
func TestLoopSupersedeBeforePostAppendsAndContinues(t *testing.T) {
	eachStore(t, "steer_supersede", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		var conversation *Conversation
		f = newLoopFixture(t, store, withRules(
			&modelRule{name: "stale answer", times: 1,
				before: func(context.Context, stepView) error {
					f.postUser(conversation.ID, loopFixtureUser, "wait, use 6816 instead")
					return nil
				},
				reply: func(stepView) ModelStepResult { return say("6815 is open.") }},
			&modelRule{name: "fresh answer", times: 1,
				when:  func(v stepView) bool { return steerText(v.LastUserText()) == "wait, use 6816 instead" },
				reply: func(stepView) ModelStepResult { return say("6816 is closed.") }},
		))
		var run *AgentRun
		conversation, run = f.startChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.mustStatus(done, AgentRunStatusCompleted)

		messages := f.messages(conversation.ID)
		if len(messages) != 3 || messages[0].Content != "is 6815 open?" || messages[1].Content != "wait, use 6816 instead" ||
			!strings.Contains(messages[2].Content, "6816 is closed.") {
			t.Fatalf("channel = %s, want the trigger, the follow-up and the fresh answer", kitDescribeMessages(messages))
		}
		for _, message := range messages {
			if strings.Contains(message.Content, "6815 is open.") {
				t.Fatalf("the superseded answer was posted: %s", kitDescribeMessages(messages))
			}
		}
		if got, want := steerShape(f, run.ID), "user,assistant,user,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		var assistants []string
		for _, entry := range f.entries(run.ID) {
			if entry.RunID == run.ID && entry.Type == SessionEntryAssistant {
				assistants = append(assistants, entry.Assistant.Text)
			}
		}
		if len(assistants) != 2 || assistants[0] != "6815 is open." || assistants[1] != "6816 is closed." {
			t.Fatalf("assistant entries = %q", assistants)
		}
		if second := (stepView{req: f.model.calls(ModelStepPurposeStep)[1]}); !strings.Contains(second.describe(), "assistant") {
			t.Fatalf("step 2 does not render the unposted answer: %s", second.describe())
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
	eachStore(t, "steer_race", func(t *testing.T, store KernelStore) {
		var mu sync.Mutex
		rng := rand.New(rand.NewSource(7))
		jitter := func() time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return time.Duration(rng.Intn(400)) * time.Microsecond
		}
		entered := make(chan struct{}, rounds*4)
		f := newLoopFixture(t, store, withRules(&modelRule{name: "answer",
			before: func(context.Context, stepView) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				time.Sleep(jitter())
				return nil
			},
			reply: func(v stepView) ModelStepResult { return say("Answer to: " + steerText(v.LastUserText())) }}))

		for round := 0; round < rounds; round++ {
			f.clock.Advance(time.Second)
			for len(entered) > 0 {
				<-entered
			}
			conversation, run := f.startChat(fmt.Sprintf("round %d question", round))
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
				if later.ID != run.ID && !isTerminalAgentRunStatus(later.Status) {
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
func steerPostConcurrently(f *loopFixture, conversationID, text string) error {
	for attempt := 0; attempt < 50; attempt++ {
		conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
		if err != nil {
			return err
		}
		posted, err := f.convs.PostChannelMessage(f.ctx, PostChannelMessageRequest{
			Scope: f.scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: loopFixtureUser}, Intent: MessageIntentQuestion,
			Content: text, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			IdempotencyKey: "race-" + text,
		})
		if errors.Is(err, ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return err
		}
		for try := 0; try < 50; try++ {
			_, _, err = f.scheduler.ScheduleMessage(f.ctx, f.scope, conversationID, posted.Message.ID)
			if !errors.Is(err, ErrRevisionConflict) {
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
func steerCheckNoMessageLost(t *testing.T, f *loopFixture, conversationID string, round int) {
	t.Helper()
	runs := steerConversationRuns(f, conversationID)
	triggers := map[string]*AgentRun{}
	for _, run := range runs {
		if !isTerminalAgentRunStatus(run.Status) {
			t.Fatalf("round %d: run %s is still %s", round, run.ID, run.Status)
		}
		if trigger, _ := run.Context[conversationRunContextTriggerID].(string); trigger != "" {
			triggers[trigger] = run
		}
	}
	for _, message := range f.messages(conversationID) {
		if message.Sender.Type != ConversationParticipantUser {
			continue
		}
		if run := triggers[message.ID]; run != nil && run.Status == AgentRunStatusCompleted && steerReadBeforeAnswer(f, run, message.ID) {
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
				kitDescribeMessages(f.messages(conversationID)), strings.Join(shapes, "\n  "))
		}
	}
}

// steerReadBeforeAnswer reports whether run logged messageID as a user entry
// before the assistant entry of its posted answer (its last one).
func steerReadBeforeAnswer(f *loopFixture, run *AgentRun, messageID string) bool {
	if run.Status != AgentRunStatusCompleted {
		return false
	}
	seen := false
	answered := false
	for _, entry := range f.entries(run.ID) {
		if entry.RunID != run.ID {
			continue
		}
		switch {
		case entry.Type == SessionEntryUser && entry.User != nil && entry.User.MessageID == messageID:
			seen = true
			answered = false
		case entry.Type == SessionEntryAssistant && seen:
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
	eachStore(t, "steer_visible", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		var conversation *Conversation
		var run *AgentRun
		f = newLoopFixture(t, store, withRules(
			&modelRule{name: "first", times: 1,
				before: func(context.Context, stepView) error {
					steerPostAs(f, conversation.ID, ConversationParticipant{Type: ConversationParticipantUser, ID: loopFixtureUser},
						"private note for bob", ConversationAudience{Kind: ConversationAudienceParticipants,
							Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "bob"}}}, nil)
					steerPostAs(f, conversation.ID, ConversationParticipant{Type: ConversationParticipantAgent, ID: loopFixtureAgent},
						"Working on it.", ConversationAudience{Kind: ConversationAudienceChannel},
						[]ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}})
					f.postUser(conversation.ID, loopFixtureUser, "visible follow-up")
					return nil
				},
				reply: func(v stepView) ModelStepResult {
					return act("Looking up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "6815"}))
				}},
			&modelRule{name: "answer",
				when:  func(v stepView) bool { return steerText(v.LastUserText()) == "visible follow-up" },
				reply: func(stepView) ModelStepResult { return say("Done.") }},
		))
		conversation, run = f.startChat("is 6815 open?")
		done := steerToRest(f, run.ID, 4)
		f.mustStatus(done, AgentRunStatusCompleted)

		var texts []string
		for _, entry := range steerUserEntries(f, run.ID) {
			texts = append(texts, steerText(kitPartsText(entry.User.Content)))
		}
		if strings.Join(texts, "|") != "is 6815 open?|visible follow-up" {
			t.Fatalf("user entries = %q, want the trigger and the visible follow-up", texts)
		}
		for _, req := range f.model.calls(ModelStepPurposeStep) {
			for _, entry := range req.Entries {
				rendered := ""
				switch {
				case entry.User != nil:
					rendered = kitPartsText(entry.User.Content)
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
func steerPostAs(f *loopFixture, conversationID string, sender ConversationParticipant, text string, audience ConversationAudience, refs []ConversationReference) {
	f.t.Helper()
	conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
	if err != nil {
		f.t.Fatal(err)
	}
	intent := MessageIntentQuestion
	if sender.Type == ConversationParticipantAgent {
		intent = MessageIntentUpdate
	}
	if _, err := f.convs.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: sender, Intent: intent, Content: text, Audience: audience, References: refs,
		IdempotencyKey: "steer-" + text,
	}); err != nil {
		f.t.Fatalf("post %q: %v", text, err)
	}
}

// TestRapidMessagesShareOneRunThatSeesAll: three messages from one person
// before the Run's first step make one Run. Its first step renders all three
// as user entries in order, and it posts one answer to the first.
func TestRapidMessagesShareOneRunThatSeesAll(t *testing.T) {
	eachStore(t, "steer_rapid", func(t *testing.T, store KernelStore) {
		first, second, third := "Find the cheapest USB-C hub", "It must have HDMI", "Use my card ending with 1001 to buy it"
		f := newLoopFixture(t, store, withRules(&modelRule{name: "answer",
			when: func(v stepView) bool {
				texts := steerUserTexts(v)
				return len(texts) >= 3 && slices.Equal(texts[len(texts)-3:], []string{first, second, third})
			},
			reply: func(stepView) ModelStepResult {
				return say("Bought the Anker hub with HDMI using the card ending 1001.")
			}}))
		conversation, run := f.startChat(first)
		for _, text := range []string{second, third} {
			message := f.postUser(conversation.ID, loopFixtureUser, text)
			if runs := steerConversationRuns(f, conversation.ID); len(runs) != 1 {
				t.Fatalf("message %s started a parallel Run (%d Runs)", message.ID, len(runs))
			}
		}
		queued := f.mustRun(run.ID)
		if followUps := conversationRunFollowUps(queued); queued.Status != AgentRunStatusQueued || len(followUps) != 2 {
			t.Fatalf("follow-ups were not delivered to the queued Run: %#v", queued.PendingInterventions)
		}
		reconciled, err := f.scheduler.ReconcileScope(f.ctx, f.scope)
		if err != nil || reconciled.Scheduled != 0 {
			t.Fatalf("reconciliation = %#v, %v", reconciled, err)
		}
		if replayed := f.mustRun(run.ID); len(replayed.PendingInterventions) != 2 {
			t.Fatalf("replay duplicated delivery: %d interventions", len(replayed.PendingInterventions))
		}

		// The scheduler's follow-up delivery stamps the Run on the wall clock.
		f.clock.Advance(time.Second)
		done := steerToRest(f, run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if steps := f.model.calls(ModelStepPurposeStep); len(steps) != 1 {
			t.Fatalf("model steps = %d, want 1", len(steps))
		}
		if got, want := steerShape(f, run.ID), "user,user,user,assistant"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		if done.ConsumedInterventions != 2 {
			t.Fatalf("intervention cursor = %d, want 2", done.ConsumedInterventions)
		}
		messages := f.messages(conversation.ID)
		answers := steerAgentMessages(f, conversation.ID)
		if len(answers) != 1 || answers[0].ReplyToMessageID != messages[0].ID || !strings.Contains(answers[0].Content, "Anker") {
			t.Fatalf("channel = %s, want one answer replying to the first message", kitDescribeMessages(messages))
		}

		// A terminal Run never absorbs: the next message starts a new Run.
		fourth := f.postUser(conversation.ID, loopFixtureUser, "Now track the delivery")
		runs := steerConversationRuns(f, conversation.ID)
		if len(runs) != 2 {
			t.Fatalf("conversation Runs = %d, want 2", len(runs))
		}
		for _, next := range runs {
			if next.ID != run.ID && next.Context[conversationRunContextTriggerID] != fourth.ID {
				t.Fatalf("the new Run answers %v, want %s", next.Context[conversationRunContextTriggerID], fourth.ID)
			}
		}
		if current := f.mustRun(run.ID); len(current.PendingInterventions) != 2 {
			t.Fatalf("terminal Run changed: %d interventions", len(current.PendingInterventions))
		}
	})
}
