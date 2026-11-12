package runtime

// Conversation journeys (run-loop section 12 §5.9): Team member sessions,
// catch-up with attribution, the seed of a new session, the run_context note
// that carries the ephemeral chat facts, and the react and participation
// tools. The Runs are driven by the real runloop.Loop through the loop fixture,
// whose chat bindings carry the production conversation seams (resolved
// through conversationrun.TurnRunner). These replace the Goal-builder tests of
// conversation_card_state_test.go, conversation_places_test.go,
// conversation_image_style_test.go and the thread, Telegram, participation
// and spoken-mode coverage of the old conversation runner.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// convRunner replaces the fixture's conversation runner with one built from
// config (Places, ImageStyle, HumanParticipants, participation opt-in), on
// the fixture's clock. It resolves the Agent's own binding through the
// fixture, as the fixture's runner does.
func convRunner(f *looptest.Fixture, config conversationrun.TurnRunnerConfig) {
	f.T.Helper()
	if config.AttachmentContent == nil {
		config.AttachmentContent = &looptest.AnswerVisualContent{}
	}
	config.AgentTurns = runtimeconversation.TurnRunnerResolverFunc(f.AgentBinding)
	runner, err := conversationrun.NewTurnRunner(f.Chats, f.Coordinator(), config, models.WithClock(f.Clock.Now))
	if err != nil {
		f.T.Fatal(err)
	}
	f.Runner = runner
}

// convAnswer answers every chat step with a final answer whose text names the
// newest user message.
func convAnswer(prefix string) *looptest.ModelRule {
	return &looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
		return looptest.FinalAnswer(map[string]any{"answer": prefix + steerText(v.LastUserText())})
	}}
}

// convAsk posts text as ana in conversation, schedules it and drives the new
// Run to rest; it returns the Run.
func convAsk(f *looptest.Fixture, conversationID, text string) *models.AgentRun {
	f.T.Helper()
	message := f.Post(conversationID, looptest.FixtureUser, text)
	conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
	if err != nil {
		f.T.Fatal(err)
	}
	run := f.ChatRun(conversation, message)
	return convDrive(f, run.ID)
}

// convDrive drives runID to rest and requires it completed.
func convDrive(f *looptest.Fixture, runID string) *models.AgentRun {
	f.T.Helper()
	run := steerToRest(f, runID, 6)
	if run.Status != models.AgentRunStatusCompleted {
		f.T.Fatalf("run %s = %s (%#v), log %s", runID, run.Status, run.FailureExplanation, steerShape(f, runID))
	}
	return run
}

// convFirstStep is the first model step request of runID.
func convFirstStep(f *looptest.Fixture, runID string) models.ModelStepRequest {
	f.T.Helper()
	for _, req := range f.Model.Calls(models.ModelStepPurposeStep) {
		if req.RunID == runID {
			return req
		}
	}
	f.T.Fatalf("run %s made no model step", runID)
	return models.ModelStepRequest{}
}

// convRunContext is the run_context note a request renders for its own Run
// (the newest one).
func convRunContext(f *looptest.Fixture, req models.ModelStepRequest) string {
	f.T.Helper()
	for i := len(req.Entries) - 1; i >= 0; i-- {
		e := req.Entries[i]
		if e.Type == models.SessionEntryNote && e.Note != nil && e.Note.Kind == string(models.NoteRunContext) {
			return e.Note.Text
		}
	}
	f.T.Fatalf("request of %s renders no run_context note: %s", req.RunID, looptest.StepView{Req: req}.Describe())
	return ""
}

// convRunContextNotes is the number of run_context notes runID wrote.
func convRunContextNotes(f *looptest.Fixture, runID string) int {
	f.T.Helper()
	n := 0
	for _, entry := range f.Entries(runID) {
		if entry.RunID == runID && entry.Type == models.SessionEntryNote && entry.Note != nil && entry.Note.Kind == models.NoteRunContext {
			n++
		}
	}
	return n
}

// convSection is the block of a run_context text that starts with header,
// up to the next blank line ("" when absent).
func convSection(text, header string) string {
	index := strings.Index(text, header)
	if index < 0 {
		return ""
	}
	section := strings.TrimSuffix(text[index:], "\n</run_context>")
	if end := strings.Index(section, "\n\n"); end >= 0 {
		section = section[:end]
	}
	return section
}

func convSystem(f *looptest.Fixture, req models.ModelStepRequest) string {
	f.T.Helper()
	raw, err := json.Marshal(req.System)
	if err != nil {
		f.T.Fatal(err)
	}
	return string(raw)
}

// convArtifactOf is the artifact reference of a posted card.
func convArtifactOf(t *testing.T, message *models.ChannelMessage) string {
	t.Helper()
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceArtifact {
			return reference.ID
		}
	}
	t.Fatalf("message %s has no artifact: %#v", message.ID, message.References)
	return ""
}

// convAgentAnswer is the newest agent message of the conversation.
func convAgentAnswer(f *looptest.Fixture, conversationID string) *models.ChannelMessage {
	f.T.Helper()
	answers := steerAgentMessages(f, conversationID)
	if len(answers) == 0 {
		f.T.Fatalf("conversation %s has no agent message: %s", conversationID, looptest.DescribeMessages(f.Messages(conversationID)))
	}
	return answers[len(answers)-1]
}

