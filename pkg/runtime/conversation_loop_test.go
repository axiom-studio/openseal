package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// The conversation seams of the RunLoop (run-loop section 05 §7, §13 on
// SPEC §2.4), on the SQLite store. A chat Run's binding comes from the
// conversation turn runner, as the worker resolves it. Runs are driven
// through the real RunLoop with a recording model producer, or the seams are
// called the way the loop calls them (appendStepContext is the loop's
// phase 3).

const chatLoopWorker = "chat-loop-worker"

type chatLoopStore interface {
	finalDeliveryStore
	KernelStore
	RunCommandStore
}

// chatLoopActions is the governed call and approval store the seams read.
type chatLoopActions struct {
	mu        sync.Mutex
	calls     map[string]*ActionCall
	approvals map[string]*ApprovalCheckpoint
}

func (a *chatLoopActions) GetActionCall(_ context.Context, _ Scope, id string) (*ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

func (a *chatLoopActions) GetApproval(_ context.Context, _ Scope, id string) (*ApprovalCheckpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	approval, ok := a.approvals[id]
	if !ok {
		return nil, ErrApprovalNotFound
	}
	copied := *approval
	return &copied, nil
}

// chatLoopProducer is a model producer that records the session path each
// step renders from and answers with the next scripted text.
type chatLoopProducer struct {
	answers []string
	paths   [][]*SessionEntry
}

func (*chatLoopProducer) Kind() StepProducerKind { return StepProducerModel }

func (p *chatLoopProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	var entries []*SessionEntry
	if in.Path != nil {
		entries = append(entries, in.Path.Entries...)
	}
	p.paths = append(p.paths, entries)
	answer := "done"
	if len(p.answers) > 0 {
		answer, p.answers = p.answers[0], p.answers[1:]
	}
	return agentrun.StepOutput{Assistant: &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "chat-model", Text: answer, StopReason: ModelStopEnd,
	}}, nil
}

type chatLoopOptions struct {
	team          bool
	participation bool // RequireParticipationOptIn with participation off
}

type chatLoopFixture struct {
	t            *testing.T
	ctx          context.Context
	store        chatLoopStore
	service      *conversation.ConversationService
	conversation *Conversation
	runner       *ConversationRunTurnRunner
	actions      *chatLoopActions
	loop         *RunLoop
	team         bool
	now          time.Time
	// agents is the runner's AgentTurns resolver: the bare Agent binding of
	// each Run, before the chat seams are added.
	agents chatLoopAgentTurns
}

func forChatLoopStores(t *testing.T, options chatLoopOptions, test func(*testing.T, *chatLoopFixture)) {
	t.Helper()
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		test(t, newChatLoopFixture(t, base, options))
	})
}

