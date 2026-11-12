package runloop_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type conversationFollowUpFixture struct {
	store        *sqlite.Store
	service      *conversation.ConversationService
	scheduler    *conversationrun.ConversationRunScheduler
	conversation *models.Conversation
}

func newConversationFollowUpFixture(t *testing.T) *conversationFollowUpFixture {
	t.Helper()
	store := conversationtest.NewStore(t)
	service := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, models.Scope{Kind: "tenant", ID: "follow-ups"})
	conversation, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "follow-ups"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-42"},
		Title: "Shopping assistant", IdempotencyKey: "follow-up-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationFollowUpFixture{store: store, service: service, scheduler: conversationruntest.NewScheduler(t, store), conversation: conversation}
}

func (f *conversationFollowUpFixture) post(t *testing.T, sender, content string) *models.ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.service.PostChannelMessage(t.Context(), conversation.PostChannelMessageRequest{
		Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: sender}, Intent: models.MessageIntentQuestion,
		Content: content, RequiresResponse: true, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		IdempotencyKey: "message:" + content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func (f *conversationFollowUpFixture) schedule(t *testing.T, message *models.ChannelMessage) *models.AgentRun {
	t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

func (f *conversationFollowUpFixture) run(t *testing.T, id string) *models.AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

func (f *conversationFollowUpFixture) conversationRuns(t *testing.T) []*models.AgentRun {
	t.Helper()
	runs, err := f.store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: f.conversation.Scope, Kind: models.RunKindConversation, Owner: &f.conversation.Owner})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (f *conversationFollowUpFixture) transition(t *testing.T, run *models.AgentRun, status models.AgentRunStatus, wake *models.WakeCondition) *models.AgentRun {
	t.Helper()
	next, _, err := activity.NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
	})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// Three rapid messages from one person in one lane produce one Run that holds
// the later two as follow-ups, in order (the session side is
// TestFollowUpsDeliveredByTheSchedulerAppendToTheSessionOnce).
func TestRapidConversationMessagesShareOneRunThatSeesAll(t *testing.T) {
	f := newConversationFollowUpFixture(t)
	first := f.post(t, "user", "Find the cheapest USB-C hub on Amazon")
	second := f.post(t, "user", "It must have HDMI")
	third := f.post(t, "user", "Use my card ending with 1001 to buy it")
	run := f.schedule(t, first)
	for _, message := range []*models.ChannelMessage{second, third} {
		if delivered := f.schedule(t, message); delivered.ID != run.ID {
			t.Fatalf("message %s started a parallel Run %s", message.ID, delivered.ID)
		}
	}
	if runs := f.conversationRuns(t); len(runs) != 1 {
		t.Fatalf("conversation Runs = %d, want 1", len(runs))
	}
	run = f.run(t, run.ID)
	followUps := models.ConversationRunFollowUps(run)
	if run.Status != models.AgentRunStatusQueued || len(followUps) != 2 || followUps[0].MessageID != second.ID || followUps[1].MessageID != third.ID ||
		run.PendingInterventions[0].Instruction != second.Content || run.PendingInterventions[1].Actor != (models.ActivityActor{Type: "user", ID: "user"}) {
		t.Fatalf("follow-ups were not delivered in order: %#v", run)
	}

	// Recovery replays every message without new Runs or duplicate delivery.
	reconciled, err := f.scheduler.ReconcileScope(t.Context(), f.conversation.Scope)
	if err != nil || reconciled.Scheduled != 0 || reconciled.Replayed != 3 {
		t.Fatalf("reconciliation = %#v, %v", reconciled, err)
	}
	if runs := f.conversationRuns(t); len(runs) != 1 || len(runs[0].PendingInterventions) != 2 {
		t.Fatalf("replay duplicated work: %#v", runs)
	}

	// A terminal Run never absorbs: the next message starts a new Run.
	run = f.transition(t, f.transition(t, f.run(t, run.ID), models.AgentRunStatusRunning, nil), models.AgentRunStatusCompleted, nil)
	fourth := f.post(t, "user", "Now track the delivery")
	next := f.schedule(t, fourth)
	if next.ID == run.ID || next.Context[models.ConversationRunTriggerIDContextKey] != fourth.ID || len(next.PendingInterventions) != 0 {
		t.Fatalf("terminal Run absorbed a new message: %#v", next)
	}
	if current := f.run(t, run.ID); len(current.PendingInterventions) != 2 {
		t.Fatalf("terminal Run changed: %#v", current)
	}
}