func convRecord(f *looptest.Fixture, conversationID string, card *models.ChannelMessage, request runtimeconversation.RecordSurfaceInteractionRequest) {
	f.T.Helper()
	artifactID := convArtifactOf(f.T, card)
	request.Scope, request.ConversationID, request.MessageID = f.Scope, conversationID, card.ID
	request.SurfaceArtifactID, request.SurfaceID, request.SurfaceVersion = artifactID, artifactID, 1
	if _, _, err := f.Convs.RecordSurfaceInteraction(f.Ctx, request); err != nil {
		f.T.Fatal(err)
	}
}

func convActor(id string) models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: id}
}

// ---------------------------------------------------------------------------
// run_context note

// TestRunStartNoteCarriesEphemeralContextNotTheSystemPrompt: the card state,
// the places, the saved picture style and the team roster reach the model as
// the Run's run_context note, written once at Run start, while the system
// prompt stays byte-identical across Runs with different facts (the prompt
// cache prefix). Ported from conversation_card_state_test.go,
// conversation_places_test.go and conversation_image_style_test.go.
func TestRunStartNoteCarriesEphemeralContextNotTheSystemPrompt(t *testing.T) {
	t.Run("card_state", func(t *testing.T) {
		looptest.EachStore(t, "conv-cards", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store)
			convRunner(f, conversationrun.TurnRunnerConfig{})
			first := true
			f.Model.Add(&looptest.ModelRule{Name: "card", Reply: func(v looptest.StepView) models.ModelStepResult {
				if first {
					first = false
					return looptest.FinalAnswer(map[string]any{"answer": "Here is your list.", "visual": scenarioChecklistVisual()})
				}
				return looptest.FinalAnswer(map[string]any{"answer": "Noted."})
			}})
			conversation, run := f.StartChat("Packing list please")
			convDrive(f, run.ID)
			card := convAgentAnswer(f, conversation.ID)
			if text := convRunContext(f, convFirstStep(f, run.ID)); strings.Contains(text, "What people did on cards") {
				t.Fatalf("first run sees card state: %q", text)
			}

			record := func(actor, component, item, values, key string, repeatable bool) {
				convRecord(f, conversation.ID, card, runtimeconversation.RecordSurfaceInteractionRequest{
					ComponentID: component, ItemKey: item, Actor: convActor(actor), Repeatable: repeatable,
					Values: json.RawMessage(values), IdempotencyKey: key,
				})
			}
			for index := 0; index < 7; index++ {
				record(looptest.FixtureUser, fmt.Sprintf("k%d", index), "a", `{"checked":true}`, fmt.Sprintf("tick-%d", index), true)
			}
			record("partner", "k0", "b", `{"checked":true}`, "tick-b", true)
			record(looptest.FixtureUser, "k0", "b", `{"checked":false}`, "untick-b", true)
			record(looptest.FixtureUser, "q", "", `{"selected":["cbr"]}`, "quiz", false)
			f.Clock.Advance(time.Second)

			second := convAsk(f, conversation.ID, "Anything missing?")
			want := []string{"- Packing list: 1/3 ticked (unticked: Charger, Towel)"}
			for index := 1; index < 6; index++ {
				want = append(want, fmt.Sprintf("- List %d: 1/3 ticked (unticked: Charger, Towel)", index))
			}
			secondRequest := convFirstStep(f, second.ID)
			section := convSection(convRunContext(f, secondRequest), "What people did on cards")
			if got := strings.Split(section, "\n")[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("card state = %q, want %q", got, want)
			}
			if n := convRunContextNotes(f, second.ID); n != 1 {
				t.Fatalf("run %s wrote %d run_context notes, want 1", second.ID, n)
			}

			third := convAsk(f, conversation.ID, "Thanks")
			thirdRequest := convFirstStep(f, third.ID)
			if text := convRunContext(f, thirdRequest); strings.Contains(text, "What people did on cards") {
				t.Fatalf("unchanged cards reported again: %q", text)
			}
			// The facts differ, the system prompt does not.
			if a, b, c := convSystem(f, convFirstStep(f, run.ID)), convSystem(f, secondRequest), convSystem(f, thirdRequest); a != b || b != c {
				t.Fatalf("system prompt changed with the card state:\n%s\n%s\n%s", a, b, c)
			}
			if strings.Contains(convSystem(f, secondRequest), "ticked") {
				t.Fatal("card state leaked into the system prompt")
			}
		})
	})

	t.Run("flashcards_and_recipe", func(t *testing.T) {
		f := looptest.NewFixture(t, kerneltest.NewStore(t))
		convRunner(f, conversationrun.TurnRunnerConfig{})
		first := true
		f.Model.Add(&looptest.ModelRule{Name: "card", Reply: func(v looptest.StepView) models.ModelStepResult {
			if first {
				first = false
				return looptest.FinalAnswer(map[string]any{"answer": "Learn and cook.", "visual": scenarioFlashcardRecipeVisual()})
			}
			return looptest.FinalAnswer(map[string]any{"answer": "Noted."})
		}})
		conversation, run := f.StartChat("Teach me and feed me")
		convDrive(f, run.ID)
		card := convAgentAnswer(f, conversation.ID)
		record := func(component, action, item, values, key string) {
			convRecord(f, conversation.ID, card, runtimeconversation.RecordSurfaceInteractionRequest{
				ComponentID: component, ActionID: action, ItemKey: item, Actor: convActor(looptest.FixtureUser), Repeatable: true,
				Values: json.RawMessage(values), IdempotencyKey: key,
			})
		}
		record("deck", "mark", "pain", `{"result":"again"}`, "mark-1")
		record("deck", "mark", "pain", `{"result":"got"}`, "mark-2")
		record("deck", "mark", "eau", `{"result":"again"}`, "mark-3")
		record("pancakes", "", "flour", `{"checked":true}`, "tick-1")
		f.Clock.Advance(time.Second)

		second := convAsk(f, conversation.ID, "How am I doing?")
		section := convSection(convRunContext(f, convFirstStep(f, second.ID)), "What people did on cards")
		want := []string{`- Flashcards "French food words": 1 got, 1 again, 1 unseen`, `- Recipe "Pancakes": 1/2 ingredients ticked`}
		if got := strings.Split(section, "\n")[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("card state = %q, want %q", got, want)
		}
		third := convAsk(f, conversation.ID, "Thanks")
		if text := convRunContext(f, convFirstStep(f, third.ID)); strings.Contains(text, "What people did on cards") {
			t.Fatalf("unchanged cards reported again: %q", text)
		}
	})

	t.Run("poll", func(t *testing.T) {
		f := looptest.NewFixture(t, kerneltest.NewStore(t))
		people := []models.ConversationParticipant{convActor("ana"), convActor("ben"), convActor("cy")}
		convRunner(f, conversationrun.TurnRunnerConfig{
			HumanParticipants: func(_ context.Context, _ *models.Conversation) ([]models.ConversationParticipant, error) {
				return people, nil
			},
		})
		poll := presentation.Component{ID: "vote", Type: "poll", Title: "Dinner", Summary: "Where to eat.", Poll: &presentation.Poll{
			Question: "Where shall we eat?", Options: []presentation.PollOption{{ID: "thai", Label: "Thai"}, {ID: "pizza", Label: "Pizza"}, {ID: "pub", Label: "Pub"}},
		}}
		one := 1.0
		explorer := presentation.Component{ID: "calc", Type: "explorer", Summary: "A calculator.", Explorer: &presentation.Explorer{
			Inputs:  []presentation.ExplorerInput{{ID: "x", Label: "X", Kind: "toggle", Default: &one}},
			Outputs: []presentation.ExplorerOutput{{ID: "y", Label: "Y", Expr: "if(x, 1, 2)"}},
		}}
		first := true
		f.Model.Add(&looptest.ModelRule{Name: "poll", Reply: func(v looptest.StepView) models.ModelStepResult {
			if first {
				first = false
				return looptest.FinalAnswer(map[string]any{"answer": "Vote here.", "visual": &presentation.Visual{Title: "Dinner", Components: []presentation.Component{poll, explorer}}})
			}
			return looptest.FinalAnswer(map[string]any{"answer": "Noted."})
		}})
		conversation, run := f.StartChat("Ask everyone where to eat")
		convDrive(f, run.ID)
		card := convAgentAnswer(f, conversation.ID)
		record := func(actor, component, action, values, key string) {
			convRecord(f, conversation.ID, card, runtimeconversation.RecordSurfaceInteractionRequest{
				ComponentID: component, ActionID: action, Actor: convActor(actor), Repeatable: true,
				Values: json.RawMessage(values), IdempotencyKey: key,
			})
		}
		record("ana", "vote", presentation.PollVoteAction, `{"optionIds":["pizza"]}`, "vote-1")
		record("ben", "vote", presentation.PollVoteAction, `{"optionIds":["thai"]}`, "vote-2")
		record("ana", "vote", presentation.PollVoteAction, `{"optionIds":["thai"]}`, "vote-3")
		record("ana", "calc", presentation.ExplorerShareAction, `{"inputs":{"x":1}}`, "share-1")
		f.Clock.Advance(time.Second)

		second := convAsk(f, conversation.ID, "Who wants what?")
		section := convSection(convRunContext(f, convFirstStep(f, second.ID)), "What people did on cards")
		if got := strings.Split(section, "\n")[1:]; strings.Join(got, "\n") != `- Poll "Dinner": Thai 2, Pizza 0, Pub 0 (2 of 3 voted)` {
			t.Fatalf("card state = %q", got)
		}
		third := convAsk(f, conversation.ID, "Thanks")
		if text := convRunContext(f, convFirstStep(f, third.ID)); strings.Contains(text, "What people did on cards") {
			t.Fatalf("unchanged poll reported again: %q", text)
		}
	})

	t.Run("places", func(t *testing.T) {
		f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(convAnswer("ok: ")))
		found := map[string][]runtimeconversation.Place{}
		convRunner(f, conversationrun.TurnRunnerConfig{Places: func(_ context.Context, run *models.AgentRun) []runtimeconversation.Place {
			conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
			return found[conversationID]
		}})
		// A chat that already found places lists them, capped.
		conversation, run := f.StartChat("Driving directions please")
		for i := 0; i < runtimeconversation.MaximumConversationPlaces+5; i++ {
			found[conversation.ID] = append(found[conversation.ID], runtimeconversation.Place{Place: fmt.Sprintf("place:%012x", i), Name: fmt.Sprintf("Stop %d", i), Lat: 37.4, Lon: -121.9})
		}
		convDrive(f, run.ID)
		request := convFirstStep(f, run.ID)
		section := convSection(convRunContext(f, request), "Places already found in this chat")
		lines := strings.Split(section, "\n")
		if len(lines) != runtimeconversation.MaximumConversationPlaces+1 || lines[1] != "- Stop 0 (place place:000000000000, 37.4,-121.9)" {
			t.Fatalf("places section = %q", section)
		}
		// A chat without places has no places section, and the same prompt.
		f.Clock.Advance(time.Second) // a new chat
		_, otherRun := f.StartChat("Hello")
		convDrive(f, otherRun.ID)
		otherRequest := convFirstStep(f, otherRun.ID)
		if text := convRunContext(f, otherRequest); strings.Contains(text, "Places already found") {
			t.Fatalf("a chat without places: %q", text)
		}
		if convSystem(f, request) != convSystem(f, otherRequest) {
			t.Fatal("places changed the system prompt")
		}
		// Places that go away are reported as gone once.
		found[conversation.ID] = nil
		gone := convAsk(f, conversation.ID, "Forget them")
		if text := convRunContext(f, convFirstStep(f, gone.ID)); !strings.Contains(text, "Places already found in this chat: none.") {
			t.Fatalf("emptied places not reported: %q", text)
		}
	})

	t.Run("image_style", func(t *testing.T) {
		f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(convAnswer("ok: ")))
		// The host reads the style saved by the sender of the Run's trigger.
		styles := map[string]string{"Draw a fox": " cartoon "}
		convRunner(f, conversationrun.TurnRunnerConfig{ImageStyle: func(_ context.Context, run *models.AgentRun) string {
			conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
			triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
			message, err := f.Convs.GetChannelMessage(f.Ctx, f.Scope, conversationID, triggerID)
			if err != nil {
				t.Fatal(err)
			}
			return styles[message.Content]
		}})
		_, fox := f.StartChat("Draw a fox")
		convDrive(f, fox.ID)
		foxRequest := convFirstStep(f, fox.ID)
		if text := convRunContext(f, foxRequest); !strings.Contains(text, "Preferred picture style: cartoon. generate_image applies it automatically; pass style only to override it for one image when asked.") {
			t.Fatalf("style not in the run_context note: %q", text)
		}
		f.Clock.Advance(time.Second) // a new chat
		_, owl := f.StartChat("Draw an owl")
		convDrive(f, owl.ID)
		owlRequest := convFirstStep(f, owl.ID)
		if text := convRunContext(f, owlRequest); strings.Contains(text, "picture style") {
			t.Fatalf("unsaved style: %q", text)
		}
		if convSystem(f, foxRequest) != convSystem(f, owlRequest) || strings.Contains(convSystem(f, foxRequest), "cartoon") {
			t.Fatal("the saved style changed the system prompt")
		}
	})
}

