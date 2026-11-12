package runtime

// External channel journeys on the run loop (run-loop section 12 §5.10,
// tests-T13). The looptest.Fixture drives real chat Runs; an external endpoint on
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
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation/externalconversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// externalLoopStore is what the loop and the external workers need of one
// store.
type externalLoopStore interface {
	models.KernelStore
	models.RunProgressAcknowledgementStore
	models.ExternalConversationReplyStore
}

// externalLoopFixture is a looptest.Fixture whose chats arrive through an external
// endpoint of the Run's owner.
type externalLoopFixture struct {
	*looptest.Fixture
	ext      externalLoopStore
	adapters *skill.Catalog
	endpoint *models.ExternalConversationEndpoint
	host     *externalconversationtest.StreamChatHost
	progress *externalconversation.RunProgressAcknowledgementWorker
	replies  *externalconversation.ReplyWorker
	delivery *externalconversation.DeliveryWorker
}

// externalChat is one inbound provider message and the Run answering it.
type externalChat struct {
	conversation *models.Conversation
	inbound      *models.ChannelMessage
	run          *models.AgentRun
	item         *models.ExternalConversationInboxItem
}

// newExternalLoopFixture creates the endpoint (owned by finn, or by Team
// "team" looptest.WithTeam) and moves the loop fixture into the endpoint's scope.
// renderer is the progress worker's acknowledgement renderer; nil refuses to
// render.
func newExternalLoopFixture(t *testing.T, store models.KernelStore, provider string, operations []capability.ConversationDeliveryOperation, renderer agentrun.RunProgressAcknowledgementRenderer, opts ...looptest.Option) *externalLoopFixture {
	t.Helper()
	ext, ok := store.(externalLoopStore)
	if !ok {
		t.Fatalf("%T is not an external conversation store", store)
	}
	f := &externalLoopFixture{Fixture: looptest.NewFixture(t, store, opts...), ext: ext, host: &externalconversationtest.StreamChatHost{Messages: map[string]string{}}}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: looptest.FixtureAgent}
	if len(f.Config.Team) > 0 {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}
	}
	f.adapters, f.endpoint = externalconversationtest.EndpointFixtureOwnedBy(t, f.Ctx, ext, provider, operations, owner)
	// The endpoint fixture fixes the scope ({tenant, <provider>}); the
	// fixture Skill is bound there too so governed calls resolve.
	f.Scope = f.endpoint.Scope
	if err := f.Catalog.Bind(f.Ctx, &skill.Binding{
		ID: looptest.FixtureBindingID, Scope: skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, DeploymentID: looptest.FixtureAgent,
		SkillID: looptest.FixtureSkillID, SkillVersion: looptest.FixtureSkillVer,
		AllowedActions: []string{"lookup", "list_items", "update_status", "send", "explode"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if renderer == nil {
		renderer = externalconversation.RunProgressAcknowledgementRendererFunc(func(context.Context, agentrun.RunProgressAcknowledgementRequest) (string, error) {
			return "", errors.New("this scenario renders no progress acknowledgement")
		})
	}
	var err error
	if f.progress, err = externalconversation.NewRunProgressAcknowledgementWorker(ext, f.adapters, renderer, externalconversation.RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	}, models.WithClock(f.Clock.Now)); err != nil {
		t.Fatal(err)
	}
	if f.replies, err = externalconversation.NewReplyWorker(ext, f.adapters, models.WithClock(f.Clock.Now)); err != nil {
		t.Fatal(err)
	}
	if f.delivery, err = externalconversation.NewDeliveryWorker(ext, f.adapters, f.host, externalconversation.DeliveryWorkerConfig{WorkerID: "delivery"}, models.WithClock(f.Clock.Now)); err != nil {
		t.Fatal(err)
	}
	return f
}

// startExternal posts text from the provider into a new external-origin
// conversation, schedules its Run and records the applied inbox item.
func (f *externalLoopFixture) startExternal(text string) externalChat {
	f.T.Helper()
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: f.endpoint.Owner, Title: "Provider thread",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: f.endpoint.ID, Version: f.endpoint.Revision},
		IdempotencyKey: "external-loop-chat",
	})
	if err != nil {
		f.T.Fatalf("create conversation: %v", err)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: looptest.FixtureUser}, Intent: models.MessageIntentQuestion,
		Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "external-loop-inbound",
	})
	if err != nil {
		f.T.Fatalf("post inbound: %v", err)
	}
	run := f.ChatRun(conversation, posted.Message)
	now := f.Clock.Now()
	item := &models.ExternalConversationInboxItem{
		ID: "external-loop-inbox", Scope: f.Scope, EndpointID: f.endpoint.ID,
		EndpointRevision: f.endpoint.Revision, Adapter: f.endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "external-loop-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "conversation", ExternalThreadID: "thread", ExternalMessageID: "message",
			ExternalParticipantID: looptest.FixtureUser, Text: text, OrderingKey: "conversation:message", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID, RunID: run.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := f.ext.ReceiveExternalConversationEvent(f.Ctx, item); err != nil || replayed {
		f.T.Fatalf("store inbox item: replayed=%t err=%v", replayed, err)
	}
	return externalChat{conversation: conversation, inbound: posted.Message, run: run, item: item}
}

