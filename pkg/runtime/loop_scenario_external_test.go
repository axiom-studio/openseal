package runtime

// External channel journeys on the run loop (run-loop section 12 §5.10,
// tests-T13). The loopFixture drives real chat Runs; an external endpoint on
// the same store stands for the connector (Slack or webchat) that the Run's
// conversation came from. The external workers then project what the loop
// wrote: step narration stays the step's activity summary, turn_report_progress
// becomes forwarded commentary, the answer preview streams into one provider
// message that the committed answer finishes, and the progress
// acknowledgement is one sessionless, tool-free model step.
//
// The failure-reply tests (TestExternalFinalFailureAnswerSurvivesBusyChannel,
// TestTerminalFailureReplyUsesTheTypedExplanation) belong to CONV-06.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// externalLoopStore is what the loop and the external workers need of one
// store.
type externalLoopStore interface {
	KernelStore
	RunProgressAcknowledgementStore
	ExternalConversationReplyStore
}

// externalLoopFixture is a loopFixture whose chats arrive through an external
// endpoint of the Run's owner.
type externalLoopFixture struct {
	*loopFixture
	ext      externalLoopStore
	adapters *skill.Catalog
	endpoint *ExternalConversationEndpoint
	host     *streamChatHost
	progress *RunProgressAcknowledgementWorker
	replies  *ExternalConversationReplyWorker
	delivery *ExternalConversationDeliveryWorker
}

// externalChat is one inbound provider message and the Run answering it.
type externalChat struct {
	conversation *Conversation
	inbound      *ChannelMessage
	run          *AgentRun
	item         *ExternalConversationInboxItem
}

// newExternalLoopFixture creates the endpoint (owned by finn, or by Team
// "team" withTeam) and moves the loop fixture into the endpoint's scope.
// renderer is the progress worker's acknowledgement renderer; nil refuses to
// render.
func newExternalLoopFixture(t *testing.T, store KernelStore, provider string, operations []capability.ConversationDeliveryOperation, renderer RunProgressAcknowledgementRenderer, opts ...loopOption) *externalLoopFixture {
	t.Helper()
	ext, ok := store.(externalLoopStore)
	if !ok {
		t.Fatalf("%T is not an external conversation store", store)
	}
	f := &externalLoopFixture{loopFixture: newLoopFixture(t, store, opts...), ext: ext, host: &streamChatHost{messages: map[string]string{}}}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent}
	if len(f.config.team) > 0 {
		owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}
	}
	f.adapters, f.endpoint = externalConversationEndpointFixtureOwnedBy(t, f.ctx, ext, provider, operations, owner)
	// The endpoint fixture fixes the scope ({tenant, <provider>}); the
	// fixture Skill is bound there too so governed calls resolve.
	f.scope = f.endpoint.Scope
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: loopFixtureBindingID, Scope: skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, DeploymentID: loopFixtureAgent,
		SkillID: loopFixtureSkillID, SkillVersion: loopFixtureSkillVer,
		AllowedActions: []string{"lookup", "list_items", "update_status", "send", "explode"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if renderer == nil {
		renderer = RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
			return "", errors.New("this scenario renders no progress acknowledgement")
		})
	}
	var err error
	if f.progress, err = NewRunProgressAcknowledgementWorker(ext, f.adapters, renderer, RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	}, models.WithClock(f.clock.Now)); err != nil {
		t.Fatal(err)
	}
	if f.replies, err = NewExternalConversationReplyWorker(ext, f.adapters, models.WithClock(f.clock.Now)); err != nil {
		t.Fatal(err)
	}
	if f.delivery, err = NewExternalConversationDeliveryWorker(ext, f.adapters, f.host, ExternalConversationDeliveryWorkerConfig{WorkerID: "delivery"}, models.WithClock(f.clock.Now)); err != nil {
		t.Fatal(err)
	}
	return f
}