// ---------------------------------------------------------------------------
// Posting and reading

// convChat creates a chat owned by owner (finn when empty) without posting.
func convChat(f *looptest.Fixture, owner models.ObjectiveOwner, origin *models.ConversationReference) *models.Conversation {
	f.T.Helper()
	if owner.ID == "" {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: looptest.FixtureAgent}
	}
	f.Clock.Advance(time.Millisecond)
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: owner, Title: "Journey chat", Origin: origin,
		IdempotencyKey: "conv-chat-" + fmt.Sprint(f.Clock.Now().UnixNano()),
	})
	if err != nil {
		f.T.Fatalf("create conversation: %v", err)
	}
	return conversation
}

// convPost posts a message; edit fills in what the default user question
// (ana, channel audience) does not say.
func convPost(f *looptest.Fixture, conversationID, text string, edit func(*runtimeconversation.PostChannelMessageRequest)) *models.ChannelMessage {
	f.T.Helper()
	conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
	if err != nil {
		f.T.Fatal(err)
	}
	request := runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: convActor(looptest.FixtureUser), Intent: models.MessageIntentQuestion, Content: text,
		Audience:       models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		IdempotencyKey: fmt.Sprintf("conv-post-%d", conversation.Revision),
	}
	if edit != nil {
		edit(&request)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, request)
	if err != nil {
		f.T.Fatalf("post %q: %v", text, err)
	}
	f.Clock.Advance(time.Millisecond)
	return posted.Message
}

