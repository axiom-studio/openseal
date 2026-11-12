package runtime

// Conversation journeys (run-loop section 12 §5.9): Team member sessions,
// catch-up with attribution, the seed of a new session, the run_context note
// that carries the ephemeral chat facts, and the react and participation
// tools. The Runs are driven by the real RunLoop through the loop fixture,
// whose chat bindings carry the production conversation seams (resolved
// through ConversationRunTurnRunner). These replace the Goal-builder tests of
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

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// convRunner replaces the fixture's conversation runner with one built from
// config (Places, ImageStyle, HumanParticipants, participation opt-in), on
// the fixture's clock. It resolves the Agent's own binding through the
// fixture, as the fixture's runner does.
func convRunner(f *loopFixture, config ConversationRunTurnRunnerConfig) {
	f.t.Helper()
	if config.AttachmentContent == nil {
		config.AttachmentContent = &answerVisualContent{}
	}
	config.AgentTurns = TurnRunnerResolverFunc(f.agentBinding)
	runner, err := NewConversationRunTurnRunner(f.chats, f.coordinator(), config, models.WithClock(f.clock.Now))
	if err != nil {
		f.t.Fatal(err)
	}
	f.runner = runner
}

// convAnswer answers every chat step with a final answer whose text names the
// newest user message.
func convAnswer(prefix string) *modelRule {
	return &modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
		return finalAnswer(map[string]any{"answer": prefix + steerText(v.LastUserText())})
	}}
}

// convAsk posts text as ana in conversation, schedules it and drives the new
// Run to rest; it returns the Run.
func convAsk(f *loopFixture, conversationID, text string) *AgentRun {
	f.t.Helper()
	message := f.post(conversationID, loopFixtureUser, text)
	conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
	if err != nil {
		f.t.Fatal(err)
	}
	run := f.chatRun(conversation, message)
	return convDrive(f, run.ID)
}

// convDrive drives runID to rest and requires it completed.
func convDrive(f *loopFixture, runID string) *AgentRun {
	f.t.Helper()
	run := steerToRest(f, runID, 6)
	if run.Status != AgentRunStatusCompleted {
		f.t.Fatalf("run %s = %s (%#v), log %s", runID, run.Status, run.FailureExplanation, steerShape(f, runID))
	}
	return run
}

// convFirstStep is the first model step request of runID.
func convFirstStep(f *loopFixture, runID string) ModelStepRequest {
	f.t.Helper()
	for _, req := range f.model.calls(ModelStepPurposeStep) {
		if req.RunID == runID {
			return req
		}
	}
	f.t.Fatalf("run %s made no model step", runID)
	return ModelStepRequest{}
}

// convRunContext is the run_context note a request renders for its own Run
// (the newest one).
func convRunContext(f *loopFixture, req ModelStepRequest) string {
	f.t.Helper()
	for i := len(req.Entries) - 1; i >= 0; i-- {
		e := req.Entries[i]
		if e.Type == SessionEntryNote && e.Note != nil && e.Note.Kind == string(NoteRunContext) {
			return e.Note.Text
		}
	}
	f.t.Fatalf("request of %s renders no run_context note: %s", req.RunID, stepView{req: req}.describe())
	return ""
}