// Waiting Runs keep waiting on their approval, credential, browser handoff or
// pause; the message is still durable on the Run for its next Turn. Only a
// sleeping retry or schedule timer is cut short by fresh human input.
func TestConversationFollowUpDeliveryPreservesWaitingStates(t *testing.T) {
	wakeAt := time.Now().Add(time.Hour).UTC()
	for _, test := range []struct {
		status models.AgentRunStatus
		wake   *models.WakeCondition
		want   models.AgentRunStatus
	}{
		{status: models.AgentRunStatusRunning, want: models.AgentRunStatusRunning},
		{status: models.AgentRunStatusSleeping, wake: &models.WakeCondition{Type: "timer", WakeAt: &wakeAt}, want: models.AgentRunStatusQueued},
		{status: models.AgentRunStatusWaitingForEvent, wake: &models.WakeCondition{Type: "human_intervention", Reference: "credential-request"}, want: models.AgentRunStatusWaitingForEvent},
		{status: models.AgentRunStatusWaitingForApproval, wake: &models.WakeCondition{Type: "approval", Reference: "approval-1"}, want: models.AgentRunStatusWaitingForApproval},
		{status: models.AgentRunStatusPaused, want: models.AgentRunStatusPaused},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			f := newConversationFollowUpFixture(t)
			run := f.schedule(t, f.post(t, "user", "Buy the hub"))
			run = f.transition(t, run, models.AgentRunStatusRunning, nil)
			if test.status != models.AgentRunStatusRunning {
				run = f.transition(t, run, test.status, test.wake)
			}
			message := f.post(t, "user", "Actually use the other card")
			delivered := f.schedule(t, message)
			if delivered.ID != run.ID || delivered.Status != test.want || !runHasFollowUp(delivered, message.ID) {
				t.Fatalf("delivered Run = %#v", delivered)
			}
			if test.want == test.status && (test.wake == nil) != (delivered.WakeCondition == nil) {
				t.Fatalf("wake condition changed: %#v", delivered.WakeCondition)
			}
			if test.want == models.AgentRunStatusQueued && delivered.WakeCondition != nil {
				t.Fatalf("queued Run kept a stale timer: %#v", delivered.WakeCondition)
			}
			if len(f.conversationRuns(t)) != 1 {
				t.Fatal("waiting Run did not absorb the message")
			}
		})
	}
}

func TestConversationFollowUpSkipsRunExplainingItsFailure(t *testing.T) {
	f := newConversationFollowUpFixture(t)
	run := f.schedule(t, f.post(t, "user", "Buy the hub"))
	run = f.transition(t, run, models.AgentRunStatusRunning, nil)
	run, _, err := activity.NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
		FailureExplanation: &models.RunFailureExplanation{Kind: "execution", Code: "action_failed", Message: "checkout failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := f.post(t, "user", "Try again with the other card")
	next := f.schedule(t, message)
	if next.ID == run.ID || next.Context[models.ConversationRunTriggerIDContextKey] != message.ID {
		t.Fatalf("failing Run absorbed a new request: %#v", next)
	}
}

func TestExternalInboxFollowUpItemDefersToTriggerItem(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "inbox"}
	run := &models.AgentRun{Scope: scope, Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "chat", models.ConversationRunTriggerIDContextKey: "trigger"}}
	run.PendingInterventions = []models.AgentRunIntervention{{
		ID:                  models.ConversationFollowUpInterventionID(scope, "follow-up"),
		ConversationMessage: &models.ConversationMessageDelivery{ConversationID: "chat", MessageID: "follow-up"},
	}, {
		ID: "operator-guidance", Instruction: "Keep going",
	}}
	if conversationrun.ExternalInboxItemIsFollowUp(&models.ExternalConversationInboxItem{ChannelMessageID: "trigger"}, run) {
		t.Fatal("trigger item treated as follow-up")
	}
	if !conversationrun.ExternalInboxItemIsFollowUp(&models.ExternalConversationInboxItem{ChannelMessageID: "follow-up"}, run) {
		t.Fatal("follow-up item would duplicate the Run's reply")
	}
	if conversationrun.ExternalInboxItemIsFollowUp(&models.ExternalConversationInboxItem{ChannelMessageID: "unrelated"}, run) {
		t.Fatal("unrelated item treated as follow-up")
	}
}