// convSchedule schedules message through the production scheduler and
// returns the Run that took it.
func convSchedule(f *looptest.Fixture, conversationID string, message *models.ChannelMessage) *models.AgentRun {
	f.T.Helper()
	result, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversationID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		f.T.Fatalf("schedule %s: %#v, %v", message.ID, result, err)
	}
	return result.Run
}

// convRunEntries is the entries runID wrote, in log order.
func convRunEntries(f *looptest.Fixture, runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

// convAuthor is a stored user entry's author as "<type>:<id>".
func convAuthor(entry *models.SessionEntry) string {
	if entry == nil || entry.User == nil {
		return ""
	}
	return entry.User.Author.Type + ":" + entry.User.Author.ID
}

// convUserBeforeAssistant is the user entries runID appended before its first
// assistant entry.
func convUserBeforeAssistant(f *looptest.Fixture, runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range convRunEntries(f, runID) {
		if entry.Type == models.SessionEntryAssistant {
			break
		}
		if entry.Type == models.SessionEntryUser && entry.User != nil {
			out = append(out, entry)
		}
	}
	return out
}

func convMessageIDs(entries []*models.SessionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.User.MessageID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Team

// ---------------------------------------------------------------------------
// Catch-up and seeding

// TestCatchUpAppendsNewVisibleMessagesWithAttribution: a chat session that
// already answered once catches up only what it can see since its watermark,
// each message a user entry attributed to its sender; its own Runs' posts and
// a private message to someone else are skipped, and the watermark moves.
func TestCatchUpAppendsNewVisibleMessagesWithAttribution(t *testing.T) {
	looptest.EachStore(t, "conv-catch-up", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(convAnswer("ok: ")))
		conversation, first := f.StartChat("Start the release checklist")
		first = convDrive(f, first.ID)
		watermark := f.Session(first.ID).ChannelWatermark
		if watermark == 0 {
			t.Fatal("first Run left no watermark")
		}

		ana := convPost(f, conversation.ID, "ana: the build is green", nil)
		ben := convPost(f, conversation.ID, "ben: staging looks fine", func(r *runtimeconversation.PostChannelMessageRequest) { r.Sender = convActor("ben") })
		routine := convPost(f, conversation.ID, "Nightly report: 0 failures", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Sender = models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "routine-nightly"}
			r.Intent = models.MessageIntentUpdate
		})
		scout := convPost(f, conversation.ID, "scout: I checked the canary", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Sender = models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "scout"}
			r.Intent = models.MessageIntentAnswer
		})
		// finn's own Run posted this (a progress note of the first Run): the
		// session already holds it.
		own := convPost(f, conversation.ID, "finn: still on it", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Sender = models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: looptest.FixtureAgent}
			r.Intent = models.MessageIntentUpdate
			r.References = []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: first.ID}}
		})
		private := convPost(f, conversation.ID, "ben, between us", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Audience = models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{convActor("ben")}}
		})
		trigger := convPost(f, conversation.ID, "What is left?", nil)
		run := f.ChatRun(conversation, trigger)
		run = convDrive(f, run.ID)
		if run.SessionID != first.SessionID {
			t.Fatalf("second Run session %s, want the lane session %s", run.SessionID, first.SessionID)
		}

		caught := convUserBeforeAssistant(f, run.ID)
		want := []string{ana.ID, ben.ID, routine.ID, scout.ID, trigger.ID}
		if got := convMessageIDs(caught); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("caught up %v, want %v (own %s, private %s skipped)", got, want, own.ID, private.ID)
		}
		authors := make([]string, len(caught))
		for i, entry := range caught {
			authors[i] = convAuthor(entry)
		}
		if strings.Join(authors, ",") != "user:ana,user:ben,service:routine-nightly,agent:scout,user:ana" {
			t.Fatalf("authors = %v", authors)
		}
		for i, header := range []string{"(user ana)", "(user ben)", "(service routine-nightly)", "(agent scout)", "(user ana)"} {
			if text := looptest.PartsText(caught[i].User.Content); !strings.HasPrefix(text, "[") || !strings.Contains(strings.SplitN(text, "\n", 2)[0], header) {
				t.Fatalf("entry %d header = %q, want %s", i, text, header)
			}
		}
		if got := f.Session(run.ID).ChannelWatermark; got < trigger.Sequence || got <= watermark {
			t.Fatalf("watermark = %d, want at least the trigger's %d (was %d)", got, trigger.Sequence, watermark)
		}
		// The second Run's step renders the first exchange, then the caught-up
		// messages: one session, one log.
		view := looptest.StepView{Req: convFirstStep(f, run.ID)}
		if texts := view.UserTexts(); len(texts) < 6 || !strings.Contains(texts[0], "Start the release checklist") {
			t.Fatalf("second Run's step = %s", view.Describe())
		}
	})

	t.Run("thread_root", func(t *testing.T) {
		// A message in a local thread runs in the thread's lane and session:
		// the canonical trigger is the thread message, the session holds only
		// the thread, and the answer goes back into the thread.
		f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(convAnswer("ok: ")))
		conversation := convChat(f, models.ObjectiveOwner{}, nil)
		root := convPost(f, conversation.ID, "Call one started", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Sender = models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "calls"}
			r.Intent = models.MessageIntentSystem
		})
		outside := convPost(f, conversation.ID, "an unrelated question", nil)
		inThread := convPost(f, conversation.ID, "first call request", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.ReplyToMessageID = root.ID
			r.RequiresResponse = true
		})
		run := convSchedule(f, conversation.ID, inThread)
		lane := models.ConversationRunLane(conversation.ID, root.ID, looptest.FixtureAgent)
		if run.Context["threadRootMessageId"] != root.ID || run.ConcurrencyKey != lane {
			t.Fatalf("thread Run = key %q context %#v", run.ConcurrencyKey, run.Context)
		}
		run = convDriveNextMember(f)
		session := f.Session(run.ID)
		if session.ThreadRootID != root.ID || session.ID != models.ConversationSessionID(f.Scope, lane) {
			t.Fatalf("thread session = %#v", session)
		}
		ids := convMessageIDs(convUserBeforeAssistant(f, run.ID))
		if slices.Contains(ids, outside.ID) || !slices.Contains(ids, inThread.ID) {
			t.Fatalf("thread session caught up %v (outside %s, trigger %s)", ids, outside.ID, inThread.ID)
		}
		if text := convRunContext(f, convFirstStep(f, run.ID)); !strings.Contains(text, "Reply to message "+inThread.ID) {
			t.Fatalf("run_context does not name the thread trigger: %q", text)
		}
		answer := convAgentAnswer(f, conversation.ID)
		if answer.ReplyToMessageID != inThread.ID || answer.ThreadRootID != root.ID {
			t.Fatalf("answer = reply %s thread %s, want %s in %s", answer.ReplyToMessageID, answer.ThreadRootID, inThread.ID, root.ID)
		}
		// The unthreaded chat has its own lane: the outside message starts a
		// Run there, not in the thread's.
		ordinary := convSchedule(f, conversation.ID, outside)
		if ordinary.ConcurrencyKey != models.ConversationRunLane(conversation.ID, "", looptest.FixtureAgent) || ordinary.ID == run.ID {
			t.Fatalf("unthreaded Run = %#v", ordinary)
		}
	})
}