// commentary runs the progress worker's commentary pass (commentary, answer
// stream, thread status). It returns an error instead of failing, so model
// rules can call it from inside a step.
func (f *externalLoopFixture) commentary() ([]*models.ExternalConversationDelivery, error) {
	return f.progress.ProcessCommentaryScope(f.Ctx, f.Scope)
}

func (f *externalLoopFixture) mustCommentary() []*models.ExternalConversationDelivery {
	f.T.Helper()
	deliveries, err := f.commentary()
	if err != nil {
		f.T.Fatalf("commentary pass: %v", err)
	}
	return deliveries
}

// deliverAll drains the outbox through the fake provider chat.
func (f *externalLoopFixture) deliverAll() error {
	for range 20 {
		delivery, err := f.delivery.ProcessOne(f.Ctx, f.Scope)
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
	f.T.Helper()
	if err := f.deliverAll(); err != nil {
		f.T.Fatal(err)
	}
}

func (f *externalLoopFixture) reply() []*models.ExternalConversationDelivery {
	f.T.Helper()
	deliveries, err := f.replies.ProcessScope(f.Ctx, f.Scope, 10)
	if err != nil {
		f.T.Fatalf("reply pass: %v", err)
	}
	return deliveries
}

// outbox is every delivery of the endpoint whose correlation kind is kind
// ("" = all).
func (f *externalLoopFixture) outbox(kind string) []*models.ExternalConversationDelivery {
	f.T.Helper()
	deliveries, err := f.ext.ListExternalConversationDeliveries(f.Ctx, models.ExternalConversationDeliveryFilter{Scope: f.Scope, EndpointID: f.endpoint.ID, Limit: 100})
	if err != nil {
		f.T.Fatal(err)
	}
	var out []*models.ExternalConversationDelivery
	for _, delivery := range deliveries {
		if kind == "" || delivery.Correlation != nil && delivery.Correlation.Kind == kind {
			out = append(out, delivery)
		}
	}
	return out
}

// replyDelivery is the delivery of the reply to the inbox item: the one
// whose idempotency key names the item, nil when there is none.
func (f *externalLoopFixture) replyDelivery(itemID string) *models.ExternalConversationDelivery {
	f.T.Helper()
	var found *models.ExternalConversationDelivery
	for _, delivery := range f.outbox("") {
		if delivery.IdempotencyKey != "external-conversation-reply-delivery:"+itemID {
			continue
		}
		if found != nil {
			f.T.Fatalf("two reply deliveries for item %s: %s and %s", itemID, found.ID, delivery.ID)
		}
		found = delivery
	}
	return found
}

func (f *externalLoopFixture) operations() []capability.ConversationDeliveryOperation {
	out := make([]capability.ConversationDeliveryOperation, 0, len(f.host.Requests))
	for _, request := range f.host.Requests {
		out = append(out, request.Delivery.Operation)
	}
	return out
}

// eachExternalStore runs test on every matrix store that also carries the
// external conversation tables.
func eachExternalStore(t *testing.T, prefix string, test func(t *testing.T, store models.KernelStore)) {
	t.Helper()
	looptest.EachStore(t, prefix, func(t *testing.T, store models.KernelStore) {
		if _, ok := store.(externalLoopStore); !ok {
			t.Skipf("%T has no external conversation store", store)
		}
		test(t, store)
	})
}

// lastAgentMessage is the newest channel message sent by an Agent with intent.
func lastAgentMessage(messages []*models.ChannelMessage, intent models.ConversationMessageIntent) *models.ChannelMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Sender.Type == models.ConversationParticipantAgent && messages[i].Intent == intent {
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
	eachExternalStore(t, "external_narration", func(t *testing.T, store models.KernelStore) {
		f := newExternalLoopFixture(t, store, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, nil, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act(narration, looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Issue 7 is open.") }},
		))
		chat := f.startExternal("Is issue 7 open?")
		f.MustStatus(f.DriveToRest(chat.run.ID, 4), models.AgentRunStatusCompleted)

		steps := f.Steps(chat.run.ID)
		if len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if steps[0].OutputSummary != narration {
			t.Fatalf("step 1 outputSummary = %q, want %q", steps[0].OutputSummary, narration)
		}
		if steps[1].OutputSummary != "" {
			t.Fatalf("the final step has narration %q", steps[1].OutputSummary)
		}
		narrations := f.Events(chat.run.ID, activity.EventTurnNarration)
		if len(narrations) != 1 || narrations[0].Summary != narration || narrations[0].TurnID != steps[0].ID {
			t.Fatalf("turn.narration events = %#v", narrations)
		}
		if commentary := f.Events(chat.run.ID, "turn.commentary"); len(commentary) != 0 {
			t.Fatalf("narration became commentary: %#v", commentary)
		}

		for range 2 {
			f.mustCommentary()
		}
		if forwarded := f.outbox("run_commentary"); len(forwarded) != 0 {
			t.Fatalf("narration was forwarded: %#v", forwarded)
		}
		messages := f.Messages(chat.conversation.ID)
		for _, message := range messages {
			if strings.Contains(message.Content, narration) {
				t.Fatalf("narration reached the channel: %s", looptest.DescribeMessages(messages))
			}
		}
		if answer := lastAgentMessage(messages, models.MessageIntentAnswer); answer == nil || answer.Content != "Issue 7 is open." || len(messages) != 2 {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
		}
	})
}

// TestLoopReportProgressStaysCommentaryAndReachesSlack: turn_report_progress
// writes turn.commentary, and the connector forwards it once as a channel
// message and a Slack delivery, under the member's name in a Team.
func TestLoopReportProgressStaysCommentaryAndReachesSlack(t *testing.T) {
	const update = "Found the failing health check; fixing it now."
	for _, team := range []bool{false, true} {
		name, speaker := "agent", looptest.FixtureAgent
		var opts []looptest.Option
		if team {
			name, speaker = "team_member", "agent-a"
			opts = append(opts, looptest.WithTeam("agent-a"))
		}
		t.Run(name, func(t *testing.T) {
			eachExternalStore(t, "external_progress", func(t *testing.T, store models.KernelStore) {
				f := newExternalLoopFixture(t, store, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, nil, append(opts, looptest.WithRules(
					&looptest.ModelRule{Name: "progress", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
						if !v.HasTool(string(models.ControlToolReportProgress)) {
							t.Errorf("turn_report_progress is not offered: %s", v.Describe())
						}
						return looptest.Act("", looptest.Call(string(models.ControlToolReportProgress), map[string]any{"summary": update}))
					}},
					&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("The health check is fixed.") }},
				))...)
				chat := f.startExternal("Why is the deploy failing?")
				f.MustStatus(f.DriveToRest(chat.run.ID, 4), models.AgentRunStatusCompleted)

				commentary := f.Events(chat.run.ID, "turn.commentary")
				if len(commentary) != 1 || commentary[0].Summary != update || commentary[0].AgentID != speaker {
					t.Fatalf("turn.commentary events = %#v", commentary)
				}
				if entries := f.ToolEntries(chat.run.ID); len(entries) != 1 || entries[0].State != models.ToolEntryCompleted {
					t.Fatalf("progress tool entries = %v", f.EntryTypes(chat.run.ID))
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
				message, err := f.ext.GetChannelMessage(f.Ctx, f.Scope, chat.conversation.ID, forwarded[0].ChannelMessageID)
				if err != nil || message == nil || message.Content != update || message.Sender.ID != speaker || message.Intent != models.MessageIntentAcknowledgment {
					t.Fatalf("forwarded message = %#v, %v", message, err)
				}
				if answer := lastAgentMessage(f.Messages(chat.conversation.ID), models.MessageIntentAnswer); answer == nil || answer.Content != "The health check is fixed." || answer.Sender.ID != speaker {
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
func previewRule(name, attempt, text string, reply func(looptest.StepView) models.ModelStepResult, after func(context.Context) error) *looptest.ModelRule {
	return &looptest.ModelRule{Name: name, Times: 1, Reply: reply, Before: func(ctx context.Context, _ looptest.StepView) error {
		if !models.HasTurnAnswerPreview(ctx) {
			return errors.New("the loop installed no answer preview observer for the step")
		}
		if err := models.ReportTurnAnswerPreview(ctx, models.TurnAnswerPreview{AttemptID: attempt, Sequence: 1, Text: text}); err != nil {
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
	looptest.EachStore(t, "external_preview", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			previewRule("step-1", "attempt-1", "Checking…", func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking issue 7.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}, nil),
			previewRule("step-2", "attempt-2", "Issue is open", func(looptest.StepView) models.ModelStepResult { return looptest.Say("Issue 7 is open.") }, nil),
		))
		conversation, run := f.StartChat("Is issue 7 open?")
		f.MustStatus(f.DriveToRest(run.ID, 4), models.AgentRunStatusCompleted)

		frames := f.PreviewFrames(run.ID)
		want := []looptest.PreviewFrame{
			{Attempt: "attempt-1", Sequence: "1", Text: "Checking…"},
			{Attempt: "attempt-1", Sequence: "2", Reset: true},
			{Attempt: "attempt-2", Sequence: "1", Text: "Issue is open"},
		}
		if fmt.Sprint(frames) != fmt.Sprint(want) {
			t.Fatalf("preview frames = %v, want %v", frames, want)
		}
		steps := f.Steps(run.ID)
		events := f.Events(run.ID, "turn.answer_preview")
		if len(steps) != 2 || events[0].TurnID != steps[0].ID || events[1].TurnID != steps[0].ID || events[2].TurnID != steps[1].ID {
			t.Fatalf("preview frames are not on their steps: steps=%d events=%#v", len(steps), events)
		}
		messages := f.Messages(conversation.ID)
		answer := lastAgentMessage(messages, models.MessageIntentAnswer)
		if answer == nil || answer.Content != "Issue 7 is open." || len(messages) != 2 {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
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
	eachExternalStore(t, "external_stream", func(t *testing.T, store models.KernelStore) {
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
		}, nil, looptest.WithRules(
			previewRule("step-1", "attempt-1", firstPreview, func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking issue 7.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "7"}))
			}, func(context.Context) error { return stream(capability.ConversationDeliveryMessageSend) }),
			&looptest.ModelRule{Name: "step-2", Times: 1,
				Before: func(ctx context.Context, _ looptest.StepView) error {
					// The step-1 reset is the newest preview: nothing is posted
					// and the streamed message keeps its text.
					f.Clock.Advance(5 * time.Second)
					if err := stream(); err != nil {
						return err
					}
					if len(f.host.Messages) != 1 || f.host.Messages["chat-1"] != firstPreview {
						return fmt.Errorf("chat after the reset = %#v", f.host.Messages)
					}
					if err := models.ReportTurnAnswerPreview(ctx, models.TurnAnswerPreview{AttemptID: "attempt-2", Sequence: 1, Text: secondPreview}); err != nil {
						return err
					}
					return stream(capability.ConversationDeliveryMessageUpdate)
				},
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say(finalAnswer) }},
		))
		chat := f.startExternal("Is issue 7 open?")
		f.MustStatus(f.DriveToRest(chat.run.ID, 4), models.AgentRunStatusCompleted)
		if len(f.host.Messages) != 1 || f.host.Messages["chat-1"] != secondPreview {
			t.Fatalf("chat before the reply = %#v", f.host.Messages)
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
		final := f.replyDelivery(chat.item.ID)
		if final == nil || final.Operation != capability.ConversationDeliveryMessageUpdate || final.Parameters["providerMessageId"] != "chat-1" ||
			final.Status != models.ExternalConversationDeliveryDelivered {
			t.Fatalf("committed reply = %#v", final)
		}
		answer := lastAgentMessage(f.Messages(chat.conversation.ID), models.MessageIntentAnswer)
		if answer == nil || answer.Content != finalAnswer || final.ChannelMessageID != answer.ID {
			t.Fatalf("committed reply message = %#v, answer %#v", final, answer)
		}
		want := []capability.ConversationDeliveryOperation{
			capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryMessageUpdate,
		}
		if got := f.operations(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("provider operations = %v, want %v", got, want)
		}
		if len(f.host.Messages) != 1 || f.host.Messages["chat-1"] != finalAnswer {
			t.Fatalf("chat = %#v", f.host.Messages)
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
			eachExternalStore(t, "external_ack_"+c.provider, func(t *testing.T, store models.KernelStore) {
				var f *externalLoopFixture
				renderer := &lateProgressAckRenderer{}
				f = newExternalLoopFixture(t, store, c.provider, c.operations, renderer, looptest.WithRules(
					&looptest.ModelRule{Name: "ack",
						When: func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeAcknowledgement },
						Reply: func(v looptest.StepView) models.ModelStepResult {
							if c.toolCall {
								return looptest.Act("", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "7"}))
							}
							return looptest.Say(ack)
						}},
					&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("The deployment is healthy.") }},
				))
				renderer.inner = externalconversation.NewProgressAcknowledgementRenderer(looptest.FakeStepHost{ScriptedModel: f.Model, FakeHostTools: f.Tools}, looptest.FixtureAgent,
					looptest.FixtureDefinition, "1", nil, []string{"You are finn, the fixture Agent."}, models.WithClock(f.Clock.Now))
				chat := f.startExternal("Can you check the deployment?")
				f.Clock.Advance(time.Minute)

				deliveries, err := f.progress.ProcessScope(f.Ctx, f.Scope)
				acks := f.Model.Calls(models.ModelStepPurposeAcknowledgement)
				if len(acks) != 1 || len(f.Model.Calls("")) != 1 {
					t.Fatalf("model calls = %d (acknowledgements %d), want exactly one acknowledgement", len(f.Model.Calls("")), len(acks))
				}
				req := acks[0]
				if req.SessionID != "" || len(req.Tools) != 0 || req.RunID != chat.run.ID || req.AgentID != looptest.FixtureAgent || len(req.Entries) != 1 {
					t.Fatalf("acknowledgement request is not a sessionless tool-free step: %s session=%q run=%q", looptest.StepView{Req: req}.Describe(), req.SessionID, req.RunID)
				}
				// The chat Run names its lane session from creation; the
				// acknowledgement must not have created it, nor any step row.
				current := f.MustRun(chat.run.ID)
				if session, err := f.Raw.(models.AgentSessionStore).GetAgentSession(f.Ctx, f.Scope, current.SessionID); err == nil && session != nil {
					t.Fatalf("the acknowledgement created session %s", session.ID)
				}
				if steps := f.Steps(chat.run.ID); len(steps) != 0 {
					t.Fatalf("the acknowledgement wrote %d step rows", len(steps))
				}
				acknowledgements := f.outbox("run_acknowledgement")
				messages := f.Messages(chat.conversation.ID)
				if c.toolCall {
					if err == nil || !strings.Contains(err.Error(), "actionable progress acknowledgement") || len(deliveries) != 0 || len(acknowledgements) != 0 || len(messages) != 1 {
						t.Fatalf("a tool-call acknowledgement was accepted: deliveries=%#v err=%v channel=%s", deliveries, err, looptest.DescribeMessages(messages))
					}
				} else {
					if err != nil || len(deliveries) != 1 || deliveries[0].Operation != c.want || len(acknowledgements) != 1 {
						t.Fatalf("acknowledgement deliveries = %#v, %v", deliveries, err)
					}
					switch c.want {
					case capability.ConversationDeliveryTypingIndicator:
						if deliveries[0].Parameters["status"] != ack || len(messages) != 1 {
							t.Fatalf("native status = %#v, channel %s", deliveries[0].Parameters, looptest.DescribeMessages(messages))
						}
					default:
						posted := lastAgentMessage(messages, models.MessageIntentAcknowledgment)
						if posted == nil || posted.Content != ack || posted.Sender.ID != looptest.FixtureAgent || len(messages) != 2 {
							t.Fatalf("text acknowledgement channel = %s", looptest.DescribeMessages(messages))
						}
					}
				}

				// The Run itself then works through the loop as usual: one step,
				// one session, and the acknowledgement left no entry in it.
				f.MustStatus(f.DriveToRest(chat.run.ID, 4), models.AgentRunStatusCompleted)
				if steps := f.Steps(chat.run.ID); len(steps) != 1 {
					t.Fatalf("steps = %d, want 1", len(steps))
				}
				if got := f.Model.Calls(models.ModelStepPurposeStep); len(got) != 1 {
					t.Fatalf("chat steps = %d, want 1", len(got))
				}
				for _, entry := range f.Entries(chat.run.ID) {
					if entry.Type == models.SessionEntryAssistant && entry.Assistant != nil && strings.Contains(entry.Assistant.Text, ack) {
						t.Fatalf("the acknowledgement reached the session: %v", f.EntryTypes(chat.run.ID))
					}
				}
			})
		})
	}
}

// lateProgressAckRenderer lets the renderer be built after the fixture (it
// needs the fixture's scripted model).
type lateProgressAckRenderer struct {
	inner agentrun.RunProgressAcknowledgementRenderer
}

func (r *lateProgressAckRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request agentrun.RunProgressAcknowledgementRequest) (string, error) {
	if r.inner == nil {
		return "", errors.New("progress acknowledgement renderer is not built yet")
	}
	return r.inner.RenderRunProgressAcknowledgement(ctx, request)
}
