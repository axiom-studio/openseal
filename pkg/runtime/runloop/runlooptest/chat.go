package runlooptest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
)

const ChatWorker = "chat-loop-worker"

type ChatStore interface {
	ChatFinalStore
	models.KernelStore
	models.RunCommandStore
}

// ChatActions is the governed call and approval store the seams read.
type ChatActions struct {
	mu        sync.Mutex
	Calls     map[string]*models.ActionCall
	Approvals map[string]*models.ApprovalCheckpoint
}

func (a *ChatActions) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.Calls[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

func (a *ChatActions) GetApproval(_ context.Context, _ models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	approval, ok := a.Approvals[id]
	if !ok {
		return nil, models.ErrApprovalNotFound
	}
	copied := *approval
	return &copied, nil
}

// ChatProducer is a model producer that records the session path each
// step renders from and answers with the next scripted text.
type ChatProducer struct {
	Answers []string
	Paths   [][]*models.SessionEntry
}

func (*ChatProducer) Kind() models.StepProducerKind { return models.StepProducerModel }

func (p *ChatProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	var entries []*models.SessionEntry
	if in.Path != nil {
		entries = append(entries, in.Path.Entries...)
	}
	p.Paths = append(p.Paths, entries)
	answer := "done"
	if len(p.Answers) > 0 {
		answer, p.Answers = p.Answers[0], p.Answers[1:]
	}
	return agentrun.StepOutput{Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "chat-model", Text: answer, StopReason: models.ModelStopEnd,
	}}, nil
}

type ChatOptions struct {
	Team          bool
	Participation bool // RequireParticipationOptIn with participation off
}

type ChatFixture struct {
	T            *testing.T
	Ctx          context.Context
	Store        ChatStore
	Service      *conversation.Service
	Conversation *models.Conversation
	Runner       *conversationrun.TurnRunner
	Actions      *ChatActions
	Loop         *runloop.Loop
	Team         bool
	Now          time.Time
	// agents is the runner's AgentTurns resolver: the bare Agent binding of
	// each Run, before the chat seams are added.
	Agents ChatAgentTurns
}

func ForChatStores(t *testing.T, options ChatOptions, test func(*testing.T, *ChatFixture)) {
	t.Helper()
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		test(t, NewChatFixture(t, base, options))
	})
}