// TestNewSessionForExistingChatSeedsTheLast30Messages: the first Run of a
// chat that has history but no session starts its session from the newest
// 30 visible messages, behind a history_omitted note. Earlier answers of the
// same Agent, posted without a session, are user entries attributed to it
// ("you, elsewhere"), not assistant entries (spec 12 decision issue 6).
func TestNewSessionForExistingChatSeedsTheLast30Messages(t *testing.T) {
	looptest.EachStore(t, "conv-seed", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(convAnswer("ok: ")))
		conversation := convChat(f, models.ObjectiveOwner{}, nil)
		var messages []*models.ChannelMessage
		for i := 1; i <= 45; i++ {
			sender, intent := convActor(looptest.FixtureUser), models.MessageIntentQuestion
			if i%2 == 0 {
				sender, intent = models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: looptest.FixtureAgent}, models.MessageIntentAnswer
			}
			messages = append(messages, convPost(f, conversation.ID, fmt.Sprintf("message #%d", i), func(r *runtimeconversation.PostChannelMessageRequest) {
				r.Sender, r.Intent = sender, intent
			}))
		}
		run := convDrive(f, f.ChatRun(conversation, messages[44]).ID)

		entries := convRunEntries(f, run.ID)
		var users []*models.SessionEntry
		omitted := -1
		for i, entry := range entries {
			if entry.Type == models.SessionEntryAssistant {
				break
			}
			if entry.Type == models.SessionEntryNote && entry.Note != nil && entry.Note.Kind == models.NoteHistoryOmitted && omitted < 0 {
				omitted = i
			}
			if entry.Type == models.SessionEntryUser && entry.User != nil {
				users = append(users, entry)
			}
		}
		if len(users) != 30 {
			t.Fatalf("seeded %d user entries, want 30", len(users))
		}
		if users[0].User.MessageID != messages[15].ID || users[29].User.MessageID != messages[44].ID {
			t.Fatalf("seed = %s..%s, want #16 (%s)..#45", users[0].User.MessageID, users[29].User.MessageID, messages[15].ID)
		}
		if omitted < 0 || entries[omitted].Sequence > users[0].Sequence {
			t.Fatalf("history_omitted note missing or after the seed (at %d)", omitted)
		}
		for _, entry := range users {
			author := convAuthor(entry)
			header := strings.SplitN(looptest.PartsText(entry.User.Content), "\n", 2)[0]
			switch author {
			case "user:" + looptest.FixtureUser:
			case "agent:" + looptest.FixtureAgent:
				if !strings.Contains(header, "(you, elsewhere finn)") {
					t.Fatalf("earlier finn answer header = %q", header)
				}
			default:
				t.Fatalf("seeded entry author %q", author)
			}
		}
	})

	t.Run("telegram_dm", func(t *testing.T) {
		// Telegram private chats quote earlier messages with reply ids; a
		// reply root there is not a thread. The reply's Run works in the
		// chat's own lane, and its seed holds the whole chat, not only the
		// quoted message's "thread".
		f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(convAnswer("ok: ")))
		conversation := convChat(f, models.ObjectiveOwner{}, &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: "telegram-endpoint"})
		source := func(id, thread string) *models.ExternalMessageSource {
			return &models.ExternalMessageSource{Provider: "telegram", ChannelType: "private", ChannelID: "chat-7", ThreadID: thread, MessageID: id, ParticipantID: "kev"}
		}
		earlier := convPost(f, conversation.ID, "my flight is at 9", func(r *runtimeconversation.PostChannelMessageRequest) { r.ExternalSource = source("chat-7:1", "") })
		quoted := convPost(f, conversation.ID, "and the hotel is booked", func(r *runtimeconversation.PostChannelMessageRequest) { r.ExternalSource = source("chat-7:2", "") })
		reply := convPost(f, conversation.ID, "when should I leave?", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.ExternalSource = source("chat-7:3", "reply:2")
			r.ReplyToMessageID = quoted.ID
			r.RequiresResponse = true
		})
		if root := models.ExternalConversationThreadRoot(conversation, reply); root != "" {
			t.Fatalf("a Telegram DM reply root became a thread: %s", root)
		}
		run := convSchedule(f, conversation.ID, reply)
		if run.ConcurrencyKey != models.ConversationRunLane(conversation.ID, "", looptest.FixtureAgent) || run.Context["threadRootMessageId"] != nil {
			t.Fatalf("Telegram DM reply Run = key %q context %#v", run.ConcurrencyKey, run.Context)
		}
		run = steerToRest(f, run.ID, 6)
		ids := convMessageIDs(convUserBeforeAssistant(f, run.ID))
		if !slices.Contains(ids, earlier.ID) || !slices.Contains(ids, quoted.ID) || !slices.Contains(ids, reply.ID) {
			t.Fatalf("Telegram DM seed = %v, want the whole chat (%s %s %s)", ids, earlier.ID, quoted.ID, reply.ID)
		}
		if session := f.Session(run.ID); session.ThreadRootID != "" {
			t.Fatalf("Telegram DM session is a thread session: %#v", session)
		}
	})
}