// convRunContextNotes is the number of run_context notes runID wrote.
func convRunContextNotes(f *loopFixture, runID string) int {
	f.t.Helper()
	n := 0
	for _, entry := range f.entries(runID) {
		if entry.RunID == runID && entry.Type == SessionEntryNote && entry.Note != nil && entry.Note.Kind == NoteRunContext {
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

func convSystem(f *loopFixture, req ModelStepRequest) string {
	f.t.Helper()
	raw, err := json.Marshal(req.System)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

// convArtifactOf is the artifact reference of a posted card.
func convArtifactOf(t *testing.T, message *ChannelMessage) string {
	t.Helper()
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceArtifact {
			return reference.ID
		}
	}
	t.Fatalf("message %s has no artifact: %#v", message.ID, message.References)
	return ""
}

// convAgentAnswer is the newest agent message of the conversation.
func convAgentAnswer(f *loopFixture, conversationID string) *ChannelMessage {
	f.t.Helper()
	answers := steerAgentMessages(f, conversationID)
	if len(answers) == 0 {
		f.t.Fatalf("conversation %s has no agent message: %s", conversationID, kitDescribeMessages(f.messages(conversationID)))
	}
	return answers[len(answers)-1]
}

func convRecord(f *loopFixture, conversationID string, card *ChannelMessage, request RecordSurfaceInteractionRequest) {
	f.t.Helper()
	artifactID := convArtifactOf(f.t, card)
	request.Scope, request.ConversationID, request.MessageID = f.scope, conversationID, card.ID
	request.SurfaceArtifactID, request.SurfaceID, request.SurfaceVersion = artifactID, artifactID, 1
	if _, _, err := f.convs.RecordSurfaceInteraction(f.ctx, request); err != nil {
		f.t.Fatal(err)
	}
}

func convActor(id string) ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantUser, ID: id}
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
		eachStore(t, "conv-cards", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store)
			convRunner(f, ConversationRunTurnRunnerConfig{})
			first := true
			f.model.add(&modelRule{name: "card", reply: func(v stepView) ModelStepResult {
				if first {
					first = false
					return finalAnswer(map[string]any{"answer": "Here is your list.", "visual": cardStateTestVisual()})
				}
				return finalAnswer(map[string]any{"answer": "Noted."})
			}})
			conversation, run := f.startChat("Packing list please")
			convDrive(f, run.ID)
			card := convAgentAnswer(f, conversation.ID)
			if text := convRunContext(f, convFirstStep(f, run.ID)); strings.Contains(text, "What people did on cards") {
				t.Fatalf("first run sees card state: %q", text)
			}

			record := func(actor, component, item, values, key string, repeatable bool) {
				convRecord(f, conversation.ID, card, RecordSurfaceInteractionRequest{
					ComponentID: component, ItemKey: item, Actor: convActor(actor), Repeatable: repeatable,
					Values: json.RawMessage(values), IdempotencyKey: key,
				})
			}
			for index := 0; index < 7; index++ {
				record(loopFixtureUser, fmt.Sprintf("k%d", index), "a", `{"checked":true}`, fmt.Sprintf("tick-%d", index), true)
			}
			record("partner", "k0", "b", `{"checked":true}`, "tick-b", true)
			record(loopFixtureUser, "k0", "b", `{"checked":false}`, "untick-b", true)
			record(loopFixtureUser, "q", "", `{"selected":["cbr"]}`, "quiz", false)
			f.clock.Advance(time.Second)

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
		f := newLoopFixture(t, newTestKernelStore(t))
		convRunner(f, ConversationRunTurnRunnerConfig{})
		first := true
		f.model.add(&modelRule{name: "card", reply: func(v stepView) ModelStepResult {
			if first {
				first = false
				return finalAnswer(map[string]any{"answer": "Learn and cook.", "visual": cardStateWave2Visual()})
			}
			return finalAnswer(map[string]any{"answer": "Noted."})
		}})
		conversation, run := f.startChat("Teach me and feed me")
		convDrive(f, run.ID)
		card := convAgentAnswer(f, conversation.ID)
		record := func(component, action, item, values, key string) {
			convRecord(f, conversation.ID, card, RecordSurfaceInteractionRequest{
				ComponentID: component, ActionID: action, ItemKey: item, Actor: convActor(loopFixtureUser), Repeatable: true,
				Values: json.RawMessage(values), IdempotencyKey: key,
			})
		}
		record("deck", "mark", "pain", `{"result":"again"}`, "mark-1")
		record("deck", "mark", "pain", `{"result":"got"}`, "mark-2")
		record("deck", "mark", "eau", `{"result":"again"}`, "mark-3")
		record("pancakes", "", "flour", `{"checked":true}`, "tick-1")
		f.clock.Advance(time.Second)

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
		f := newLoopFixture(t, newTestKernelStore(t))
		people := []ConversationParticipant{convActor("ana"), convActor("ben"), convActor("cy")}
		convRunner(f, ConversationRunTurnRunnerConfig{
			HumanParticipants: func(_ context.Context, _ *Conversation) ([]ConversationParticipant, error) { return people, nil },
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
		f.model.add(&modelRule{name: "poll", reply: func(v stepView) ModelStepResult {
			if first {
				first = false
				return finalAnswer(map[string]any{"answer": "Vote here.", "visual": &presentation.Visual{Title: "Dinner", Components: []presentation.Component{poll, explorer}}})
			}
			return finalAnswer(map[string]any{"answer": "Noted."})
		}})
		conversation, run := f.startChat("Ask everyone where to eat")
		convDrive(f, run.ID)
		card := convAgentAnswer(f, conversation.ID)
		record := func(actor, component, action, values, key string) {
			convRecord(f, conversation.ID, card, RecordSurfaceInteractionRequest{
				ComponentID: component, ActionID: action, Actor: convActor(actor), Repeatable: true,
				Values: json.RawMessage(values), IdempotencyKey: key,
			})
		}
		record("ana", "vote", presentation.PollVoteAction, `{"optionIds":["pizza"]}`, "vote-1")
		record("ben", "vote", presentation.PollVoteAction, `{"optionIds":["thai"]}`, "vote-2")
		record("ana", "vote", presentation.PollVoteAction, `{"optionIds":["thai"]}`, "vote-3")
		record("ana", "calc", presentation.ExplorerShareAction, `{"inputs":{"x":1}}`, "share-1")
		f.clock.Advance(time.Second)

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
		f := newLoopFixture(t, newTestKernelStore(t), withRules(convAnswer("ok: ")))
		found := map[string][]ConversationPlace{}
		convRunner(f, ConversationRunTurnRunnerConfig{Places: func(_ context.Context, run *AgentRun) []ConversationPlace {
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			return found[conversationID]
		}})
		// A chat that already found places lists them, capped.
		conversation, run := f.startChat("Driving directions please")
		for i := 0; i < MaximumConversationPlaces+5; i++ {
			found[conversation.ID] = append(found[conversation.ID], ConversationPlace{Place: fmt.Sprintf("place:%012x", i), Name: fmt.Sprintf("Stop %d", i), Lat: 37.4, Lon: -121.9})
		}
		convDrive(f, run.ID)
		request := convFirstStep(f, run.ID)
		section := convSection(convRunContext(f, request), "Places already found in this chat")
		lines := strings.Split(section, "\n")
		if len(lines) != MaximumConversationPlaces+1 || lines[1] != "- Stop 0 (place place:000000000000, 37.4,-121.9)" {
			t.Fatalf("places section = %q", section)
		}
		// A chat without places has no places section, and the same prompt.
		f.clock.Advance(time.Second) // a new chat
		_, otherRun := f.startChat("Hello")
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
		f := newLoopFixture(t, newTestKernelStore(t), withRules(convAnswer("ok: ")))
		// The host reads the style saved by the sender of the Run's trigger.
		styles := map[string]string{"Draw a fox": " cartoon "}
		convRunner(f, ConversationRunTurnRunnerConfig{ImageStyle: func(_ context.Context, run *AgentRun) string {
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			message, err := f.convs.GetChannelMessage(f.ctx, f.scope, conversationID, triggerID)
			if err != nil {
				t.Fatal(err)
			}
			return styles[message.Content]
		}})
		_, fox := f.startChat("Draw a fox")
		convDrive(f, fox.ID)
		foxRequest := convFirstStep(f, fox.ID)
		if text := convRunContext(f, foxRequest); !strings.Contains(text, "Preferred picture style: cartoon. generate_image applies it automatically; pass style only to override it for one image when asked.") {
			t.Fatalf("style not in the run_context note: %q", text)
		}
		f.clock.Advance(time.Second) // a new chat
		_, owl := f.startChat("Draw an owl")
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
func convChat(f *loopFixture, owner ObjectiveOwner, origin *ConversationReference) *Conversation {
	f.t.Helper()
	if owner.ID == "" {
		owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent}
	}
	f.clock.Advance(time.Millisecond)
	conversation, _, err := f.convs.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.scope, Owner: owner, Title: "Journey chat", Origin: origin,
		IdempotencyKey: "conv-chat-" + fmt.Sprint(f.clock.Now().UnixNano()),
	})
	if err != nil {
		f.t.Fatalf("create conversation: %v", err)
	}
	return conversation
}

// convPost posts a message; edit fills in what the default user question
// (ana, channel audience) does not say.
func convPost(f *loopFixture, conversationID, text string, edit func(*PostChannelMessageRequest)) *ChannelMessage {
	f.t.Helper()
	conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
	if err != nil {
		f.t.Fatal(err)
	}
	request := PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: convActor(loopFixtureUser), Intent: MessageIntentQuestion, Content: text,
		Audience:       ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: fmt.Sprintf("conv-post-%d", conversation.Revision),
	}
	if edit != nil {
		edit(&request)
	}
	posted, err := f.convs.PostChannelMessage(f.ctx, request)
	if err != nil {
		f.t.Fatalf("post %q: %v", text, err)
	}
	f.clock.Advance(time.Millisecond)
	return posted.Message
}

