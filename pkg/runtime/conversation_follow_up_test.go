package runtime

import (
	"context"
	"testing"
	"time"
)

type conversationFollowUpFixture struct {
	store        *SQLiteStore
	service      *ConversationService
	scheduler    *ConversationRunScheduler
	conversation *Conversation
}

func newConversationFollowUpFixture(t *testing.T) *conversationFollowUpFixture {
	t.Helper()
	store := newTestKernelStore(t)
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "follow-ups"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Shopping assistant", IdempotencyKey: "follow-up-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationFollowUpFixture{store: store, service: service, scheduler: mustConversationRunScheduler(t, store), conversation: conversation}
}

func (f *conversationFollowUpFixture) post(t *testing.T, sender, content string) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: sender}, Intent: MessageIntentQuestion,
		Content: content, RequiresResponse: true, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: "message:" + content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func (f *conversationFollowUpFixture) schedule(t *testing.T, message *ChannelMessage) *AgentRun {
	t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

func (f *conversationFollowUpFixture) run(t *testing.T, id string) *AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

func (f *conversationFollowUpFixture) conversationRuns(t *testing.T) []*AgentRun {
	t.Helper()
	runs, err := f.store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: f.conversation.Scope, Kind: RunKindConversation, Owner: &f.conversation.Owner})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (f *conversationFollowUpFixture) transition(t *testing.T, run *AgentRun, status AgentRunStatus, wake *WakeCondition) *AgentRun {
	t.Helper()
	next, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
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
	for _, message := range []*ChannelMessage{second, third} {
		if delivered := f.schedule(t, message); delivered.ID != run.ID {
			t.Fatalf("message %s started a parallel Run %s", message.ID, delivered.ID)
		}
	}
	if runs := f.conversationRuns(t); len(runs) != 1 {
		t.Fatalf("conversation Runs = %d, want 1", len(runs))
	}
	run = f.run(t, run.ID)
	followUps := conversationRunFollowUps(run)
	if run.Status != AgentRunStatusQueued || len(followUps) != 2 || followUps[0].MessageID != second.ID || followUps[1].MessageID != third.ID ||
		run.PendingInterventions[0].Instruction != second.Content || run.PendingInterventions[1].Actor != (ActivityActor{Type: "user", ID: "user"}) {
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
	run = f.transition(t, f.transition(t, f.run(t, run.ID), AgentRunStatusRunning, nil), AgentRunStatusCompleted, nil)
	fourth := f.post(t, "user", "Now track the delivery")
	next := f.schedule(t, fourth)
	if next.ID == run.ID || next.Context[conversationRunContextTriggerID] != fourth.ID || len(next.PendingInterventions) != 0 {
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
		status AgentRunStatus
		wake   *WakeCondition
		want   AgentRunStatus
	}{
		{status: AgentRunStatusRunning, want: AgentRunStatusRunning},
		{status: AgentRunStatusSleeping, wake: &WakeCondition{Type: "timer", WakeAt: &wakeAt}, want: AgentRunStatusQueued},
		{status: AgentRunStatusWaitingForEvent, wake: &WakeCondition{Type: "human_intervention", Reference: "credential-request"}, want: AgentRunStatusWaitingForEvent},
		{status: AgentRunStatusWaitingForApproval, wake: &WakeCondition{Type: "approval", Reference: "approval-1"}, want: AgentRunStatusWaitingForApproval},
		{status: AgentRunStatusPaused, want: AgentRunStatusPaused},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			f := newConversationFollowUpFixture(t)
			run := f.schedule(t, f.post(t, "user", "Buy the hub"))
			run = f.transition(t, run, AgentRunStatusRunning, nil)
			if test.status != AgentRunStatusRunning {
				run = f.transition(t, run, test.status, test.wake)
			}
			message := f.post(t, "user", "Actually use the other card")
			delivered := f.schedule(t, message)
			if delivered.ID != run.ID || delivered.Status != test.want || !conversationRunHasFollowUp(delivered, message.ID) {
				t.Fatalf("delivered Run = %#v", delivered)
			}
			if test.want == test.status && (test.wake == nil) != (delivered.WakeCondition == nil) {
				t.Fatalf("wake condition changed: %#v", delivered.WakeCondition)
			}
			if test.want == AgentRunStatusQueued && delivered.WakeCondition != nil {
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
	run = f.transition(t, run, AgentRunStatusRunning, nil)
	run, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
		FailureExplanation: &RunFailureExplanation{Kind: "execution", Code: "action_failed", Message: "checkout failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := f.post(t, "user", "Try again with the other card")
	next := f.schedule(t, message)
	if next.ID == run.ID || next.Context[conversationRunContextTriggerID] != message.ID {
		t.Fatalf("failing Run absorbed a new request: %#v", next)
	}
}

func TestExternalInboxFollowUpItemDefersToTriggerItem(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "inbox"}
	run := &AgentRun{Scope: scope, Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "trigger"}}
	run.PendingInterventions = []AgentRunIntervention{{
		ID:                  conversationFollowUpInterventionID(scope, "follow-up"),
		ConversationMessage: &ConversationMessageDelivery{ConversationID: "chat", MessageID: "follow-up"},
	}, {
		ID: "operator-guidance", Instruction: "Keep going",
	}}
	if externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "trigger"}, run) {
		t.Fatal("trigger item treated as follow-up")
	}
	if !externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "follow-up"}, run) {
		t.Fatal("follow-up item would duplicate the Run's reply")
	}
	if externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "unrelated"}, run) {
		t.Fatal("unrelated item treated as follow-up")
	}
}