// ---------------------------------------------------------------------------
// react, participation, spoken replies

// convParticipation turns the chat's participation opt-in on or off.
func convParticipation(f *looptest.Fixture, conversationID string, enabled bool) {
	f.T.Helper()
	conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
	if err != nil {
		f.T.Fatal(err)
	}
	if _, err := f.Convs.UpdateConversation(f.Ctx, runtimeconversation.UpdateConversationRequest{
		Scope: f.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision, ParticipationEnabled: &enabled,
	}); err != nil {
		f.T.Fatal(err)
	}
}

// convToolEntries is runID's tool entries named name.
func convToolEntries(f *looptest.Fixture, runID, name string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range convRunEntries(f, runID) {
		if entry.Type == models.SessionEntryTool && entry.Tool != nil && entry.Tool.Name == name {
			out = append(out, entry)
		}
	}
	return out
}

// TestReactAndParticipationAreTools: react is a tool the chat Run calls (the
// reaction lands on the message, the tool entry completes), and the channel's
// participation opt-in decides whether an answer is posted: outside it the
// Run ends at once, and an opt-out while the Run works discards the answer
// but keeps the usage it spent.
func TestReactAndParticipationAreTools(t *testing.T) {
	t.Run("react", func(t *testing.T) {
		looptest.EachStore(t, "conv-react", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store)
			var triggerID string
			f.Model.Add(
				&looptest.ModelRule{Name: "react", When: func(v looptest.StepView) bool { _, _, ok := v.ToolResult(string(models.ControlToolReact)); return !ok },
					Reply: func(v looptest.StepView) models.ModelStepResult {
						if !v.HasTool(string(models.ControlToolReact)) {
							t.Errorf("react is not offered: %s", v.Describe())
						}
						return looptest.Act("Picking it up.", looptest.Call(string(models.ControlToolReact), map[string]any{"messageId": triggerID, "emoji": "👀"}))
					}},
				&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "On it: the deploy is green."})
				}},
			)
			conversation := convChat(f, models.ObjectiveOwner{}, nil)
			trigger := convPost(f, conversation.ID, "Is the deploy green?", nil)
			triggerID = trigger.ID
			run := convDrive(f, f.ChatRun(conversation, trigger).ID)
			reacts := convToolEntries(f, run.ID, string(models.ControlToolReact))
			if len(reacts) != 1 || reacts[0].State != models.ToolEntryCompleted || reacts[0].Tool.Result.IsError {
				t.Fatalf("react entries = %#v", reacts)
			}
			reacted, err := f.Convs.GetChannelMessage(f.Ctx, f.Scope, conversation.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			agent := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: looptest.FixtureAgent}
			if len(reacted.Reactions) != 1 || reacted.Reactions[0].Emoji != "👀" || !slices.Contains(reacted.Reactions[0].Actors, agent) {
				t.Fatalf("reactions = %#v", reacted.Reactions)
			}
			if answer := convAgentAnswer(f, conversation.ID); answer.Content != "On it: the deploy is green." {
				t.Fatalf("answer = %#v", answer)
			}
		})
	})

	t.Run("participation_opt_in", func(t *testing.T) {
		f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(convAnswer("ok: ")))
		convRunner(f, conversationrun.TurnRunnerConfig{RequireParticipationOptIn: true})
		conversation := convChat(f, models.ObjectiveOwner{}, nil)

		// Not opted in: the Run ends at once, with no model call and no post.
		quiet := convPost(f, conversation.ID, "anyone there?", nil)
		skipped := convDrive(f, f.ChatRun(conversation, quiet).ID)
		if skipped.Output["participationSkipped"] != true || len(steerAgentMessages(f, conversation.ID)) != 0 {
			t.Fatalf("Run outside the opt-in = %#v, messages %s", skipped.Output, looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
		if len(f.Model.Calls("")) != 0 {
			t.Fatalf("a Run outside the opt-in called the model %d times", len(f.Model.Calls("")))
		}

		// Opted in: a later message is answered; the earlier one stays unanswered.
		convParticipation(f, conversation.ID, true)
		asked := convPost(f, conversation.ID, "now?", nil)
		answered := convDrive(f, f.ChatRun(conversation, asked).ID)
		if answered.Output["participationSkipped"] == true || convAgentAnswer(f, conversation.ID).ReplyToMessageID != asked.ID {
			t.Fatalf("opted-in Run = %#v, messages %s", answered.Output, looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
		current, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p := current.Participation; p == nil || !p.Enabled || quiet.Sequence > p.AfterSequence {
			t.Fatalf("participation = %#v, want the opt-in to start after message %d", p, quiet.Sequence)
		}
	})

	t.Run("participation_opt_out_discards_the_answer", func(t *testing.T) {
		looptest.EachStore(t, "conv-optout", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store)
			convRunner(f, conversationrun.TurnRunnerConfig{RequireParticipationOptIn: true})
			conversation := convChat(f, models.ObjectiveOwner{}, nil)
			convParticipation(f, conversation.ID, true)
			f.Dispatch.On("list_items", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"items": []interface{}{"a", "b"}}, nil
			})
			f.Model.Add(
				&looptest.ModelRule{Name: "read", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.WithUsage(looptest.Act("Reading the items.", looptest.Call(looptest.GovernedTool(v, "list_items"), nil)), 1200, 40)
				}},
				&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.WithUsage(looptest.FinalAnswer(map[string]any{"answer": "There are two items."}), 1300, 30)
				}, Before: func(_ context.Context, _ looptest.StepView) error {
					// The channel opts out while the Run works.
					convParticipation(f, conversation.ID, false)
					return nil
				}},
			)
			trigger := convPost(f, conversation.ID, "How many items?", nil)
			run := convDrive(f, f.ChatRun(conversation, trigger).ID)
			if run.Output["participationSkipped"] != true {
				t.Fatalf("opted-out Run output = %#v", run.Output)
			}
			if messages := steerAgentMessages(f, conversation.ID); len(messages) != 0 {
				t.Fatalf("the discarded answer was posted: %s", looptest.DescribeMessages(messages))
			}
			var in, out int64
			for _, step := range f.Steps(run.ID) {
				in += int64(step.Usage.InputTokens)
				out += int64(step.Usage.OutputTokens)
			}
			if in != 2500 || out != 70 {
				t.Fatalf("step usage %d/%d; want the spent 2500/70 kept", in, out)
			}
		})
	})
}