// convSchedule schedules message through the production scheduler and
// returns the Run that took it.
func convSchedule(f *loopFixture, conversationID string, message *ChannelMessage) *AgentRun {
	f.t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversationID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		f.t.Fatalf("schedule %s: %#v, %v", message.ID, result, err)
	}
	return result.Run
}

// convRunEntries is the entries runID wrote, in log order.
func convRunEntries(f *loopFixture, runID string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range f.entries(runID) {
		if entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

// convAuthor is a stored user entry's author as "<type>:<id>".
func convAuthor(entry *SessionEntry) string {
	if entry == nil || entry.User == nil {
		return ""
	}
	return entry.User.Author.Type + ":" + entry.User.Author.ID
}

// convUserBeforeAssistant is the user entries runID appended before its first
// assistant entry.
func convUserBeforeAssistant(f *loopFixture, runID string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range convRunEntries(f, runID) {
		if entry.Type == SessionEntryAssistant {
			break
		}
		if entry.Type == SessionEntryUser && entry.User != nil {
			out = append(out, entry)
		}
	}
	return out
}

func convMessageIDs(entries []*SessionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.User.MessageID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Team

// convTeamCatalog is the Agent catalog seam a Team member's control authority
// reads its delegation roster from: an active Team with no other eligible
// member, so the member gets no delegate targets.
type convTeamCatalog struct{ AgentTurnCatalog }

func (convTeamCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return &kernelteam.Deployment{Status: kernelteam.DeploymentActive}, nil
}

func (convTeamCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return nil, nil
}

// convMemberBinding is the fixture's chat binding for the Runs the fixture
// cannot bind: a Team member Run, whose control authority is resolved through
// the Team catalog seam (the fixture resolves it without a catalog, which a
// Team-owned Run cannot do), and a thread Run, whose lane session is opened
// with its thread root (the fixture opens the chat's unthreaded lane). The
// conversation turn runner adds the conversation loop seams on top of
// convMemberAgentBinding, as it does for the fixture's own binding.
func convMemberBinding(f *loopFixture, run *AgentRun) *TurnRunnerBinding {
	f.t.Helper()
	runner, err := NewConversationRunTurnRunner(f.chats, f.coordinator(), ConversationRunTurnRunnerConfig{
		AttachmentContent: &answerVisualContent{}, AgentTurns: TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
			return convMemberAgentBinding(f, run), nil
		}),
	}, models.WithClock(f.clock.Now))
	if err != nil {
		f.t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(f.ctx, run)
	if err != nil {
		f.t.Fatalf("resolve member conversation turn runner: %v", err)
	}
	return binding
}

// convMemberAgentBinding is the member or thread Run's own Agent binding:
// its lane session, its control authority, the model producer, the run-start
// preparer and the claim tool executor.
func convMemberAgentBinding(f *loopFixture, run *AgentRun) *TurnRunnerBinding {
	f.t.Helper()
	sessions := f.store.(AgentSessionStore)
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	root, _ := run.Context["threadRootMessageId"].(string)
	opened, err := sessions.OpenConversationAgentSession(f.ctx, OpenConversationAgentSessionRequest{
		Scope: f.scope, ConversationID: conversationID, ThreadRootID: root, AgentID: run.AssignedAgentID, RunID: run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: f.workerID, Now: f.clock.Now()}, Now: f.clock.Now(),
	})
	if err != nil {
		f.t.Fatalf("open conversation session: %v", err)
	}
	session := opened.Session
	control, err := ResolveControlAuthority(f.ctx, ControlAuthorityInput{
		Run: run, DeploymentID: run.AssignedAgentID, Runs: f.store, Conversations: f.chats, Catalog: convTeamCatalog{},
	})
	if err != nil {
		f.t.Fatalf("resolve member authority: %v", err)
	}
	preparer := newCatalogModelStepPreparer(catalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
	})
	host := fakeStepHost{scriptedModel: f.model, fakeHostTools: f.tools}
	producer, err := NewModelStepProducer(host, preparer, f.store)
	if err != nil {
		f.t.Fatal(err)
	}
	binding := &TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: loopFixtureAgent,
		DefinitionID: loopFixtureDefinition, DefinitionVersion: "1",
		ModelProvider: string(kitModelProvider), Model: kitModelName,
		ModelActions: append([]capability.ModelAction(nil), f.offered...),
		Producer:     producer, Session: session, Host: host, Authority: control,
		Preparers: []StepPreparer{newRunStartPreparer(sessions, f.store)},
		Tools:     f.dispatcher.ForClaim(LoopToolClaim{}),
	}
	return binding
}