// Follow-up delivery on the run loop: the scheduler records each message as a
// message-backed intervention (bumping the Run's revision), and the Run's
// next step ingests it into its own session through catch-up.

func countFollowUpEntries(entries []*models.SessionEntry, messageID string) int {
	count := 0
	for _, entry := range entries {
		if entry.Type == models.SessionEntryUser && entry.User != nil && entry.User.MessageID == messageID {
			count++
		}
	}
	return count
}

func expectNoGuidanceChanged(t *testing.T, f *runlooptest.ChatFixture, runID string) {
	t.Helper()
	for _, eventType := range f.EventTypes(runID) {
		if eventType == "turn.guidance_changed" {
			t.Fatalf("run %s recorded turn.guidance_changed: %v", runID, f.EventTypes(runID))
		}
	}
}

func TestFollowUpsDeliveredByTheSchedulerAppendToTheSessionOnce(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("first", "user-1", "Find the cheapest USB-C hub")
		run := f.StartRun(trigger)
		binding := f.Bind(run, &runlooptest.ChatProducer{})
		prepareChatStep(f, run, binding, 1)

		second := f.Post("second", "user-1", "It must have HDMI")
		third := f.Post("third", "user-1", "Use the card ending 1001")
		for _, message := range []*models.ChannelMessage{second, third} {
			before := f.Run(run.ID).Revision
			if delivered := f.Schedule(message); delivered.ID != run.ID || delivered.Revision <= before {
				t.Fatalf("follow-up %s: run %s revision %d (was %d)", message.ID, delivered.ID, delivered.Revision, before)
			}
		}
		// A replayed delivery records nothing new.
		if replayed := f.Schedule(third); len(replayed.PendingInterventions) != 2 {
			t.Fatalf("replay duplicated the follow-up: %#v", replayed.PendingInterventions)
		}

		prepareChatStep(f, run, binding, 2)
		prepareChatStep(f, run, binding, 3)
		runlooptest.ExpectChatShape(t, f.Path(binding.Session.ID),
			"user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+second.ID, "user:"+third.ID)
		if got := f.Run(run.ID).ConsumedInterventions; got != 2 {
			t.Fatalf("consumed interventions = %d, want 2", got)
		}
		expectNoGuidanceChanged(t, f, run.ID)
	})
}

// A message from another person is not a follow-up: it starts its own Run.
func TestFollowUpFromAnotherPersonStartsItsOwnRun(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("first", "user-1", "Find the cheapest USB-C hub")
		run := f.StartRun(trigger)
		other := f.Post("other", "user-2", "What is on my calendar?")
		started := f.Schedule(other)
		if started.ID == run.ID || started.Context[models.ConversationRunTriggerIDContextKey] != other.ID || len(f.Run(run.ID).PendingInterventions) != 0 {
			t.Fatalf("another person's message was absorbed: %#v", started)
		}
	})
}

// followUpDuringStepProducer delivers a follow-up while its first step runs,
// then answers each step with the next scripted text.
type followUpDuringStepProducer struct {
	runlooptest.ChatProducer
	during func()
}

func (p *followUpDuringStepProducer) Produce(ctx context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	if p.during != nil {
		during := p.during
		p.during = nil
		during()
	}
	return p.ChatProducer.Produce(ctx, in)
}