// Follow-up delivery on the run loop: the scheduler records each message as a
// message-backed intervention (bumping the Run's revision), and the Run's
// next step ingests it into its own session through catch-up.

func (f *chatLoopFixture) scheduler() *ConversationRunScheduler {
	f.t.Helper()
	scheduler, err := NewConversationRunScheduler(f.store, f.store, ConversationRunSchedulerConfig{})
	if err != nil {
		f.t.Fatal(err)
	}
	return scheduler
}

func (f *chatLoopFixture) schedule(message *ChannelMessage) *AgentRun {
	f.t.Helper()
	result, _, err := f.scheduler().ScheduleMessage(f.ctx, f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		f.t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

// startMember creates and claims agentID's member Run answering trigger in a
// Team chat, as the routing Run would (teamMemberRunRequest).
func (f *chatLoopFixture) startMember(trigger *ChannelMessage, agentID string) *AgentRun {
	f.t.Helper()
	request := conversationAgentRunRequest(f.conversation, trigger)
	request.AssignedAgentID = agentID
	request.ConcurrencyKey = conversationRunLane(f.conversation.ID, "", agentID)
	request.SessionID = conversationSessionID(f.conversation.Scope, request.ConcurrencyKey)
	request.IdempotencyKey = "member-" + agentID + "-" + trigger.ID
	commands := NewRunCommandService(f.store)
	commands.now = func() time.Time { return f.now }
	result, err := commands.CreateAgentRun(f.ctx, request)
	if err != nil {
		f.t.Fatalf("create member run: %v", err)
	}
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: chatLoopWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != result.Run.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, result.Run.ID)
	}
	return claimed
}

// bindMember is bind for agentID's member Run.
func (f *chatLoopFixture) bindMember(run *AgentRun, agentID string, producer StepProducer) *TurnRunnerBinding {
	f.t.Helper()
	binding := &TurnRunnerBinding{
		DeploymentID: agentID, DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(ModelStepProviderAnthropic), Model: "chat-model",
		Producer: producer, Preparers: []StepPreparer{newRunStartPreparer(f.store, f.store)},
		Authority: ControlAuthority{AgentID: agentID, CanAskUser: true, CanReact: true},
	}
	if err := f.runner.bindConversationLoop(f.ctx, run, binding, ConversationLoopStores{Sessions: f.store, Actions: f.actions, Activity: f.store}); err != nil {
		f.t.Fatalf("bind %s: %v", agentID, err)
	}
	return binding
}

func (f *chatLoopFixture) eventTypes(runID string) []string {
	f.t.Helper()
	events, err := f.store.ListActivity(f.ctx, ActivityFilter{Scope: f.conversation.Scope, RunID: runID, Limit: 500})
	if err != nil {
		f.t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
	}
	return types
}

func countFollowUpEntries(entries []*SessionEntry, messageID string) int {
	count := 0
	for _, entry := range entries {
		if entry.Type == SessionEntryUser && entry.User != nil && entry.User.MessageID == messageID {
			count++
		}
	}
	return count
}

func expectNoGuidanceChanged(t *testing.T, f *chatLoopFixture, runID string) {
	t.Helper()
	for _, eventType := range f.eventTypes(runID) {
		if eventType == "turn.guidance_changed" {
			t.Fatalf("run %s recorded turn.guidance_changed: %v", runID, f.eventTypes(runID))
		}
	}
}