// startExternal posts text from the provider into a new external-origin
// conversation, schedules its Run and records the applied inbox item.
func (f *externalLoopFixture) startExternal(text string) externalChat {
	f.t.Helper()
	conversation, _, err := f.convs.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.scope, Owner: f.endpoint.Owner, Title: "Provider thread",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: f.endpoint.ID, Version: f.endpoint.Revision},
		IdempotencyKey: "external-loop-chat",
	})
	if err != nil {
		f.t.Fatalf("create conversation: %v", err)
	}
	posted, err := f.convs.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: loopFixtureUser}, Intent: MessageIntentQuestion,
		Content: text, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "external-loop-inbound",
	})
	if err != nil {
		f.t.Fatalf("post inbound: %v", err)
	}
	run := f.chatRun(conversation, posted.Message)
	now := f.clock.Now()
	item := &ExternalConversationInboxItem{
		ID: "external-loop-inbox", Scope: f.scope, EndpointID: f.endpoint.ID,
		EndpointRevision: f.endpoint.Revision, Adapter: f.endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "external-loop-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "conversation", ExternalThreadID: "thread", ExternalMessageID: "message",
			ExternalParticipantID: loopFixtureUser, Text: text, OrderingKey: "conversation:message", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID, RunID: run.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := f.ext.ReceiveExternalConversationEvent(f.ctx, item); err != nil || replayed {
		f.t.Fatalf("store inbox item: replayed=%t err=%v", replayed, err)
	}
	return externalChat{conversation: conversation, inbound: posted.Message, run: run, item: item}
}

// commentary runs the progress worker's commentary pass (commentary, answer
// stream, thread status). It returns an error instead of failing, so model
// rules can call it from inside a step.
func (f *externalLoopFixture) commentary() ([]*ExternalConversationDelivery, error) {
	return f.progress.ProcessCommentaryScope(f.ctx, f.scope)
}

func (f *externalLoopFixture) mustCommentary() []*ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.commentary()
	if err != nil {
		f.t.Fatalf("commentary pass: %v", err)
	}
	return deliveries
}

// deliverAll drains the outbox through the fake provider chat.
func (f *externalLoopFixture) deliverAll() error {
	for range 20 {
		delivery, err := f.delivery.ProcessOne(f.ctx, f.scope)
		if err != nil {
			return err
		}
		if delivery == nil {
			return nil
		}
	}
	return errors.New("outbox did not drain")
}

func (f *externalLoopFixture) mustDeliverAll() {
	f.t.Helper()
	if err := f.deliverAll(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *externalLoopFixture) reply() []*ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.replies.ProcessScope(f.ctx, f.scope, 10)
	if err != nil {
		f.t.Fatalf("reply pass: %v", err)
	}
	return deliveries
}