// A message delivered while a step runs bumps the Run's revision, so the
// answer drafted without it is not posted; the next step sees the follow-up
// in the session and its answer is the one posted.
func TestFollowUpDuringStepSupersedesItsAnswer(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("first", "user-1", "Find a USB-C hub")
		run := f.StartRun(trigger)
		var second *models.ChannelMessage
		producer := &followUpDuringStepProducer{
			ChatProducer: runlooptest.ChatProducer{Answers: []string{"Here is a hub without HDMI.", "Here is a hub with HDMI."}},
			during: func() {
				second = f.Post("second", "user-1", "Only ones with HDMI")
				f.Schedule(second)
			},
		}
		binding := f.Bind(run, producer)
		exit := f.Drive(run, binding)
		if exit.Run.Status != models.AgentRunStatusCompleted || len(producer.Paths) != 2 {
			t.Fatalf("exit = %s %s after %d steps", exit.Reason, exit.Run.Status, len(producer.Paths))
		}
		if countFollowUpEntries(producer.Paths[1], second.ID) != 1 || countFollowUpEntries(producer.Paths[0], second.ID) != 0 {
			t.Fatal("the second step did not render the follow-up exactly once")
		}
		messages := f.AgentMessages()
		if len(messages) != 1 || messages[0].Content != "Here is a hub with HDMI." {
			var contents []string
			for _, message := range messages {
				contents = append(contents, message.Content)
			}
			t.Fatalf("posted %q, want only the answer that saw the follow-up", contents)
		}
		expectNoGuidanceChanged(t, f, run.ID)
	})
}

// An unaddressed Team message from the person the members are answering goes
// to every working member as a follow-up, and each member's next step appends
// it to that member's own session.
func TestTeamFollowUpAppendsToEveryMemberSession(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{Team: true}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("trigger", "user-1", "Plan the launch.")
		members := []string{"agent-1", "agent-2"}
		runs := map[string]*models.AgentRun{}
		bindings := map[string]*agentrun.TurnRunnerBinding{}
		revisions := map[string]int64{}
		for _, agentID := range members {
			runs[agentID] = f.StartMember(trigger, agentID)
			bindings[agentID] = f.BindMember(runs[agentID], agentID, &runlooptest.ChatProducer{})
			prepareChatStep(f, runs[agentID], bindings[agentID], 1)
			revisions[agentID] = f.Run(runs[agentID].ID).Revision
		}
		if bindings["agent-1"].Session.ID == bindings["agent-2"].Session.ID {
			t.Fatal("two members share one session")
		}

		followUp := f.Post("follow-up", "user-1", "Keep it under a week.")
		f.Schedule(followUp)
		for _, agentID := range members {
			current := f.Run(runs[agentID].ID)
			if !runHasFollowUp(current, followUp.ID) || current.Revision <= revisions[agentID] {
				t.Fatalf("%s: follow-up not delivered (revision %d, was %d): %#v", agentID, current.Revision, revisions[agentID], current.PendingInterventions)
			}
		}

		for _, agentID := range members {
			run, binding := runs[agentID], bindings[agentID]
			prepareChatStep(f, run, binding, 2)
			prepareChatStep(f, run, binding, 3)
			runlooptest.ExpectChatShape(t, f.Path(binding.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+followUp.ID)
			if got := f.Run(run.ID).ConsumedInterventions; got != 1 {
				t.Fatalf("%s consumed %d interventions", agentID, got)
			}
			expectNoGuidanceChanged(t, f, run.ID)
		}
		if runs, err := f.Store.ListAgentRuns(f.Ctx, models.AgentRunFilter{Scope: f.Conversation.Scope, Kind: models.RunKindConversation, Owner: &f.Conversation.Owner}); err != nil || len(runs) != len(members) {
			t.Fatalf("the follow-up started new work: %d runs, %v", len(runs), err)
		}
	})
}

// runHasFollowUp reports whether messageID was delivered into run as a
// follow-up.
func runHasFollowUp(run *models.AgentRun, messageID string) bool {
	return slices.ContainsFunc(models.ConversationRunFollowUps(run), func(delivery models.ConversationMessageDelivery) bool {
		return delivery.MessageID == messageID
	})
}