// TestSpokenTriggerKeepsItsResponseMode: a spoken message's Run carries the
// response mode from its trigger: the run_context names a spoken reply, the
// spoken system section is offered, and the answer is posted spoken.
func TestSpokenTriggerKeepsItsResponseMode(t *testing.T) {
	f := looptest.NewFixture(t, kerneltest.NewStore(t), looptest.WithRules(&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
		args := map[string]any{"answer": "Leave at seven."}
		// The spoken final tool requires silent.
		for _, tool := range v.Request().Tools {
			if raw, _ := json.Marshal(tool); tool.Name == string(models.ControlToolFinalAnswer) && strings.Contains(string(raw), `"silent"`) {
				args["silent"] = false
			}
		}
		return looptest.FinalAnswer(args)
	}}))
	conversation := convChat(f, models.ObjectiveOwner{}, nil)
	spoken := convPost(f, conversation.ID, "when should I leave?", func(r *runtimeconversation.PostChannelMessageRequest) {
		r.ResponseMode = "spoken"
		r.RequiresResponse = true
	})
	run := convSchedule(f, conversation.ID, spoken)
	if run.Context["responseMode"] != "spoken" {
		t.Fatalf("spoken Run context = %#v", run.Context)
	}
	run = convDrive(f, run.ID)
	request := convFirstStep(f, run.ID)
	if text := convRunContext(f, request); !strings.Contains(text, "Spoken reply: answer in words to be spoken.") {
		t.Fatalf("spoken run_context = %q", text)
	}
	answer := convAgentAnswer(f, conversation.ID)
	if answer.ResponseMode != "spoken" || answer.ReplyToMessageID != spoken.ID {
		t.Fatalf("spoken answer = %#v", answer)
	}

	// A written message in the same chat gets a written reply and the same
	// lane session, without the spoken section.
	written := convPost(f, conversation.ID, "and the hotel?", func(r *runtimeconversation.PostChannelMessageRequest) { r.RequiresResponse = true })
	next := convDrive(f, convSchedule(f, conversation.ID, written).ID)
	if next.SessionID != run.SessionID || next.Context["responseMode"] != nil {
		t.Fatalf("written Run = session %s context %#v", next.SessionID, next.Context)
	}
	if text := convRunContext(f, convFirstStep(f, next.ID)); strings.Contains(text, "Spoken reply") {
		t.Fatalf("written run_context = %q", text)
	}
	if reply := convAgentAnswer(f, conversation.ID); reply.ResponseMode != "" {
		t.Fatalf("written answer = %#v", reply)
	}
	if spokenSystem, writtenSystem := convSystem(f, request), convSystem(f, convFirstStep(f, next.ID)); spokenSystem == writtenSystem {
		t.Fatal("the spoken system section is missing from the spoken Run")
	}
}