// outbox is every delivery of the endpoint whose correlation kind is kind
// ("" = all).
func (f *externalLoopFixture) outbox(kind string) []*ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.ext.ListExternalConversationDeliveries(f.ctx, ExternalConversationDeliveryFilter{Scope: f.scope, EndpointID: f.endpoint.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []*ExternalConversationDelivery
	for _, delivery := range deliveries {
		if kind == "" || delivery.Correlation != nil && delivery.Correlation.Kind == kind {
			out = append(out, delivery)
		}
	}
	return out
}

func (f *externalLoopFixture) operations() []capability.ConversationDeliveryOperation {
	out := make([]capability.ConversationDeliveryOperation, 0, len(f.host.requests))
	for _, request := range f.host.requests {
		out = append(out, request.Delivery.Operation)
	}
	return out
}

// eachExternalStore runs test on every matrix store that also carries the
// external conversation tables.
func eachExternalStore(t *testing.T, prefix string, test func(t *testing.T, store KernelStore)) {
	t.Helper()
	eachStore(t, prefix, func(t *testing.T, store KernelStore) {
		if _, ok := store.(externalLoopStore); !ok {
			t.Skipf("%T has no external conversation store", store)
		}
		test(t, store)
	})
}

// previewFrame is one turn.answer_preview event of a Run, read from its
// payload.
type previewFrame struct {
	attempt  string
	sequence string
	text     string
	reset    bool
}

func (p previewFrame) String() string {
	if p.reset {
		return fmt.Sprintf("reset(%s#%s)", p.attempt, p.sequence)
	}
	return fmt.Sprintf("text(%s#%s %q)", p.attempt, p.sequence, p.text)
}

func (f *loopFixture) previewFrames(runID string) []previewFrame {
	f.t.Helper()
	var out []previewFrame
	for _, event := range f.events(runID, "turn.answer_preview") {
		frame := previewFrame{
			attempt: fmt.Sprint(event.Payload["attemptId"]), sequence: fmt.Sprint(event.Payload["sequence"]),
		}
		frame.text, _ = event.Payload["text"].(string)
		frame.reset, _ = event.Payload["reset"].(bool)
		out = append(out, frame)
	}
	return out
}

// lastAgentMessage is the newest channel message sent by an Agent with intent.
func lastAgentMessage(messages []*ChannelMessage, intent ConversationMessageIntent) *ChannelMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Sender.Type == ConversationParticipantAgent && messages[i].Intent == intent {
			return messages[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Narration and commentary

// TestLoopNarrationIsTheStepSummaryNotCommentary: a step's narration is its
// step row's outputSummary and its turn.narration event. It is never
// turn.commentary, so the connector forwards nothing for it.
func TestLoopNarrationIsTheStepSummaryNotCommentary(t *testing.T) {
	const narration = "Looking up issue 7."
	eachExternalStore(t, "external_narration", func(t *testing.T, store KernelStore) {
		f := newExternalLoopFixture(t, store, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, nil, withRules(
			&modelRule{name: "lookup", times: 1, reply: func(v stepView) ModelStepResult {
				return act(narration, call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return say("Issue 7 is open.") }},
		))
		chat := f.startExternal("Is issue 7 open?")
		f.mustStatus(f.driveToRest(chat.run.ID, 4), AgentRunStatusCompleted)

		steps := f.steps(chat.run.ID)
		if len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if steps[0].OutputSummary != narration {
			t.Fatalf("step 1 outputSummary = %q, want %q", steps[0].OutputSummary, narration)
		}
		if steps[1].OutputSummary != "" {
			t.Fatalf("the final step has narration %q", steps[1].OutputSummary)
		}
		narrations := f.events(chat.run.ID, ActivityEventTurnNarration)
		if len(narrations) != 1 || narrations[0].Summary != narration || narrations[0].TurnID != steps[0].ID {
			t.Fatalf("turn.narration events = %#v", narrations)
		}
		if commentary := f.events(chat.run.ID, "turn.commentary"); len(commentary) != 0 {
			t.Fatalf("narration became commentary: %#v", commentary)
		}

		for range 2 {
			f.mustCommentary()
		}
		if forwarded := f.outbox("run_commentary"); len(forwarded) != 0 {
			t.Fatalf("narration was forwarded: %#v", forwarded)
		}
		messages := f.messages(chat.conversation.ID)
		for _, message := range messages {
			if strings.Contains(message.Content, narration) {
				t.Fatalf("narration reached the channel: %s", kitDescribeMessages(messages))
			}
		}
		if answer := lastAgentMessage(messages, MessageIntentAnswer); answer == nil || answer.Content != "Issue 7 is open." || len(messages) != 2 {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
	})
}

// TestLoopReportProgressStaysCommentaryAndReachesSlack: turn_report_progress
// writes turn.commentary, and the connector forwards it once as a channel
// message and a Slack delivery, under the member's name in a Team.
func TestLoopReportProgressStaysCommentaryAndReachesSlack(t *testing.T) {
	const update = "Found the failing health check; fixing it now."
	for _, team := range []bool{false, true} {
		name, speaker := "agent", loopFixtureAgent
		var opts []loopOption
		if team {
			name, speaker = "team_member", "agent-a"
			opts = append(opts, withTeam("agent-a"))
		}
		t.Run(name, func(t *testing.T) {
			eachExternalStore(t, "external_progress", func(t *testing.T, store KernelStore) {
				f := newExternalLoopFixture(t, store, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, nil, append(opts, withRules(
					&modelRule{name: "progress", times: 1, reply: func(v stepView) ModelStepResult {
						if !v.HasTool(string(ControlToolReportProgress)) {
							t.Errorf("turn_report_progress is not offered: %s", v.describe())
						}
						return act("", call(string(ControlToolReportProgress), map[string]any{"summary": update}))
					}},
					&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return say("The health check is fixed.") }},
				))...)
				chat := f.startExternal("Why is the deploy failing?")
				f.mustStatus(f.driveToRest(chat.run.ID, 4), AgentRunStatusCompleted)

				commentary := f.events(chat.run.ID, "turn.commentary")
				if len(commentary) != 1 || commentary[0].Summary != update || commentary[0].AgentID != speaker {
					t.Fatalf("turn.commentary events = %#v", commentary)
				}
				if entries := f.toolEntries(chat.run.ID); len(entries) != 1 || entries[0].State != ToolEntryCompleted {
					t.Fatalf("progress tool entries = %v", f.entryTypes(chat.run.ID))
				}

				first := f.mustCommentary()
				if len(first) != 1 || first[0].Operation != capability.ConversationDeliveryMessageSend ||
					first[0].Correlation == nil || first[0].Correlation.Kind != "run_commentary" || first[0].Correlation.ID != chat.run.ID ||
					first[0].ExternalThreadID != "thread" || first[0].ExternalConversationID != "conversation" {
					t.Fatalf("commentary deliveries = %#v", first)
				}
				f.mustCommentary()
				forwarded := f.outbox("run_commentary")
				if len(forwarded) != 1 {
					t.Fatalf("commentary forwarded %d times, want 1", len(forwarded))
				}
				message, err := f.ext.GetChannelMessage(f.ctx, f.scope, chat.conversation.ID, forwarded[0].ChannelMessageID)
				if err != nil || message == nil || message.Content != update || message.Sender.ID != speaker || message.Intent != MessageIntentAcknowledgment {
					t.Fatalf("forwarded message = %#v, %v", message, err)
				}
				if answer := lastAgentMessage(f.messages(chat.conversation.ID), MessageIntentAnswer); answer == nil || answer.Content != "The health check is fixed." || answer.Sender.ID != speaker {
					t.Fatalf("answer = %#v", answer)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Answer preview and streaming

// previewRule reports an answer preview from inside the model step, as atlas
// does while it streams, and then replies.
func previewRule(name, attempt, text string, reply func(stepView) ModelStepResult, after func(context.Context) error) *modelRule {
	return &modelRule{name: name, times: 1, reply: reply, before: func(ctx context.Context, _ stepView) error {
		if !HasTurnAnswerPreview(ctx) {
			return errors.New("the loop installed no answer preview observer for the step")
		}
		if err := ReportTurnAnswerPreview(ctx, TurnAnswerPreview{AttemptID: attempt, Sequence: 1, Text: text}); err != nil {
			return err
		}
		if after != nil {
			return after(ctx)
		}
		return nil
	}}
}

// TestLoopAnswerPreviewOnlyForTheFinalStep: a step that previews and then
// calls tools has its preview reset by the kernel; only the final step's
// preview stands, and the canonical answer replaces it.
func TestLoopAnswerPreviewOnlyForTheFinalStep(t *testing.T) {
	eachStore(t, "external_preview", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			previewRule("step-1", "attempt-1", "Checking…", func(v stepView) ModelStepResult {
				return act("Checking issue 7.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}, nil),
			previewRule("step-2", "attempt-2", "Issue is open", func(stepView) ModelStepResult { return say("Issue 7 is open.") }, nil),
		))
		conversation, run := f.startChat("Is issue 7 open?")
		f.mustStatus(f.driveToRest(run.ID, 4), AgentRunStatusCompleted)

		frames := f.previewFrames(run.ID)
		want := []previewFrame{
			{attempt: "attempt-1", sequence: "1", text: "Checking…"},
			{attempt: "attempt-1", sequence: "2", reset: true},
			{attempt: "attempt-2", sequence: "1", text: "Issue is open"},
		}
		if fmt.Sprint(frames) != fmt.Sprint(want) {
			t.Fatalf("preview frames = %v, want %v", frames, want)
		}
		steps := f.steps(run.ID)
		events := f.events(run.ID, "turn.answer_preview")
		if len(steps) != 2 || events[0].TurnID != steps[0].ID || events[1].TurnID != steps[0].ID || events[2].TurnID != steps[1].ID {
			t.Fatalf("preview frames are not on their steps: steps=%d events=%#v", len(steps), events)
		}
		messages := f.messages(conversation.ID)
		answer := lastAgentMessage(messages, MessageIntentAnswer)
		if answer == nil || answer.Content != "Issue 7 is open." || len(messages) != 2 {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
	})
}

// TestExternalAnswerStreamFollowsTheLoopPreview: on a connector that can edit
// messages, the step-1 preview opens one provider message, the reset after
// its tool calls leaves it alone, the step-2 preview edits it and the
// committed answer finishes it. No second provider message appears.
func TestExternalAnswerStreamFollowsTheLoopPreview(t *testing.T) {
	const (
		firstPreview  = "Checking the deployment health first."
		secondPreview = "Issue 7 is open and assigned to Ana."
		finalAnswer   = "Issue 7 is open and assigned to Ana since Monday."
	)
	eachExternalStore(t, "external_stream", func(t *testing.T, store KernelStore) {
		var f *externalLoopFixture
		// stream runs the commentary pass from inside a step and delivers what
		// it enqueued; want is the operations it must enqueue.
		stream := func(want ...capability.ConversationDeliveryOperation) error {
			deliveries, err := f.commentary()
			if err != nil {
				return err
			}
			got := make([]capability.ConversationDeliveryOperation, 0, len(deliveries))
			for _, delivery := range deliveries {
				got = append(got, delivery.Operation)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return fmt.Errorf("stream enqueued %v, want %v", got, want)
			}
			return f.deliverAll()
		}
		f = newExternalLoopFixture(t, store, "webchat", []capability.ConversationDeliveryOperation{
			capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate,
		}, nil, withRules(
			previewRule("step-1", "attempt-1", firstPreview, func(v stepView) ModelStepResult {
				return act("Checking issue 7.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}, func(context.Context) error { return stream(capability.ConversationDeliveryMessageSend) }),
			&modelRule{name: "step-2", times: 1,
				before: func(ctx context.Context, _ stepView) error {
					// The step-1 reset is the newest preview: nothing is posted
					// and the streamed message keeps its text.
					f.clock.Advance(5 * time.Second)
					if err := stream(); err != nil {
						return err
					}
					if len(f.host.messages) != 1 || f.host.messages["chat-1"] != firstPreview {
						return fmt.Errorf("chat after the reset = %#v", f.host.messages)
					}
					if err := ReportTurnAnswerPreview(ctx, TurnAnswerPreview{AttemptID: "attempt-2", Sequence: 1, Text: secondPreview}); err != nil {
						return err
					}
					return stream(capability.ConversationDeliveryMessageUpdate)
				},
				reply: func(stepView) ModelStepResult { return say(finalAnswer) }},
		))
		chat := f.startExternal("Is issue 7 open?")
		f.mustStatus(f.driveToRest(chat.run.ID, 4), AgentRunStatusCompleted)
		if len(f.host.messages) != 1 || f.host.messages["chat-1"] != secondPreview {
			t.Fatalf("chat before the reply = %#v", f.host.messages)
		}

		// A finished Run streams nothing more; the committed answer finishes
		// the streamed message.
		if deliveries := f.mustCommentary(); len(deliveries) != 0 {
			t.Fatalf("a finished Run kept streaming: %#v", deliveries)
		}
		for range 2 {
			f.reply()
			f.mustDeliverAll()
		}
		final, err := f.ext.GetExternalConversationDelivery(f.ctx, f.scope, stableExternalConversationID(f.scope, f.endpoint.ID, "delivery", externalConversationReplyKey(chat.item.ID)))
		if err != nil || final == nil || final.Operation != capability.ConversationDeliveryMessageUpdate || final.Parameters["providerMessageId"] != "chat-1" ||
			final.Status != ExternalConversationDeliveryDelivered {
			t.Fatalf("committed reply = %#v, %v", final, err)
		}
		answer := lastAgentMessage(f.messages(chat.conversation.ID), MessageIntentAnswer)
		if answer == nil || answer.Content != finalAnswer || final.ChannelMessageID != answer.ID {
			t.Fatalf("committed reply message = %#v, answer %#v", final, answer)
		}
		want := []capability.ConversationDeliveryOperation{
			capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryMessageUpdate,
		}
		if got := f.operations(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("provider operations = %v, want %v", got, want)
		}
		if len(f.host.messages) != 1 || f.host.messages["chat-1"] != finalAnswer {
			t.Fatalf("chat = %#v", f.host.messages)
		}
	})
}

// ---------------------------------------------------------------------------
// Progress acknowledgement

// TestProgressAcknowledgementIsOneToolFreeStepWithoutSession: the progress
// worker's model-step renderer (as the catalog resolver builds it) makes
// exactly one acknowledgement model step with no tools and no session, and
// writes no session and no step row for the Run. A native progress status
// is used when the connector has one, a text message otherwise. A reply with
// a tool call is refused and nothing is delivered.
func TestProgressAcknowledgementIsOneToolFreeStepWithoutSession(t *testing.T) {
	const ack = "Checking the deployment now."
	for _, c := range []struct {
		name       string
		provider   string
		operations []capability.ConversationDeliveryOperation
		toolCall   bool
		want       capability.ConversationDeliveryOperation
	}{
		{name: "native_status", provider: "slack", operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator}, want: capability.ConversationDeliveryTypingIndicator},
		{name: "text_fallback", provider: "webchat", operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, want: capability.ConversationDeliveryMessageSend},
		{name: "tool_call_refused", provider: "webchat", operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, toolCall: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachExternalStore(t, "external_ack_"+c.provider, func(t *testing.T, store KernelStore) {
				var f *externalLoopFixture
				renderer := &lateProgressAckRenderer{}
				f = newExternalLoopFixture(t, store, c.provider, c.operations, renderer, withRules(
					&modelRule{name: "ack",
						when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeAcknowledgement },
						reply: func(v stepView) ModelStepResult {
							if c.toolCall {
								return act("", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
							}
							return say(ack)
						}},
					&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return say("The deployment is healthy.") }},
				))
				renderer.inner = &modelStepProgressAcknowledgementRenderer{
					host: fakeStepHost{scriptedModel: f.model, fakeHostTools: f.tools}, agentID: loopFixtureAgent,
					definitionID: loopFixtureDefinition, definitionVersion: "1",
					system: []string{"You are finn, the fixture Agent.", runProgressAcknowledgementSystemInstruction},
					now:    f.clock.Now,
				}
				chat := f.startExternal("Can you check the deployment?")
				f.clock.Advance(time.Minute)

				deliveries, err := f.progress.ProcessScope(f.ctx, f.scope)
				acks := f.model.calls(ModelStepPurposeAcknowledgement)
				if len(acks) != 1 || len(f.model.calls("")) != 1 {
					t.Fatalf("model calls = %d (acknowledgements %d), want exactly one acknowledgement", len(f.model.calls("")), len(acks))
				}
				req := acks[0]
				if req.SessionID != "" || len(req.Tools) != 0 || req.RunID != chat.run.ID || req.AgentID != loopFixtureAgent || len(req.Entries) != 1 {
					t.Fatalf("acknowledgement request is not a sessionless tool-free step: %s session=%q run=%q", stepView{req: req}.describe(), req.SessionID, req.RunID)
				}
				// The chat Run names its lane session from creation; the
				// acknowledgement must not have created it, nor any step row.
				current := f.mustRun(chat.run.ID)
				if session, err := f.raw.(AgentSessionStore).GetAgentSession(f.ctx, f.scope, current.SessionID); err == nil && session != nil {
					t.Fatalf("the acknowledgement created session %s", session.ID)
				}
				if steps := f.steps(chat.run.ID); len(steps) != 0 {
					t.Fatalf("the acknowledgement wrote %d step rows", len(steps))
				}
				acknowledgements := f.outbox(runProgressAcknowledgementCorrelationKind)
				messages := f.messages(chat.conversation.ID)
				if c.toolCall {
					if err == nil || !strings.Contains(err.Error(), "actionable progress acknowledgement") || len(deliveries) != 0 || len(acknowledgements) != 0 || len(messages) != 1 {
						t.Fatalf("a tool-call acknowledgement was accepted: deliveries=%#v err=%v channel=%s", deliveries, err, kitDescribeMessages(messages))
					}
				} else {
					if err != nil || len(deliveries) != 1 || deliveries[0].Operation != c.want || len(acknowledgements) != 1 {
						t.Fatalf("acknowledgement deliveries = %#v, %v", deliveries, err)
					}
					switch c.want {
					case capability.ConversationDeliveryTypingIndicator:
						if deliveries[0].Parameters["status"] != ack || len(messages) != 1 {
							t.Fatalf("native status = %#v, channel %s", deliveries[0].Parameters, kitDescribeMessages(messages))
						}
					default:
						posted := lastAgentMessage(messages, MessageIntentAcknowledgment)
						if posted == nil || posted.Content != ack || posted.Sender.ID != loopFixtureAgent || len(messages) != 2 {
							t.Fatalf("text acknowledgement channel = %s", kitDescribeMessages(messages))
						}
					}
				}

				// The Run itself then works through the loop as usual: one step,
				// one session, and the acknowledgement left no entry in it.
				f.mustStatus(f.driveToRest(chat.run.ID, 4), AgentRunStatusCompleted)
				if steps := f.steps(chat.run.ID); len(steps) != 1 {
					t.Fatalf("steps = %d, want 1", len(steps))
				}
				if got := f.model.calls(ModelStepPurposeStep); len(got) != 1 {
					t.Fatalf("chat steps = %d, want 1", len(got))
				}
				for _, entry := range f.entries(chat.run.ID) {
					if entry.Type == SessionEntryAssistant && entry.Assistant != nil && strings.Contains(entry.Assistant.Text, ack) {
						t.Fatalf("the acknowledgement reached the session: %v", f.entryTypes(chat.run.ID))
					}
				}
			})
		})
	}
}

// lateProgressAckRenderer lets the renderer be built after the fixture (it
// needs the fixture's scripted model).
type lateProgressAckRenderer struct {
	inner RunProgressAcknowledgementRenderer
}

func (r *lateProgressAckRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request RunProgressAcknowledgementRequest) (string, error) {
	if r.inner == nil {
		return "", errors.New("progress acknowledgement renderer is not built yet")
	}
	return r.inner.RenderRunProgressAcknowledgement(ctx, request)
}