func NewChatFixture(t *testing.T, base conversationtest.ThreadStartStore, options ChatOptions) *ChatFixture {
	t.Helper()
	store, ok := base.(ChatStore)
	if !ok {
		t.Fatalf("%T is not a kernel store with the session log", base)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "chat-loop-" + uuid.NewString()[:8]}
	service := conversation.NewService(store)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	if options.Team {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-1"}
	}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Chat loop", IdempotencyKey: "chat-loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := runloop.New(store, nil, nil, runloop.Config{
		PlanToolEntries: func(*agentrun.TurnRunnerBinding, *models.AgentSession, *models.AgentTurn, *models.SessionEntry, []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
			return nil, fmt.Errorf("chat loop tests make no tool calls")
		},
		PlainTextFinal: func(_ context.Context, cc agentrun.ControlContext, text string) (control.Result, error) {
			return control.Result{Kind: control.Final, Final: &agentrun.FinalAnswer{
				StepID: cc.Step.ID, PlainText: true, Answer: text, Output: map[string]interface{}{conversationrun.FinalOutputSummaryKey: text},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &ChatFixture{
		T: t, Ctx: ctx, Store: store, Service: service, Conversation: conversation, Loop: loop, Team: options.Team,
		Actions: &ChatActions{Calls: map[string]*models.ActionCall{}, Approvals: map[string]*models.ApprovalCheckpoint{}},
		Now:     time.Now().UTC().Truncate(time.Millisecond),
		Agents:  ChatAgentTurns{},
	}
	f.Runner = f.NewRunner(conversationrun.TurnRunnerConfig{RequireParticipationOptIn: options.Participation})
	return f
}

// NewRunner is a conversation turn runner over the fixture's store, with
// config and the fixture's Agent bindings as its AgentTurns resolver.
func (f *ChatFixture) NewRunner(config conversationrun.TurnRunnerConfig) *conversationrun.TurnRunner {
	f.T.Helper()
	config.AgentTurns = f.Agents
	runner, err := conversationrun.NewTurnRunner(chatLoopRunnerStore(f.T, f.Store, f.Actions), chatCoordinator(f.T, f.Service), config)
	if err != nil {
		f.T.Fatal(err)
	}
	return runner
}

// ChatAgentTurns is the bare Agent binding BindAgent registered for each
// Run, by Run id.
type ChatAgentTurns map[string]*agentrun.TurnRunnerBinding

func (a ChatAgentTurns) ResolveTurnRunner(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	binding, ok := a[run.ID]
	if !ok {
		return nil, fmt.Errorf("no Agent binding registered for Run %s", run.ID)
	}
	return binding, nil
}

// sqliteChatLoopStore and postgresChatLoopStore are the fixture store as the
// turn runner sees it: the chat seams read governed calls and approvals from
// ChatActions, everything else from the store itself.
type sqliteChatLoopStore struct {
	*sqlite.Store
	actions *ChatActions
}

func (s *sqliteChatLoopStore) GetActionCall(ctx context.Context, scope models.Scope, id string) (*models.ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

func (s *sqliteChatLoopStore) GetApproval(ctx context.Context, scope models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}

type postgresChatLoopStore struct {
	*postgres.Store
	actions *ChatActions
}

func (s *postgresChatLoopStore) GetActionCall(ctx context.Context, scope models.Scope, id string) (*models.ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

func (s *postgresChatLoopStore) GetApproval(ctx context.Context, scope models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}

func chatLoopRunnerStore(t *testing.T, store ChatStore, actions *ChatActions) models.ConversationStore {
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

// SetParticipation turns the chat's participation opt-in on or off.
func (f *ChatFixture) SetParticipation(enabled bool) {
	f.T.Helper()
	current, err := f.Service.GetConversation(f.Ctx, f.Conversation.Scope, f.Conversation.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	f.Conversation, err = f.Service.UpdateConversation(f.Ctx, conversation.UpdateConversationRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision, ParticipationEnabled: &enabled,
	})
	if err != nil {
		f.T.Fatal(err)
	}
}

func (f *ChatFixture) Lease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: ChatWorker, Now: f.Now}
}

func (f *ChatFixture) Post(key, userID, content string) *models.ChannelMessage {
	f.T.Helper()
	current, err := f.Service.GetConversation(f.Ctx, f.Conversation.Scope, f.Conversation.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	posted, err := f.Service.PostChannelMessage(f.Ctx, conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: userID}, Intent: models.MessageIntentQuestion,
		Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		f.T.Fatal(err)
	}
	f.Conversation, err = f.Service.GetConversation(f.Ctx, current.Scope, current.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	return posted.Message
}

// StartRun creates and claims the chat Run that answers trigger.
func (f *ChatFixture) StartRun(trigger *models.ChannelMessage) *models.AgentRun {
	f.T.Helper()
	request := conversation.AgentRunRequest(f.Conversation, trigger)
	if f.Team {
		request.AssignedAgentID = "agent-1"
		request.ConcurrencyKey = models.ConversationRunLane(f.Conversation.ID, "", "agent-1")
		request.SessionID = models.ConversationSessionID(f.Conversation.Scope, request.ConcurrencyKey)
		request.IdempotencyKey = "member-" + trigger.ID
	}
	commands := agentrun.NewRunCommandService(f.Store, models.WithClock(func() time.Time { return f.Now }))
	result, err := commands.CreateAgentRun(f.Ctx, request)
	if err != nil {
		f.T.Fatalf("create run: %v", err)
	}
	created := result.Run
	claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Conversation.Scope, WorkerID: ChatWorker, Now: f.Now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		f.T.Fatalf("claim = %#v, %v (want %s)", claimed, err, created.ID)
	}
	return claimed
}

// Bind resolves agent-1's binding for run.
func (f *ChatFixture) Bind(run *models.AgentRun, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	return f.BindAgent(run, "agent-1", producer, actions...)
}

// BindAgent resolves run's binding through the conversation turn runner, as
// the worker does: the runner adds the chat seams to a bare binding of
// agentID with producer and actions.
func (f *ChatFixture) BindAgent(run *models.AgentRun, agentID string, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	f.Agents[run.ID] = &agentrun.TurnRunnerBinding{
		DeploymentID: agentID, DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(models.ModelStepProviderAnthropic), Model: "chat-model", ModelActions: actions,
		Producer: producer, Preparers: []agentrun.StepPreparer{runloop.NewRunStartPreparer(f.Store, f.Store)},
		// What ResolveControlAuthority grants the speaking Agent of a chat.
		Authority: agentrun.ControlAuthority{AgentID: agentID, CanAskUser: true, CanReact: true},
	}
	binding, err := f.Runner.ResolveTurnRunner(f.Ctx, run)
	if err != nil {
		f.T.Fatalf("resolve the chat binding of %s: %v", run.ID, err)
	}
	return binding
}

func (f *ChatFixture) Drive(run *models.AgentRun, binding *agentrun.TurnRunnerBinding) runloop.Exit {
	f.T.Helper()
	current, err := f.Store.GetAgentRun(f.Ctx, run.Scope, run.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	exit := f.Loop.Drive(f.Ctx, runloop.Claim{Run: current, WorkerID: ChatWorker, Binding: binding})
	if exit.Err != nil {
		f.T.Fatalf("drive %s: %v (exit %s)", run.ID, exit.Err, exit.Reason)
	}
	return exit
}

func (f *ChatFixture) Path(sessionID string) []*models.SessionEntry {
	f.T.Helper()
	path, err := f.Store.LoadAgentSessionPath(f.Ctx, f.Conversation.Scope, sessionID)
	if err != nil {
		f.T.Fatal(err)
	}
	return path.Entries
}

func (f *ChatFixture) AgentMessages() []*models.ChannelMessage {
	f.T.Helper()
	messages, err := f.Service.ListChannelMessages(f.Ctx, models.ChannelMessageFilter{Scope: f.Conversation.Scope, ConversationID: f.Conversation.ID, Limit: 100})
	if err != nil {
		f.T.Fatal(err)
	}
	var out []*models.ChannelMessage
	for _, message := range messages {
		if message.Sender.Type != models.ConversationParticipantUser {
			out = append(out, message)
		}
	}
	return out
}

func (f *ChatFixture) Run(id string) *models.AgentRun {
	f.T.Helper()
	run, err := f.Store.GetAgentRun(f.Ctx, f.Conversation.Scope, id)
	if err != nil {
		f.T.Fatal(err)
	}
	return run
}

// StepEntries appends one assistant entry with these tool calls (pending)
// for run, as a model step of the loop would.
func (f *ChatFixture) StepEntries(run *models.AgentRun, sessionID, stepID string, step int64, calls ...models.ModelToolCall) map[string]*models.SessionEntry {
	f.T.Helper()
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(sessionID, models.AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: step, Type: models.SessionEntryAssistant,
		Assistant: &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: "chat-model", StopReason: models.ModelStopToolCalls, ToolCalls: calls},
	}
	batch := []*models.SessionEntry{assistant}
	for _, call := range calls {
		batch = append(batch, &models.SessionEntry{
			ID: models.SessionEntryID(sessionID, models.ToolEntryKey(stepID, call.ID)), RunID: run.ID, StepID: stepID, Step: step,
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectWrite},
		})
	}
	lease := f.Lease()
	appended, err := f.Store.AppendAgentSessionEntries(f.Ctx, models.AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: sessionID, RunID: run.ID, Lease: &lease, Entries: batch, Now: f.Now,
	})
	if err != nil {
		f.T.Fatalf("append step: %v", err)
	}
	entries := map[string]*models.SessionEntry{}
	for _, entry := range appended.Entries {
		if entry.Type == models.SessionEntryTool {
			entries[entry.ToolCallID] = entry
		}
	}
	return entries
}

func (f *ChatFixture) Settle(run *models.AgentRun, entry *models.SessionEntry, to models.ToolEntryState, actionCallID string, result *models.ToolResult) *models.SessionEntry {
	f.T.Helper()
	transitioned, err := f.Store.TransitionToolEntry(f.Ctx, models.ToolEntryTransitionRequest{
		Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: f.Lease(),
		From: entry.State, ExpectedRevision: entry.Revision, To: to, ActionCallID: actionCallID, Result: result, Now: f.Now,
	})
	if err != nil {
		f.T.Fatalf("settle %s: %v", entry.ID, err)
	}
	return transitioned.Entry
}

func ChatEntryText(entry *models.SessionEntry) string {
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

// ChatShape is the path as "type:detail" items: the message id of user
// entries, the note kind, the Run of assistant and tool entries.
func ChatShape(entries []*models.SessionEntry) []string {
	shape := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch entry.Type {
		case models.SessionEntryUser:
			shape = append(shape, "user:"+entry.User.MessageID)
		case models.SessionEntryNote:
			shape = append(shape, "note:"+string(entry.Note.Kind)+":"+entry.RunID)
		default:
			shape = append(shape, string(entry.Type)+":"+entry.RunID)
		}
	}
	return shape
}

func ExpectChatShape(t *testing.T, entries []*models.SessionEntry, want ...string) {
	t.Helper()
	got := ChatShape(entries)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("path =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// PromotingChatStore promotes a chat Run past the foreground timeout.
type PromotingChatStore interface {
	PromoteConversationTask(ctx context.Context, req models.ConversationTaskPromotionRequest) (*models.ConversationTaskResult, error)
}

func (f *ChatFixture) Promote(run *models.AgentRun) {
	f.T.Helper()
	store, ok := f.Store.(PromotingChatStore)
	if !ok {
		f.T.Fatalf("%T cannot promote", f.Store)
	}
	result, err := store.PromoteConversationTask(f.Ctx, models.ConversationTaskPromotionRequest{
		Scope: run.Scope, RunID: run.ID, Now: run.CreatedAt.Add(models.ConversationTaskForegroundTimeout),
	})
	if err != nil || result == nil {
		f.T.Fatalf("promote %s: %#v, %v", run.ID, result, err)
	}
}

// ChatFinalStore is a copy of the conversation run tests' finalDeliveryStore; change them
// together.
type ChatFinalStore interface {
	conversationtest.ThreadStartStore
	models.AgentSessionStore
	models.ArtifactStore
}

// chatCoordinator routes every unaddressed message to agent-1. It is a copy
// of runtime's conversationRunTestCoordinator; change them together.
func chatCoordinator(t *testing.T, service *conversation.Service) *conversation.Coordinator {
	t.Helper()
	participants := conversation.ParticipantSourceFunc(func(context.Context, conversation.ParticipantQuery) ([]conversation.ParticipantBinding, error) {
		return []conversation.ParticipantBinding{{
			Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	router := conversation.RouterFunc(func(context.Context, conversation.RouteRequest) (conversation.RouteDecision, error) {
		return conversation.RouteDecision{AgentID: "agent-1", Reason: "owns the canary"}, nil
	})
	coordinator, err := conversation.NewCoordinator(service, participants, router, conversation.DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func (f *ChatFixture) ChatRunner(resolver conversation.TurnRunnerResolver, participation bool) *conversationrun.TurnRunner {
	f.T.Helper()
	runner, err := conversationrun.NewTurnRunner(f.Store, chatCoordinator(f.T, f.Service), conversationrun.TurnRunnerConfig{
		AgentTurns: resolver, RequireParticipationOptIn: participation,
	})
	if err != nil {
		f.T.Fatal(err)
	}
	return runner
}

func (f *ChatFixture) Scheduler() *conversationrun.Scheduler {
	f.T.Helper()
	scheduler, err := conversationrun.NewScheduler(f.Store, f.Store, conversationrun.SchedulerConfig{})
	if err != nil {
		f.T.Fatal(err)
	}
	return scheduler
}

func (f *ChatFixture) Schedule(message *models.ChannelMessage) *models.AgentRun {
	f.T.Helper()
	result, _, err := f.Scheduler().ScheduleMessage(f.Ctx, f.Conversation.Scope, f.Conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		f.T.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

// StartMember creates and claims agentID's member Run answering trigger in a
// Team chat, as the routing Run would (teamMemberRunRequest).
func (f *ChatFixture) StartMember(trigger *models.ChannelMessage, agentID string) *models.AgentRun {
	f.T.Helper()
	request := conversation.AgentRunRequest(f.Conversation, trigger)
	request.AssignedAgentID = agentID
	request.ConcurrencyKey = models.ConversationRunLane(f.Conversation.ID, "", agentID)
	request.SessionID = models.ConversationSessionID(f.Conversation.Scope, request.ConcurrencyKey)
	request.IdempotencyKey = "member-" + agentID + "-" + trigger.ID
	commands := agentrun.NewRunCommandService(f.Store, models.WithClock(func() time.Time { return f.Now }))
	result, err := commands.CreateAgentRun(f.Ctx, request)
	if err != nil {
		f.T.Fatalf("create member run: %v", err)
	}
	claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Conversation.Scope, WorkerID: ChatWorker, Now: f.Now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != result.Run.ID {
		f.T.Fatalf("claim = %#v, %v (want %s)", claimed, err, result.Run.ID)
	}
	return claimed
}

// BindMember is Bind for agentID's member Run.
func (f *ChatFixture) BindMember(run *models.AgentRun, agentID string, producer agentrun.StepProducer) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	return f.BindAgent(run, agentID, producer)
}

func (f *ChatFixture) EventTypes(runID string) []string {
	f.T.Helper()
	events, err := f.Store.ListActivity(f.Ctx, models.ActivityFilter{Scope: f.Conversation.Scope, RunID: runID, Limit: 500})
	if err != nil {
		f.T.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
	}
	return types
}