// convDriveNextMember claims the next queued Run (a Team member or thread
// Run) and drives it to completion with convMemberBinding.
func convDriveNextMember(f *loopFixture) *AgentRun {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
		Scope: f.scope, WorkerID: f.workerID, Now: f.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		f.t.Fatalf("claim next member: %#v, %v", claimed, err)
	}
	for i := 0; ; i++ {
		binding := convMemberBinding(f, claimed)
		if binding.Session != nil && claimed.SessionID != binding.Session.ID {
			claimed = f.mustRun(claimed.ID)
		}
		exit := f.loop.Drive(f.ctx, RunLoopClaim{Run: claimed, WorkerID: f.workerID, Binding: binding})
		if exit.Err != nil && exit.Reason != RunLoopExitRequeued {
			f.t.Fatalf("member %s exited %s: %v", claimed.ID, exit.Reason, exit.Err)
		}
		run := f.mustRun(claimed.ID)
		if run.Status == AgentRunStatusCompleted {
			return run
		}
		if run.Status != AgentRunStatusQueued || i == 5 {
			f.t.Fatalf("member %s = %s (%#v), log %s", run.ID, run.Status, run.FailureExplanation, steerShape(f, run.ID))
		}
		if run.AvailableAt.After(f.clock.Now()) {
			f.clock.Advance(run.AvailableAt.Sub(f.clock.Now()) + time.Millisecond)
		}
		claimed = f.claim(run.ID)
	}
}

// convRoute schedules a Team message, drives its routing Run (the conversation
// runner's deterministic routing binding) and returns the routing Run.
func convRoute(f *loopFixture, conversationID string, trigger *ChannelMessage) *AgentRun {
	f.t.Helper()
	routing := convSchedule(f, conversationID, trigger)
	if routing.AssignedAgentID != "" || routing.SessionID != "" {
		f.t.Fatalf("routing Run = %#v", routing)
	}
	claimed := f.claim(routing.ID)
	binding, err := f.runner.ResolveTurnRunner(f.ctx, claimed)
	if err != nil {
		f.t.Fatal(err)
	}
	if exit := f.loop.Drive(f.ctx, RunLoopClaim{Run: claimed, WorkerID: f.workerID, Binding: binding}); exit.Reason != RunLoopExitFinal {
		f.t.Fatalf("routing exit = %#v", exit)
	}
	return f.mustRun(routing.ID)
}