// scenarioChecklistVisual is a copy of the conversation run tests' cardStateTestVisual; change them
// together.
func scenarioChecklistVisual() *presentation.Visual {
	visual := &presentation.Visual{Title: "Trip"}
	for index := 0; index < 7; index++ {
		title := fmt.Sprintf("List %d", index)
		if index == 0 {
			title = "Packing list"
		}
		visual.Components = append(visual.Components, presentation.Component{
			ID: fmt.Sprintf("k%d", index), Type: "checklist", Title: title, Summary: "Things to pack.",
			Checklist: &presentation.Checklist{Items: []presentation.ChecklistItem{{ID: "a", Label: "Passport"}, {ID: "b", Label: "Charger"}, {ID: "c", Label: "Towel"}}},
		})
	}
	visual.Components = append(visual.Components, presentation.Component{ID: "q", Type: "quiz", Summary: "One question.", Quiz: &presentation.Quiz{
		Question: "Capital of Australia?", Options: []presentation.QuizOption{{ID: "syd", Label: "Sydney"}, {ID: "cbr", Label: "Canberra"}},
		AnswerKey: &presentation.QuizAnswerKey{Correct: []string{"cbr"}, Explanation: "Canberra."},
	}})
	return visual
}

// scenarioFlashcardRecipeVisual is a copy of the conversation run tests' cardStateWave2Visual; change them
// together.
func scenarioFlashcardRecipeVisual() *presentation.Visual {
	cards := []presentation.Flashcard{{ID: "pain", Front: "le pain", Back: "bread"}, {ID: "eau", Front: "l'eau", Back: "water"}, {ID: "pomme", Front: "la pomme", Back: "apple"}}
	qty := 2.0
	return &presentation.Visual{Title: "Learn and cook", Components: []presentation.Component{
		{ID: "deck", Type: "flashcards", Title: "French food words", Summary: "Three words.", Flashcards: &presentation.Flashcards{Cards: cards}},
		{ID: "pancakes", Type: "recipe", Title: "Pancakes", Summary: "Pancakes for 2.", Recipe: &presentation.Recipe{
			Servings: 2, Checkable: true, Steps: []presentation.RecipeStep{{ID: "mix", Text: "Whisk into a batter and fry."}},
			Ingredients: []presentation.RecipeIngredient{{ID: "flour", Name: "flour"}, {ID: "eggs", Qty: &qty, Name: "eggs"}},
		}},
	}}
}