func newChatLoopFixture(t *testing.T, base conversationtest.ThreadStartStore, options chatLoopOptions) *chatLoopFixture {
	t.Helper()
	store, ok := base.(chatLoopStore)
	if !ok {
		t.Fatalf("%T is not a kernel store with the session log", base)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "chat-loop-" + uuid.NewString()[:8]}
	service := conversation.NewConversationService(store)
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	if options.team {
		owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-1"}
	}
	seedTestSpace(t, store.(SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Chat loop", IdempotencyKey: "chat-loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewRunLoop(store, nil, nil, RunLoopConfig{
		PlanToolEntries: func(*agentrun.TurnRunnerBinding, *AgentSession, *AgentTurn, *SessionEntry, []agentrun.LoopToolCall) ([]*SessionEntry, error) {
			return nil, fmt.Errorf("chat loop tests make no tool calls")
		},
		PlainTextFinal: func(_ context.Context, cc agentrun.ControlContext, text string) (ControlResult, error) {
			return ControlResult{Kind: ControlFinal, Final: &agentrun.FinalAnswer{
				StepID: cc.Step.ID, PlainText: true, Answer: text, Output: map[string]interface{}{finalOutputSummaryKey: text},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &chatLoopFixture{
		t: t, ctx: ctx, store: store, service: service, conversation: conversation, loop: loop, team: options.team,
		actions: &chatLoopActions{calls: map[string]*ActionCall{}, approvals: map[string]*ApprovalCheckpoint{}},
		now:     time.Now().UTC().Truncate(time.Millisecond),
		agents:  chatLoopAgentTurns{},
	}
	f.runner = f.newRunner(ConversationRunTurnRunnerConfig{RequireParticipationOptIn: options.participation})
	return f
}

// newRunner is a conversation turn runner over the fixture's store, with
// config and the fixture's Agent bindings as its AgentTurns resolver.
func (f *chatLoopFixture) newRunner(config ConversationRunTurnRunnerConfig) *ConversationRunTurnRunner {
	f.t.Helper()
	config.AgentTurns = f.agents
	runner, err := NewConversationRunTurnRunner(chatLoopRunnerStore(f.t, f.store, f.actions), conversationRunTestCoordinator(f.t, f.service), config)
	if err != nil {
		f.t.Fatal(err)
	}
	return runner
}

// chatLoopAgentTurns is the bare Agent binding bindAgent registered for each
// Run, by Run id.
type chatLoopAgentTurns map[string]*agentrun.TurnRunnerBinding

func (a chatLoopAgentTurns) ResolveTurnRunner(_ context.Context, run *AgentRun) (*agentrun.TurnRunnerBinding, error) {
	binding, ok := a[run.ID]
	if !ok {
		return nil, fmt.Errorf("no Agent binding registered for Run %s", run.ID)
	}
	return binding, nil
}

// sqliteChatLoopStore and postgresChatLoopStore are the fixture store as the
// turn runner sees it: the chat seams read governed calls and approvals from
// chatLoopActions, everything else from the store itself.
type sqliteChatLoopStore struct {
	*sqlite.Store
	actions *chatLoopActions
}

func (s *sqliteChatLoopStore) GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

func (s *sqliteChatLoopStore) GetApproval(ctx context.Context, scope Scope, id string) (*ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}

type postgresChatLoopStore struct {
	*postgres.Store
	actions *chatLoopActions
}

func (s *postgresChatLoopStore) GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

func (s *postgresChatLoopStore) GetApproval(ctx context.Context, scope Scope, id string) (*ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}

func chatLoopRunnerStore(t *testing.T, store chatLoopStore, actions *chatLoopActions) ConversationStore {
	t.Helper()
	switch store := store.(type) {
	case *sqlite.Store:
		return &sqliteChatLoopStore{Store: store, actions: actions}
	case *postgres.Store:
		return &postgresChatLoopStore{Store: store, actions: actions}
	}
	t.Fatalf("no chat loop runner store for %T", store)
	return nil
}

// setParticipation turns the chat's participation opt-in on or off.
func (f *chatLoopFixture) setParticipation(enabled bool) {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.conversation.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.conversation, err = f.service.UpdateConversation(f.ctx, conversation.UpdateConversationRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision, ParticipationEnabled: &enabled,
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *chatLoopFixture) lease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: chatLoopWorker, Now: f.now}
}

func (f *chatLoopFixture) post(key, userID, content string) *ChannelMessage {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.conversation.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(f.ctx, conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: userID}, Intent: MessageIntentQuestion,
		Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.conversation, err = f.service.GetConversation(f.ctx, current.Scope, current.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return posted.Message
}

// startRun creates and claims the chat Run that answers trigger.
func (f *chatLoopFixture) startRun(trigger *ChannelMessage) *AgentRun {
	f.t.Helper()
	request := conversation.ConversationAgentRunRequest(f.conversation, trigger)
	if f.team {
		request.AssignedAgentID = "agent-1"
		request.ConcurrencyKey = ConversationRunLane(f.conversation.ID, "", "agent-1")
		request.SessionID = conversationSessionID(f.conversation.Scope, request.ConcurrencyKey)
		request.IdempotencyKey = "member-" + trigger.ID
	}
	commands := agentrun.NewRunCommandService(f.store, models.WithClock(func() time.Time { return f.now }))
	result, err := commands.CreateAgentRun(f.ctx, request)
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	created := result.Run
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: chatLoopWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, created.ID)
	}
	return claimed
}

// bind resolves agent-1's binding for run.
func (f *chatLoopFixture) bind(run *AgentRun, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	return f.bindAgent(run, "agent-1", producer, actions...)
}

// bindAgent resolves run's binding through the conversation turn runner, as
// the worker does: the runner adds the chat seams to a bare binding of
// agentID with producer and actions.
func (f *chatLoopFixture) bindAgent(run *AgentRun, agentID string, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	f.agents[run.ID] = &agentrun.TurnRunnerBinding{
		DeploymentID: agentID, DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(ModelStepProviderAnthropic), Model: "chat-model", ModelActions: actions,
		Producer: producer, Preparers: []agentrun.StepPreparer{newRunStartPreparer(f.store, f.store)},
		// What ResolveControlAuthority grants the speaking Agent of a chat.
		Authority: agentrun.ControlAuthority{AgentID: agentID, CanAskUser: true, CanReact: true},
	}
	binding, err := f.runner.ResolveTurnRunner(f.ctx, run)
	if err != nil {
		f.t.Fatalf("resolve the chat binding of %s: %v", run.ID, err)
	}
	return binding
}

func (f *chatLoopFixture) drive(run *AgentRun, binding *agentrun.TurnRunnerBinding) RunLoopExit {
	f.t.Helper()
	current, err := f.store.GetAgentRun(f.ctx, run.Scope, run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	exit := f.loop.Drive(f.ctx, RunLoopClaim{Run: current, WorkerID: chatLoopWorker, Binding: binding})
	if exit.Err != nil {
		f.t.Fatalf("drive %s: %v (exit %s)", run.ID, exit.Err, exit.Reason)
	}
	return exit
}

// prepare runs the loop's phase 3 for run as the next step would.
func (f *chatLoopFixture) prepare(run *AgentRun, binding *agentrun.TurnRunnerBinding, step int64) *stepContextResult {
	f.t.Helper()
	current, err := f.store.GetAgentRun(f.ctx, run.Scope, run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	session, err := f.store.GetAgentSession(f.ctx, run.Scope, binding.Session.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	in := agentrun.StepInput{
		Run: current, Session: session, Binding: binding, WorkerID: chatLoopWorker,
		Step: &AgentTurn{ID: fmt.Sprintf("%s-step-%d", run.ID, step), Sequence: step},
	}
	result, err := appendStepContext(f.ctx, f.store, in, binding.Preparers, f.now)
	if err != nil {
		f.t.Fatalf("step context of %s: %v", run.ID, err)
	}
	return result
}

func (f *chatLoopFixture) path(sessionID string) []*SessionEntry {
	f.t.Helper()
	path, err := f.store.LoadAgentSessionPath(f.ctx, f.conversation.Scope, sessionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return path.Entries
}

func (f *chatLoopFixture) agentMessages() []*ChannelMessage {
	f.t.Helper()
	messages, err := f.service.ListChannelMessages(f.ctx, ChannelMessageFilter{Scope: f.conversation.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []*ChannelMessage
	for _, message := range messages {
		if message.Sender.Type != ConversationParticipantUser {
			out = append(out, message)
		}
	}
	return out
}

func (f *chatLoopFixture) run(id string) *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.conversation.Scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// stepEntries appends one assistant entry with these tool calls (pending)
// for run, as a model step of the loop would.
func (f *chatLoopFixture) stepEntries(run *AgentRun, sessionID, stepID string, step int64, calls ...ModelToolCall) map[string]*SessionEntry {
	f.t.Helper()
	assistant := &SessionEntry{
		ID: SessionEntryID(sessionID, AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: step, Type: SessionEntryAssistant,
		Assistant: &AssistantEntry{Provider: ModelStepProviderAnthropic, Model: "chat-model", StopReason: ModelStopToolCalls, ToolCalls: calls},
	}
	batch := []*SessionEntry{assistant}
	for _, call := range calls {
		batch = append(batch, &SessionEntry{
			ID: SessionEntryID(sessionID, ToolEntryKey(stepID, call.ID)), RunID: run.ID, StepID: stepID, Step: step,
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
			Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: ToolSideEffectWrite},
		})
	}
	lease := f.lease()
	appended, err := f.store.AppendAgentSessionEntries(f.ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: sessionID, RunID: run.ID, Lease: &lease, Entries: batch, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append step: %v", err)
	}
	entries := map[string]*SessionEntry{}
	for _, entry := range appended.Entries {
		if entry.Type == SessionEntryTool {
			entries[entry.ToolCallID] = entry
		}
	}
	return entries
}

func (f *chatLoopFixture) settle(run *AgentRun, entry *SessionEntry, to ToolEntryState, actionCallID string, result *ToolResult) *SessionEntry {
	f.t.Helper()
	transitioned, err := f.store.TransitionToolEntry(f.ctx, ToolEntryTransitionRequest{
		Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: f.lease(),
		From: entry.State, ExpectedRevision: entry.Revision, To: to, ActionCallID: actionCallID, Result: result, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("settle %s: %v", entry.ID, err)
	}
	return transitioned.Entry
}

func chatLoopEntryText(entry *SessionEntry) string {
	switch {
	case entry == nil:
		return ""
	case entry.User != nil && len(entry.User.Content) > 0:
		return entry.User.Content[0].Text
	case entry.Assistant != nil:
		return entry.Assistant.Text
	case entry.Note != nil:
		return entry.Note.Text
	}
	return ""
}

// chatLoopShape is the path as "type:detail" items: the message id of user
// entries, the note kind, the Run of assistant and tool entries.
func chatLoopShape(entries []*SessionEntry) []string {
	shape := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch entry.Type {
		case SessionEntryUser:
			shape = append(shape, "user:"+entry.User.MessageID)
		case SessionEntryNote:
			shape = append(shape, "note:"+string(entry.Note.Kind)+":"+entry.RunID)
		default:
			shape = append(shape, string(entry.Type)+":"+entry.RunID)
		}
	}
	return shape
}

func chatLoopExpectShape(t *testing.T, entries []*SessionEntry, want ...string) {
	t.Helper()
	got := chatLoopShape(entries)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("path =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestChatRunAnswersFromSessionNotGoal(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		earlier := f.post("earlier", "user-1", "The launch is on Friday.")
		trigger := f.post("trigger", "user-1", "When is the launch?")
		run := f.startRun(trigger)
		producer := &chatLoopProducer{answers: []string{"Friday."}}
		binding := f.bind(run, producer)
		exit := f.drive(run, binding)
		if exit.Reason != RunLoopExitFinal || exit.Run.Status != AgentRunStatusCompleted {
			t.Fatalf("exit = %s, run %s", exit.Reason, exit.Run.Status)
		}
		if len(producer.paths) != 1 {
			t.Fatalf("model steps = %d", len(producer.paths))
		}
		rendered := producer.paths[0]
		chatLoopExpectShape(t, rendered, "user:"+earlier.ID, "user:"+trigger.ID, "note:run_context:"+run.ID)
		for _, entry := range rendered {
			text := chatLoopEntryText(entry)
			if entry.User != nil && entry.User.Origin != UserEntryChannelMessage || strings.Contains(text, run.Goal) || strings.Contains(text, `"goal"`) {
				t.Fatalf("goal JSON reached the step: %#v %q", entry, text)
			}
		}
		if !strings.HasSuffix(chatLoopEntryText(rendered[1]), "\nWhen is the launch?") || !strings.Contains(chatLoopEntryText(rendered[2]), trigger.ID) {
			t.Fatalf("trigger %q, note %q", chatLoopEntryText(rendered[1]), chatLoopEntryText(rendered[2]))
		}
		answers := f.agentMessages()
		if len(answers) != 1 || answers[0].Content != "Friday." || answers[0].ReplyToMessageID != trigger.ID ||
			answers[0].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) {
			t.Fatalf("answers = %#v", answers)
		}
		session := f.run(run.ID).SessionID
		if session != binding.Session.ID || session != conversationSessionID(run.Scope, run.ConcurrencyKey) {
			t.Fatalf("run session %q, binding %q", session, binding.Session.ID)
		}
		// The read receipt follows the committed watermark.
		cursor, err := f.service.GetCursor(f.ctx, run.Scope, f.conversation.ID, ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"})
		if err != nil || cursor == nil || cursor.ReadSequence < trigger.Sequence {
			t.Fatalf("agent cursor = %#v, %v (trigger %d)", cursor, err, trigger.Sequence)
		}
	})
}

func TestSecondMessageSeesFirstExchangeAsLogEntries(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		first := f.post("first", "user-1", "Draft the agenda.")
		run1 := f.startRun(first)
		f.drive(run1, f.bind(run1, &chatLoopProducer{answers: []string{"Agenda: intro, demo, Q&A."}}))

		second := f.post("second", "user-1", "Add a break.")
		run2 := f.startRun(second)
		producer := &chatLoopProducer{answers: []string{"Added."}}
		exit := f.drive(run2, f.bind(run2, producer))
		if exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
			t.Fatalf("exit = %#v, steps %d", exit, len(producer.paths))
		}
		rendered := producer.paths[0]
		chatLoopExpectShape(t, rendered,
			"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
			"user:"+second.ID, "note:run_context:"+run2.ID)
		if chatLoopEntryText(rendered[2]) != "Agenda: intro, demo, Q&A." || rendered[3].User.Author.Type != "user" {
			t.Fatalf("first exchange = %q / %#v", chatLoopEntryText(rendered[2]), rendered[3].User.Author)
		}
		if answers := f.agentMessages(); len(answers) != 2 || answers[1].Content != "Added." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestFollowUpFromSamePersonAppendedAtNextStepOnce(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Summarize the report.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)

		followUp := f.post("follow-up", "user-1", "Only the risks, please.")
		current := f.run(run.ID)
		updated := cloneAgentRun(current)
		updated.PendingInterventions = append(updated.PendingInterventions, AgentRunIntervention{
			ID: conversationFollowUpInterventionID(run.Scope, followUp.ID), Actor: ActivityActor{Type: "user", ID: "user-1"},
			Instruction: followUp.Content, CreatedAt: f.now,
			ConversationMessage: &ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: followUp.ID, Sequence: followUp.Sequence, Sender: followUp.Sender},
		})
		updated.Revision++
		if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, &ActivityEvent{
			ID: "follow-up-" + uuid.NewString()[:8], Scope: run.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
			RunID: run.ID, Actor: ActivityActor{Type: "worker", ID: chatLoopWorker}, Summary: "Follow-up delivered",
			Visibility: ActivityVisibilityScope, CreatedAt: f.now,
		}, nil); err != nil {
			t.Fatal(err)
		}

		second := f.prepare(run, binding, 2)
		if !second.CursorMoved {
			t.Fatal("the follow-up was not consumed")
		}
		f.prepare(run, binding, 3)
		chatLoopExpectShape(t, f.path(binding.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+followUp.ID)
		entries := f.path(binding.Session.ID)
		if !strings.Contains(chatLoopEntryText(entries[2]), "(user user-1)") || entries[2].Step != 2 {
			t.Fatalf("follow-up entry = %q at step %d", chatLoopEntryText(entries[2]), entries[2].Step)
		}
		if got := f.run(run.ID).ConsumedInterventions; got != 1 {
			t.Fatalf("consumed interventions = %d", got)
		}
	})
}

func TestOtherPersonsMessageStartsOwnRunAndInterleavesAfterApprovalPark(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		triggerA := f.post("a", "user-1", "Send the invoice.")
		runA := f.startRun(triggerA)
		bindingA := f.bind(runA, &chatLoopProducer{})
		f.prepare(runA, bindingA, 1)
		// A's governed call is held for approval; A parks.
		f.stepEntries(runA, bindingA.Session.ID, "a-step-1", 1,
			ModelToolCall{ID: "send", Name: "billing__send_invoice", Arguments: json.RawMessage(`{"invoice":"7"}`)})

		triggerB := f.post("b", "user-2", "What is on my calendar?")
		runB := f.startRun(triggerB)
		bindingB := f.bind(runB, &chatLoopProducer{})
		f.prepare(runB, bindingB, 1)
		f.stepEntries(runB, bindingB.Session.ID, "b-step-1", 1)

		// A resumes after the approval: nothing new is ingested for it.
		if result := f.prepare(runA, bindingA, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("A ingested again: %#v", result.Append.Entries)
		}
		chatLoopExpectShape(t, f.path(bindingA.Session.ID),
			"user:"+triggerA.ID, "note:run_context:"+runA.ID, "assistant:"+runA.ID, "tool:"+runA.ID,
			"user:"+triggerB.ID, "note:run_context:"+runB.ID, "assistant:"+runB.ID)
		if bindingA.Session.ID != bindingB.Session.ID {
			t.Fatalf("one lane, two sessions: %s %s", bindingA.Session.ID, bindingB.Session.ID)
		}
		entries := f.path(bindingB.Session.ID)
		if entries[4].User.Author.ID != "user-2" || !strings.Contains(chatLoopEntryText(entries[5]), triggerB.ID) {
			t.Fatalf("B's trigger %#v, note %q", entries[4].User.Author, chatLoopEntryText(entries[5]))
		}
	})
}

// TestRunActionsPresentWithoutActiveWork: a chat with no active work keeps
// the run-control actions (the tool list does not change when work starts or
// ends); a call names no active work, so it is refused.
func TestRunActionsPresentWithoutActiveWork(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: RunActionPause, InputSchema: definition.Actions[RunActionPause].InputSchema,
		}
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}
		quiet := f.post("quiet", "user-1", "Hello")
		idle := f.startRun(quiet)
		binding := f.bind(idle, &chatLoopProducer{}, pause, other)
		if len(binding.ModelActions) != 2 || binding.ModelActions[0].Name != pause.Name {
			t.Fatalf("idle chat actions = %#v", binding.ModelActions)
		}
		refusal, err := binding.ToolCheck(f.ctx, agentrun.StepToolsInput{Run: idle, Binding: binding}, &SessionEntry{
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: "call",
			Tool: &ToolEntry{Name: pause.Name, Arguments: json.RawMessage(`{"runId":"nope"}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(refusal, "is not active work in this conversation") {
			t.Fatalf("idle refusal = %q", refusal)
		}
	})
}

func TestRunControlInvalidRunIdIsToolError(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: RunActionPause, InputSchema: definition.Actions[RunActionPause].InputSchema,
		}
		resume := pause
		resume.Name, resume.Action, resume.InputSchema = "openseal_runs__resume", RunActionResume, definition.Actions[RunActionResume].InputSchema
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}

		trigger := f.post("pause", "user-1", "Pause the export.")
		run := f.startRun(trigger)
		portfolio := NewPortfolioService(f.store, models.WithClock(func() time.Time { return f.now }))
		work, err := portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
			Scope: run.Scope, Kind: RunKindAgentWork, ParentRunID: run.ID, Owner: run.Owner, AssignedAgentID: "agent-1",
			Goal: "Export the report", Source: RunSourceFork, IdempotencyKey: "work-" + run.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		binding := f.bind(run, &chatLoopProducer{}, pause, resume, other)
		if len(binding.ModelActions) != 3 {
			t.Fatalf("active chat actions = %#v", binding.ModelActions)
		}
		if _, ok := binding.ModelActions[0].InputSchema["properties"].(map[string]interface{})["runId"].(map[string]interface{})["enum"]; ok {
			t.Fatal("run id schema carries a per-claim enum")
		}
		check := func(name, runID string) string {
			t.Helper()
			refusal, err := binding.ToolCheck(f.ctx, agentrun.StepToolsInput{Run: run, Binding: binding}, &SessionEntry{
				Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: "call",
				Tool: &ToolEntry{Name: name, Arguments: json.RawMessage(fmt.Sprintf(`{"runId":%q}`, runID))},
			})
			if err != nil {
				t.Fatal(err)
			}
			return refusal
		}
		if refusal := check(pause.Name, work.ID); refusal != "" {
			t.Fatalf("pausing active work refused: %q", refusal)
		}
		want := "Run nope is not active work in this conversation, or it cannot be paused now. Use a run id from the active work in the run context."
		if refusal := check(pause.Name, "nope"); refusal != want {
			t.Fatalf("refusal = %q", refusal)
		}
		if refusal := check(resume.Name, work.ID); !strings.Contains(refusal, "cannot be resumed now") {
			t.Fatalf("resuming running work = %q", refusal)
		}
		if refusal := check(pause.Name, ""); refusal != "A runId from the active work in the run context is required." {
			t.Fatalf("missing run id refusal = %q", refusal)
		}
		if refusal := check(other.Name, "nope"); refusal != "" {
			t.Fatalf("a non run-control call was refused: %q", refusal)
		}
	})
}

// A tool that saves files posts nothing: work-in-progress files (panels
// drawn for a comic) stay in the Library, and the final answer hands over
// the finished ones.
func TestNoMessageAfterAGovernedToolSavesFiles(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Export both reports.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)
		entries := f.stepEntries(run, binding.Session.ID, "step-1", 1,
			ModelToolCall{ID: "a", Name: "reports__export", Arguments: json.RawMessage(`{"report":"a"}`)},
			ModelToolCall{ID: "b", Name: "reports__export", Arguments: json.RawMessage(`{"report":"b"}`)})
		for _, id := range []string{"a", "b"} {
			actionID := "action-" + id
			artifact := &Artifact{ID: "file-" + id, Version: 1, Scope: run.Scope, Name: id + ".pdf",
				Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "agent-1"}, Owner: &run.Owner, RunID: run.ID, ActionID: actionID},
				ContentRef: "object-store:" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: ArtifactClassificationInternal}
			if err := f.store.CreateArtifactVersion(f.ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			f.actions.calls[actionID] = &ActionCall{ID: actionID, Scope: run.Scope, RunID: run.ID, Status: ActionCallStatusSucceeded,
				Output: map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": artifact.ID, "version": 1}}}}
			done := f.settle(run, entries[id], ToolEntryCompleted, actionID, &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{storetest.TextPart("saved")}})
			for range 2 { // the hook is replay-safe
				if err := binding.AfterTool(f.ctx, agentrun.StepToolsInput{Run: f.run(run.ID), Binding: binding}, done); err != nil {
					t.Fatal(err)
				}
			}
		}
		if posted := f.agentMessages(); len(posted) != 0 {
			t.Fatalf("posted after tools = %#v", posted)
		}
	})
}

// Participation is on when the Run is bound and stopped before its first
// step: the step's seams end the Run without a model call.
func TestParticipationStoppedBeforeFirstStepPostsNothing(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{participation: true}, func(t *testing.T, f *chatLoopFixture) {
		f.setParticipation(true)
		trigger := f.post("trigger", "user-1", "Anyone there?")
		run := f.startRun(trigger)
		producer := &chatLoopProducer{}
		binding := f.bind(run, producer)
		f.setParticipation(false)
		exit := f.drive(run, binding)
		if exit.Reason != RunLoopExitFinal || exit.Run.Status != AgentRunStatusCompleted || exit.Run.Output["participationSkipped"] != true {
			t.Fatalf("exit = %s %s %#v", exit.Reason, exit.Run.Status, exit.Run.Output)
		}
		if len(producer.paths) != 0 {
			t.Fatalf("the model was called %d times", len(producer.paths))
		}
		if messages := f.agentMessages(); len(messages) != 0 {
			t.Fatalf("posted = %#v", messages)
		}
	})
}

func TestTeamReplacedApprovalEndsMemberSilently(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("follow-up=%v", followUp), func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{team: true}, func(t *testing.T, f *chatLoopFixture) {
				trigger := f.post("trigger", "user-1", "Deploy the canary.")
				run := f.startRun(trigger)
				producer := &chatLoopProducer{answers: []string{"Answered the follow-up."}}
				binding := f.bind(run, producer)
				f.prepare(run, binding, 1)
				entries := f.stepEntries(run, binding.Session.ID, "member-step-1", 1,
					ModelToolCall{ID: "deploy", Name: "deploy__canary", Arguments: json.RawMessage(`{}`)})
				newer := f.post("newer", "user-1", "Actually, ask the other member.")
				f.actions.calls["action-deploy"] = &ActionCall{ID: "action-deploy", Scope: run.Scope, RunID: run.ID, Status: ActionCallStatusDenied, ApprovalID: "approval-deploy"}
				f.actions.approvals["approval-deploy"] = &ApprovalCheckpoint{ID: "approval-deploy", Scope: run.Scope, Status: ApprovalStatusSuperseded, SupersededByMessageID: newer.ID}
				f.settle(run, entries["deploy"], ToolEntryError, "action-deploy", governedJSONResult(ToolResultSuperseded, map[string]interface{}{
					"status": string(ToolResultSuperseded), "reason": action.ApprovalSupersededReason,
				}))
				if followUp {
					current := f.run(run.ID)
					updated := cloneAgentRun(current)
					updated.PendingInterventions = append(updated.PendingInterventions, AgentRunIntervention{
						ID: conversationFollowUpInterventionID(run.Scope, newer.ID), Actor: ActivityActor{Type: "user", ID: "user-1"},
						Instruction: newer.Content, CreatedAt: f.now,
						ConversationMessage: &ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: newer.ID, Sequence: newer.Sequence, Sender: newer.Sender},
					})
					updated.Revision++
					if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, &ActivityEvent{
						ID: "follow-up", Scope: run.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo, RunID: run.ID,
						Actor: ActivityActor{Type: "worker", ID: chatLoopWorker}, Summary: "Follow-up delivered", Visibility: ActivityVisibilityScope, CreatedAt: f.now,
					}, nil); err != nil {
						t.Fatal(err)
					}
				}
				exit := f.drive(run, binding)
				if exit.Run.Status != AgentRunStatusCompleted {
					t.Fatalf("exit = %s %s", exit.Reason, exit.Run.Status)
				}
				if followUp {
					if len(producer.paths) != 1 || exit.Run.Output["approvalReplaced"] != nil {
						t.Fatalf("follow-up member: steps %d output %#v", len(producer.paths), exit.Run.Output)
					}
					return
				}
				if len(producer.paths) != 0 || exit.Run.Output["approvalReplaced"] != true || exit.Run.Output["replacedByMessageId"] != newer.ID {
					t.Fatalf("replaced member: steps %d output %#v", len(producer.paths), exit.Run.Output)
				}
				if messages := f.agentMessages(); len(messages) != 0 {
					t.Fatalf("replaced member posted %#v", messages)
				}
			})
		})
	}
}

// promotingChatLoopStore promotes a chat Run past the foreground timeout.
type promotingChatLoopStore interface {
	PromoteConversationTask(ctx context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error)
}

func (f *chatLoopFixture) promote(run *AgentRun) {
	f.t.Helper()
	store, ok := f.store.(promotingChatLoopStore)
	if !ok {
		f.t.Fatalf("%T cannot promote", f.store)
	}
	result, err := store.PromoteConversationTask(f.ctx, ConversationTaskPromotionRequest{
		Scope: run.Scope, RunID: run.ID, Now: run.CreatedAt.Add(ConversationTaskForegroundTimeout),
	})
	if err != nil || result == nil {
		f.t.Fatalf("promote %s: %#v, %v", run.ID, result, err)
	}
}

func TestPromotedBeforeFirstStepRendersTriggerAndRunContext(t *testing.T) {
	for _, when := range []string{"after-bind", "before-bind"} {
		t.Run(when, func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
				first := f.post("first", "user-1", "Draft the agenda.")
				run1 := f.startRun(first)
				f.drive(run1, f.bind(run1, &chatLoopProducer{answers: []string{"Agenda: intro, demo."}}))
				lane := f.run(run1.ID).SessionID

				// The second Run waited behind the first: it is promoted to a
				// child of the non-empty lane before its first step.
				second := f.post("second", "user-1", "Research the venues in depth.")
				run2 := f.startRun(second)
				var binding *agentrun.TurnRunnerBinding
				producer := &chatLoopProducer{answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.bind(run2, producer)
					f.promote(run2)
				} else {
					f.promote(run2)
					binding = f.bind(f.run(run2.ID), producer)
				}
				f.drive(run2, binding)
				if len(producer.paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.run(run2.ID).SessionID
				if child == lane || child != RunAgentSessionID(run2.Scope, run2.ID) {
					t.Fatalf("run session %q, lane %q", child, lane)
				}
				chatLoopExpectShape(t, producer.paths[0],
					"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
					"user:"+second.ID, "note:run_context:"+run2.ID)
				if note := producer.paths[0][4]; note.SessionID != child || !strings.Contains(chatLoopEntryText(note), second.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, chatLoopEntryText(note))
				}
				for _, entry := range f.path(lane) {
					if entry.RunID == run2.ID && !(entry.Type == SessionEntryNote && entry.Note.Kind == NoteTaskPromoted) {
						t.Fatalf("the promoted Run wrote to the lane: %#v", entry)
					}
				}
			})
		})
	}
}

// A Run promoted on an empty lane gets a seeded child (only its task brief);
// its goal seed is not a sign it started, so StartRun still renders the
// trigger and the run_context note after the brief (§3.3, §4.5).
func TestPromotedBeforeFirstStepOnEmptyLaneSeedsTriggerAndRunContext(t *testing.T) {
	for _, when := range []string{"after-bind", "before-bind"} {
		t.Run(when, func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
				trigger := f.post("trigger", "user-1", "Research the venues in depth.")
				run := f.startRun(trigger)
				var binding *agentrun.TurnRunnerBinding
				producer := &chatLoopProducer{answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.bind(run, producer)
					f.promote(run)
				} else {
					f.promote(run)
					binding = f.bind(f.run(run.ID), producer)
				}
				f.drive(run, binding)
				if len(producer.paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.run(run.ID).SessionID
				if child != RunAgentSessionID(run.Scope, run.ID) {
					t.Fatalf("run session %q", child)
				}
				path := producer.paths[0]
				chatLoopExpectShape(t, path, "user:", "user:"+trigger.ID, "note:run_context:"+run.ID)
				if seed := path[0]; seed.User.Origin != UserEntryGoal || !strings.Contains(chatLoopEntryText(seed), "<brief>") {
					t.Fatalf("first entry is not the task brief: %#v", seed)
				}
				if note := path[2]; note.SessionID != child || !strings.Contains(chatLoopEntryText(note), trigger.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, chatLoopEntryText(note))
				}
			})
		})
	}
}

func TestRunPromotedMidFlightCountsAsStartedInChild(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Compile the quarterly numbers.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)
		f.promote(run)
		promoted := f.bind(f.run(run.ID), &chatLoopProducer{})
		if promoted.Session.Kind != AgentSessionKindRun {
			t.Fatalf("promoted session = %#v", promoted.Session)
		}
		// The chat preparer counts the Run as started in its child: with no
		// steering it has nothing to add, where an unstarted Run would get
		// StartRun's trigger and run_context entries.
		if len(promoted.Preparers) != 2 {
			t.Fatalf("preparers = %d, want the run-start and chat preparers", len(promoted.Preparers))
		}
		step := &AgentTurn{ID: run.ID + "-step-2", Sequence: 2}
		part, err := promoted.Preparers[1].BeforeStep(f.ctx, agentrun.StepInput{Run: f.run(run.ID), Session: promoted.Session, Binding: promoted, WorkerID: chatLoopWorker, Step: step}, agentrun.SteeringPlan{})
		if err != nil || part != nil {
			t.Fatalf("the promoted Run did not count as started: %#v, %v", part, err)
		}
		// The second step appends nothing to the child: StartRun does not run again.
		if result := f.prepare(run, promoted, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("the child re-ran StartRun: %#v", result.Append.Entries)
		}
		chatLoopExpectShape(t, f.path(promoted.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID)
	})
}