// TestTeamMembersHaveTheirOwnSessionsAndTheRouterHasNone: a Team message
// addressed to two members is routed by a deterministic routing Run that has
// no session and makes no model call; each member answers in its own lane
// session, and the second member sees the first member's answer as a user
// entry attributed to that teammate.
func TestTeamMembersHaveTheirOwnSessionsAndTheRouterHasNone(t *testing.T) {
	eachStore(t, "conv-team", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withTeam("finn", "scout"))
		f.model.add(&modelRule{name: "member", reply: func(v stepView) ModelStepResult {
			return finalAnswer(map[string]any{"answer": v.Request().AgentID + " checked the canary."})
		}})
		team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}
		conversation := convChat(f, team, nil)
		trigger := convPost(f, conversation.ID, "@finn @scout can you both check the canary?", func(r *PostChannelMessageRequest) {
			r.Mentions = []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "finn"}, {Type: ConversationParticipantAgent, ID: "scout"}}
			r.RequiresResponse = true
		})
		routed := convRoute(f, conversation.ID, trigger)
		routing := routed
		ids, _ := routed.Output["responderRunIds"].([]interface{})
		if routed.Status != AgentRunStatusCompleted || routed.SessionID != "" || len(ids) != 2 {
			t.Fatalf("routing Run = %s session %q output %#v", routed.Status, routed.SessionID, routed.Output)
		}
		for _, req := range f.model.calls("") {
			if req.RunID == routing.ID {
				t.Fatalf("the routing Run called the model: %s", stepView{req: req}.describe())
			}
		}

		members := map[string]*AgentRun{}
		for _, id := range ids {
			run := f.mustRun(fmt.Sprint(id))
			members[run.AssignedAgentID] = run
		}
		if members["finn"] == nil || members["scout"] == nil {
			t.Fatalf("member Runs = %#v", members)
		}
		// The members answer one after the other; whoever goes first ("finn"
		// below) is seen by the second ("scout").
		finn := convDriveNextMember(f)
		finnAgent := ConversationParticipant{Type: ConversationParticipantAgent, ID: finn.AssignedAgentID}
		finnAnswer := convAgentAnswer(f, conversation.ID)
		if finnAnswer.Sender != finnAgent || finnAnswer.Content != finn.AssignedAgentID+" checked the canary." {
			t.Fatalf("first answer = %#v", finnAnswer)
		}
		scout := convDriveNextMember(f)
		if scout.AssignedAgentID == finn.AssignedAgentID {
			t.Fatalf("one member answered twice: %s", scout.AssignedAgentID)
		}

		// Each member has its own lane session; the router has none.
		for _, run := range []*AgentRun{finn, scout} {
			agent := run.AssignedAgentID
			lane := ConversationRunLane(conversation.ID, "", agent)
			session := f.session(run.ID)
			if run.ConcurrencyKey != lane || session.ID != conversationSessionID(f.scope, lane) || session.AgentID != agent || session.ConversationID != conversation.ID {
				t.Fatalf("%s session = %#v (run lane %q)", agent, session, run.ConcurrencyKey)
			}
		}
		if finn.SessionID == scout.SessionID {
			t.Fatal("members share a session")
		}

		// The second member answered the trigger, which came before the first
		// member's answer: its step saw only the trigger. When it is asked
		// again, catch-up brings the teammate's answer in as a user entry
		// attributed to that teammate, and skips its own earlier answer.
		scoutAnswer := convAgentAnswer(f, conversation.ID)
		again := convPost(f, conversation.ID, "@"+scout.AssignedAgentID+" what did your teammate find?", func(r *PostChannelMessageRequest) {
			r.Mentions = []ConversationParticipant{{Type: ConversationParticipantAgent, ID: scout.AssignedAgentID}}
			r.RequiresResponse = true
		})
		if rerouted := convRoute(f, conversation.ID, again); len(rerouted.Output["responderRunIds"].([]interface{})) != 1 {
			t.Fatalf("second routing = %#v", rerouted.Output)
		}
		scoutAgain := convDriveNextMember(f)
		if scoutAgain.AssignedAgentID != scout.AssignedAgentID || scoutAgain.SessionID != scout.SessionID {
			t.Fatalf("second member Run = %#v", scoutAgain)
		}
		caught := convUserBeforeAssistant(f, scoutAgain.ID)
		if ids := convMessageIDs(caught); len(ids) != 2 || ids[0] != finnAnswer.ID || ids[1] != again.ID {
			t.Fatalf("second member caught up %v, want [%s %s] (its own answer %s skipped)", ids, finnAnswer.ID, again.ID, scoutAnswer.ID)
		}
		if seen := caught[0]; convAuthor(seen) != "agent:"+finn.AssignedAgentID ||
			!strings.Contains(kitPartsText(seen.User.Content), "(agent, teammate "+finn.AssignedAgentID+")") {
			t.Fatalf("view of the teammate's answer = %s %q", convAuthor(seen), kitPartsText(seen.User.Content))
		}
		var scoutRequest ModelStepRequest
		for _, req := range f.model.calls(ModelStepPurposeStep) {
			if req.RunID == scoutAgain.ID {
				scoutRequest = req
				break
			}
		}
		if !slices.ContainsFunc(stepView{req: scoutRequest}.UserTexts(), func(text string) bool { return strings.Contains(text, finnAnswer.Content) }) {
			t.Fatalf("the teammate's answer is not rendered: %s", stepView{req: scoutRequest}.describe())
		}
		if text := convRunContext(f, convFirstStep(f, scout.ID)); !strings.Contains(text, "Team roster:") {
			t.Fatalf("member run_context has no team roster: %q", text)
		}

		// finn's session holds its own answer once: as the assistant entry,
		// never caught up again as a user entry.
		assistant := 0
		for _, entry := range f.entries(finn.ID) {
			if entry.Type == SessionEntryUser && entry.User != nil && entry.User.MessageID == finnAnswer.ID {
				t.Fatalf("finn caught up its own answer: %#v", entry)
			}
			if entry.Type == SessionEntryAssistant && entry.RunID == finn.ID {
				assistant++
			}
		}
		if assistant != 1 {
			t.Fatalf("finn's assistant entries = %d", assistant)
		}
	})
}