func TestFollowUpsDeliveredByTheSchedulerAppendToTheSessionOnce(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("first", "user-1", "Find the cheapest USB-C hub")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)

		second := f.post("second", "user-1", "It must have HDMI")
		third := f.post("third", "user-1", "Use the card ending 1001")
		for _, message := range []*ChannelMessage{second, third} {
			before := f.run(run.ID).Revision
			if delivered := f.schedule(message); delivered.ID != run.ID || delivered.Revision <= before {
				t.Fatalf("follow-up %s: run %s revision %d (was %d)", message.ID, delivered.ID, delivered.Revision, before)
			}
		}
		// A replayed delivery records nothing new.
		if replayed := f.schedule(third); len(replayed.PendingInterventions) != 2 {
			t.Fatalf("replay duplicated the follow-up: %#v", replayed.PendingInterventions)
		}

		f.prepare(run, binding, 2)
		f.prepare(run, binding, 3)
		chatLoopExpectShape(t, f.path(binding.Session.ID),
			"user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+second.ID, "user:"+third.ID)
		if got := f.run(run.ID).ConsumedInterventions; got != 2 {
			t.Fatalf("consumed interventions = %d, want 2", got)
		}
		expectNoGuidanceChanged(t, f, run.ID)
	})
}

// A message from another person is not a follow-up: it starts its own Run.
func TestFollowUpFromAnotherPersonStartsItsOwnRun(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("first", "user-1", "Find the cheapest USB-C hub")
		run := f.startRun(trigger)
		other := f.post("other", "user-2", "What is on my calendar?")
		started := f.schedule(other)
		if started.ID == run.ID || started.Context[conversationRunContextTriggerID] != other.ID || len(f.run(run.ID).PendingInterventions) != 0 {
			t.Fatalf("another person's message was absorbed: %#v", started)
		}
	})
}

// followUpDuringStepProducer delivers a follow-up while its first step runs,
// then answers each step with the next scripted text.
type followUpDuringStepProducer struct {
	chatLoopProducer
	during func()
}

func (p *followUpDuringStepProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	if p.during != nil {
		during := p.during
		p.during = nil
		during()
	}
	return p.chatLoopProducer.Produce(ctx, in)
}

// A message delivered while a step runs bumps the Run's revision, so the
// answer drafted without it is not posted; the next step sees the follow-up
// in the session and its answer is the one posted.
func TestFollowUpDuringStepSupersedesItsAnswer(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("first", "user-1", "Find a USB-C hub")
		run := f.startRun(trigger)
		var second *ChannelMessage
		producer := &followUpDuringStepProducer{
			chatLoopProducer: chatLoopProducer{answers: []string{"Here is a hub without HDMI.", "Here is a hub with HDMI."}},
			during: func() {
				second = f.post("second", "user-1", "Only ones with HDMI")
				f.schedule(second)
			},
		}
		binding := f.bind(run, producer)
		exit := f.drive(run, binding)
		if exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 2 {
			t.Fatalf("exit = %s %s after %d steps", exit.Reason, exit.Run.Status, len(producer.paths))
		}
		if countFollowUpEntries(producer.paths[1], second.ID) != 1 || countFollowUpEntries(producer.paths[0], second.ID) != 0 {
			t.Fatal("the second step did not render the follow-up exactly once")
		}
		messages := f.agentMessages()
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
	forChatLoopStores(t, chatLoopOptions{team: true}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Plan the launch.")
		members := []string{"agent-1", "agent-2"}
		runs := map[string]*AgentRun{}
		bindings := map[string]*TurnRunnerBinding{}
		revisions := map[string]int64{}
		for _, agentID := range members {
			runs[agentID] = f.startMember(trigger, agentID)
			bindings[agentID] = f.bindMember(runs[agentID], agentID, &chatLoopProducer{})
			f.prepare(runs[agentID], bindings[agentID], 1)
			revisions[agentID] = f.run(runs[agentID].ID).Revision
		}
		if bindings["agent-1"].Session.ID == bindings["agent-2"].Session.ID {
			t.Fatal("two members share one session")
		}

		followUp := f.post("follow-up", "user-1", "Keep it under a week.")
		f.schedule(followUp)
		for _, agentID := range members {
			current := f.run(runs[agentID].ID)
			if !conversationRunHasFollowUp(current, followUp.ID) || current.Revision <= revisions[agentID] {
				t.Fatalf("%s: follow-up not delivered (revision %d, was %d): %#v", agentID, current.Revision, revisions[agentID], current.PendingInterventions)
			}
		}

		for _, agentID := range members {
			run, binding := runs[agentID], bindings[agentID]
			f.prepare(run, binding, 2)
			f.prepare(run, binding, 3)
			chatLoopExpectShape(t, f.path(binding.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+followUp.ID)
			if got := f.run(run.ID).ConsumedInterventions; got != 1 {
				t.Fatalf("%s consumed %d interventions", agentID, got)
			}
			expectNoGuidanceChanged(t, f, run.ID)
		}
		if runs, err := f.store.ListAgentRuns(f.ctx, AgentRunFilter{Scope: f.conversation.Scope, Kind: RunKindConversation, Owner: &f.conversation.Owner}); err != nil || len(runs) != len(members) {
			t.Fatalf("the follow-up started new work: %d runs, %v", len(runs), err)
		}
	})
}