// ---------------------------------------------------------------------------
// Catch-up and seeding

// TestCatchUpAppendsNewVisibleMessagesWithAttribution: a chat session that
// already answered once catches up only what it can see since its watermark,
// each message a user entry attributed to its sender; its own Runs' posts and
// a private message to someone else are skipped, and the watermark moves.
func TestCatchUpAppendsNewVisibleMessagesWithAttribution(t *testing.T) {
	eachStore(t, "conv-catch-up", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(convAnswer("ok: ")))
		conversation, first := f.startChat("Start the release checklist")
		first = convDrive(f, first.ID)
		watermark := f.session(first.ID).ChannelWatermark
		if watermark == 0 {
			t.Fatal("first Run left no watermark")
		}

		ana := convPost(f, conversation.ID, "ana: the build is green", nil)
		ben := convPost(f, conversation.ID, "ben: staging looks fine", func(r *PostChannelMessageRequest) { r.Sender = convActor("ben") })
		routine := convPost(f, conversation.ID, "Nightly report: 0 failures", func(r *PostChannelMessageRequest) {
			r.Sender = ConversationParticipant{Type: ConversationParticipantService, ID: "routine-nightly"}
			r.Intent = MessageIntentUpdate
		})
		scout := convPost(f, conversation.ID, "scout: I checked the canary", func(r *PostChannelMessageRequest) {
			r.Sender = ConversationParticipant{Type: ConversationParticipantAgent, ID: "scout"}
			r.Intent = MessageIntentAnswer
		})
		// finn's own Run posted this (a progress note of the first Run): the
		// session already holds it.
		own := convPost(f, conversation.ID, "finn: still on it", func(r *PostChannelMessageRequest) {
			r.Sender = ConversationParticipant{Type: ConversationParticipantAgent, ID: loopFixtureAgent}
			r.Intent = MessageIntentUpdate
			r.References = []ConversationReference{{Kind: ConversationReferenceRun, ID: first.ID}}
		})
		private := convPost(f, conversation.ID, "ben, between us", func(r *PostChannelMessageRequest) {
			r.Audience = ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{convActor("ben")}}
		})
		trigger := convPost(f, conversation.ID, "What is left?", nil)
		run := f.chatRun(conversation, trigger)
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
			if text := kitPartsText(caught[i].User.Content); !strings.HasPrefix(text, "[") || !strings.Contains(strings.SplitN(text, "\n", 2)[0], header) {
				t.Fatalf("entry %d header = %q, want %s", i, text, header)
			}
		}
		if got := f.session(run.ID).ChannelWatermark; got < trigger.Sequence || got <= watermark {
			t.Fatalf("watermark = %d, want at least the trigger's %d (was %d)", got, trigger.Sequence, watermark)
		}
		// The second Run's step renders the first exchange, then the caught-up
		// messages: one session, one log.
		view := stepView{req: convFirstStep(f, run.ID)}
		if texts := view.UserTexts(); len(texts) < 6 || !strings.Contains(texts[0], "Start the release checklist") {
			t.Fatalf("second Run's step = %s", view.describe())
		}
	})

	t.Run("thread_root", func(t *testing.T) {
		// A message in a local thread runs in the thread's lane and session:
		// the canonical trigger is the thread message, the session holds only
		// the thread, and the answer goes back into the thread.
		f := newLoopFixture(t, newTestKernelStore(t), withRules(convAnswer("ok: ")))
		conversation := convChat(f, ObjectiveOwner{}, nil)
		root := convPost(f, conversation.ID, "Call one started", func(r *PostChannelMessageRequest) {
			r.Sender = ConversationParticipant{Type: ConversationParticipantService, ID: "calls"}
			r.Intent = MessageIntentSystem
		})
		outside := convPost(f, conversation.ID, "an unrelated question", nil)
		inThread := convPost(f, conversation.ID, "first call request", func(r *PostChannelMessageRequest) {
			r.ReplyToMessageID = root.ID
			r.RequiresResponse = true
		})
		run := convSchedule(f, conversation.ID, inThread)
		lane := ConversationRunLane(conversation.ID, root.ID, loopFixtureAgent)
		if run.Context["threadRootMessageId"] != root.ID || run.ConcurrencyKey != lane {
			t.Fatalf("thread Run = key %q context %#v", run.ConcurrencyKey, run.Context)
		}
		run = convDriveNextMember(f)
		session := f.session(run.ID)
		if session.ThreadRootID != root.ID || session.ID != conversationSessionID(f.scope, lane) {
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
		if ordinary.ConcurrencyKey != ConversationRunLane(conversation.ID, "", loopFixtureAgent) || ordinary.ID == run.ID {
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
	eachStore(t, "conv-seed", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(convAnswer("ok: ")))
		conversation := convChat(f, ObjectiveOwner{}, nil)
		var messages []*ChannelMessage
		for i := 1; i <= 45; i++ {
			sender, intent := convActor(loopFixtureUser), MessageIntentQuestion
			if i%2 == 0 {
				sender, intent = ConversationParticipant{Type: ConversationParticipantAgent, ID: loopFixtureAgent}, MessageIntentAnswer
			}
			messages = append(messages, convPost(f, conversation.ID, fmt.Sprintf("message #%d", i), func(r *PostChannelMessageRequest) {
				r.Sender, r.Intent = sender, intent
			}))
		}
		run := convDrive(f, f.chatRun(conversation, messages[44]).ID)

		entries := convRunEntries(f, run.ID)
		var users []*SessionEntry
		omitted := -1
		for i, entry := range entries {
			if entry.Type == SessionEntryAssistant {
				break
			}
			if entry.Type == SessionEntryNote && entry.Note != nil && entry.Note.Kind == NoteHistoryOmitted && omitted < 0 {
				omitted = i
			}
			if entry.Type == SessionEntryUser && entry.User != nil {
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
			header := strings.SplitN(kitPartsText(entry.User.Content), "\n", 2)[0]
			switch author {
			case "user:" + loopFixtureUser:
			case "agent:" + loopFixtureAgent:
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
		f := newLoopFixture(t, newTestKernelStore(t), withRules(convAnswer("ok: ")))
		conversation := convChat(f, ObjectiveOwner{}, &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "telegram-endpoint"})
		source := func(id, thread string) *ExternalMessageSource {
			return &ExternalMessageSource{Provider: "telegram", ChannelType: "private", ChannelID: "chat-7", ThreadID: thread, MessageID: id, ParticipantID: "kev"}
		}
		earlier := convPost(f, conversation.ID, "my flight is at 9", func(r *PostChannelMessageRequest) { r.ExternalSource = source("chat-7:1", "") })
		quoted := convPost(f, conversation.ID, "and the hotel is booked", func(r *PostChannelMessageRequest) { r.ExternalSource = source("chat-7:2", "") })
		reply := convPost(f, conversation.ID, "when should I leave?", func(r *PostChannelMessageRequest) {
			r.ExternalSource = source("chat-7:3", "reply:2")
			r.ReplyToMessageID = quoted.ID
			r.RequiresResponse = true
		})
		if root := externalConversationThreadRoot(conversation, reply); root != "" {
			t.Fatalf("a Telegram DM reply root became a thread: %s", root)
		}
		run := convSchedule(f, conversation.ID, reply)
		if run.ConcurrencyKey != ConversationRunLane(conversation.ID, "", loopFixtureAgent) || run.Context["threadRootMessageId"] != nil {
			t.Fatalf("Telegram DM reply Run = key %q context %#v", run.ConcurrencyKey, run.Context)
		}
		run = steerToRest(f, run.ID, 6)
		ids := convMessageIDs(convUserBeforeAssistant(f, run.ID))
		if !slices.Contains(ids, earlier.ID) || !slices.Contains(ids, quoted.ID) || !slices.Contains(ids, reply.ID) {
			t.Fatalf("Telegram DM seed = %v, want the whole chat (%s %s %s)", ids, earlier.ID, quoted.ID, reply.ID)
		}
		if session := f.session(run.ID); session.ThreadRootID != "" {
			t.Fatalf("Telegram DM session is a thread session: %#v", session)
		}
	})
}

// ---------------------------------------------------------------------------
// react, participation, spoken replies

// convParticipation turns the chat's participation opt-in on or off.
func convParticipation(f *loopFixture, conversationID string, enabled bool) {
	f.t.Helper()
	conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.convs.UpdateConversation(f.ctx, UpdateConversationRequest{
		Scope: f.scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision, ParticipationEnabled: &enabled,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// convToolEntries is runID's tool entries named name.
func convToolEntries(f *loopFixture, runID, name string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range convRunEntries(f, runID) {
		if entry.Type == SessionEntryTool && entry.Tool != nil && entry.Tool.Name == name {
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
		eachStore(t, "conv-react", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store)
			var triggerID string
			f.model.add(
				&modelRule{name: "react", when: func(v stepView) bool { _, _, ok := v.ToolResult(string(ControlToolReact)); return !ok },
					reply: func(v stepView) ModelStepResult {
						if !v.HasTool(string(ControlToolReact)) {
							t.Errorf("react is not offered: %s", v.describe())
						}
						return act("Picking it up.", call(string(ControlToolReact), map[string]any{"messageId": triggerID, "emoji": "👀"}))
					}},
				&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": "On it: the deploy is green."})
				}},
			)
			conversation := convChat(f, ObjectiveOwner{}, nil)
			trigger := convPost(f, conversation.ID, "Is the deploy green?", nil)
			triggerID = trigger.ID
			run := convDrive(f, f.chatRun(conversation, trigger).ID)
			reacts := convToolEntries(f, run.ID, string(ControlToolReact))
			if len(reacts) != 1 || reacts[0].State != ToolEntryCompleted || reacts[0].Tool.Result.IsError {
				t.Fatalf("react entries = %#v", reacts)
			}
			reacted, err := f.convs.GetChannelMessage(f.ctx, f.scope, conversation.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: loopFixtureAgent}
			if len(reacted.Reactions) != 1 || reacted.Reactions[0].Emoji != "👀" || !slices.Contains(reacted.Reactions[0].Actors, agent) {
				t.Fatalf("reactions = %#v", reacted.Reactions)
			}
			if answer := convAgentAnswer(f, conversation.ID); answer.Content != "On it: the deploy is green." {
				t.Fatalf("answer = %#v", answer)
			}
		})
	})

	t.Run("participation_opt_in", func(t *testing.T) {
		f := newLoopFixture(t, newTestKernelStore(t), withRules(convAnswer("ok: ")))
		convRunner(f, ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
		conversation := convChat(f, ObjectiveOwner{}, nil)

		// Not opted in: the Run ends at once, with no model call and no post.
		quiet := convPost(f, conversation.ID, "anyone there?", nil)
		skipped := convDrive(f, f.chatRun(conversation, quiet).ID)
		if skipped.Output["participationSkipped"] != true || len(steerAgentMessages(f, conversation.ID)) != 0 {
			t.Fatalf("Run outside the opt-in = %#v, messages %s", skipped.Output, kitDescribeMessages(f.messages(conversation.ID)))
		}
		if len(f.model.calls("")) != 0 {
			t.Fatalf("a Run outside the opt-in called the model %d times", len(f.model.calls("")))
		}

		// Opted in: a later message is answered; the earlier one stays unanswered.
		convParticipation(f, conversation.ID, true)
		asked := convPost(f, conversation.ID, "now?", nil)
		answered := convDrive(f, f.chatRun(conversation, asked).ID)
		if answered.Output["participationSkipped"] == true || convAgentAnswer(f, conversation.ID).ReplyToMessageID != asked.ID {
			t.Fatalf("opted-in Run = %#v, messages %s", answered.Output, kitDescribeMessages(f.messages(conversation.ID)))
		}
		current, err := f.convs.GetConversation(f.ctx, f.scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p := current.Participation; p == nil || !p.Enabled || quiet.Sequence > p.AfterSequence {
			t.Fatalf("participation = %#v, want the opt-in to start after message %d", p, quiet.Sequence)
		}
	})

	t.Run("participation_opt_out_discards_the_answer", func(t *testing.T) {
		eachStore(t, "conv-optout", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store)
			convRunner(f, ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
			conversation := convChat(f, ObjectiveOwner{}, nil)
			convParticipation(f, conversation.ID, true)
			f.dispatch.on("list_items", func(ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"items": []interface{}{"a", "b"}}, nil
			})
			f.model.add(
				&modelRule{name: "read", times: 1, reply: func(v stepView) ModelStepResult {
					return withUsage(act("Reading the items.", call(kitGovernedTool(v, "list_items"), nil)), 1200, 40)
				}},
				&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
					return withUsage(finalAnswer(map[string]any{"answer": "There are two items."}), 1300, 30)
				}, before: func(_ context.Context, _ stepView) error {
					// The channel opts out while the Run works.
					convParticipation(f, conversation.ID, false)
					return nil
				}},
			)
			trigger := convPost(f, conversation.ID, "How many items?", nil)
			run := convDrive(f, f.chatRun(conversation, trigger).ID)
			if run.Output["participationSkipped"] != true {
				t.Fatalf("opted-out Run output = %#v", run.Output)
			}
			if messages := steerAgentMessages(f, conversation.ID); len(messages) != 0 {
				t.Fatalf("the discarded answer was posted: %s", kitDescribeMessages(messages))
			}
			var in, out int64
			for _, step := range f.steps(run.ID) {
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
	f := newLoopFixture(t, newTestKernelStore(t), withRules(&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
		args := map[string]any{"answer": "Leave at seven."}
		// The spoken final tool requires silent.
		for _, tool := range v.Request().Tools {
			if raw, _ := json.Marshal(tool); tool.Name == string(ControlToolFinalAnswer) && strings.Contains(string(raw), `"silent"`) {
				args["silent"] = false
			}
		}
		return finalAnswer(args)
	}}))
	conversation := convChat(f, ObjectiveOwner{}, nil)
	spoken := convPost(f, conversation.ID, "when should I leave?", func(r *PostChannelMessageRequest) {
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
	written := convPost(f, conversation.ID, "and the hotel?", func(r *PostChannelMessageRequest) { r.RequiresResponse = true })
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
