package runloop_test

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

// copied from conversation_loop_fixture_test.go
type chatLoopFixture struct {
	t            *testing.T
	ctx          context.Context
	store        chatLoopStore
	service      *conversation.ConversationService
	conversation *models.Conversation
	runner       *conversationrun.ConversationRunTurnRunner
	actions      *chatLoopActions
	loop         *runloop.RunLoop
	team         bool
	now          time.Time
	// agents is the runner's AgentTurns resolver: the bare Agent binding of
	// each Run, before the chat seams are added.
	agents chatLoopAgentTurns
}

// copied from conversation_loop_fixture_test.go
// newRunner is a conversation turn runner over the fixture's store, with
// config and the fixture's Agent bindings as its AgentTurns resolver.
func (f *chatLoopFixture) newRunner(config conversationrun.ConversationRunTurnRunnerConfig) *conversationrun.ConversationRunTurnRunner {
	f.t.Helper()
	config.AgentTurns = f.agents
	runner, err := conversationrun.NewConversationRunTurnRunner(chatLoopRunnerStore(f.t, f.store, f.actions), conversationRunTestCoordinator(f.t, f.service), config)
	if err != nil {
		f.t.Fatal(err)
	}
	return runner
}

// copied from conversation_loop_fixture_test.go
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

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) lease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: chatLoopWorker, Now: f.now}
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) post(key, userID, content string) *models.ChannelMessage {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.conversation.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(f.ctx, conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: userID}, Intent: models.MessageIntentQuestion,
		Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
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

// copied from conversation_loop_fixture_test.go
// startRun creates and claims the chat Run that answers trigger.
func (f *chatLoopFixture) startRun(trigger *models.ChannelMessage) *models.AgentRun {
	f.t.Helper()
	request := conversation.ConversationAgentRunRequest(f.conversation, trigger)
	if f.team {
		request.AssignedAgentID = "agent-1"
		request.ConcurrencyKey = models.ConversationRunLane(f.conversation.ID, "", "agent-1")
		request.SessionID = models.ConversationSessionID(f.conversation.Scope, request.ConcurrencyKey)
		request.IdempotencyKey = "member-" + trigger.ID
	}
	commands := agentrun.NewRunCommandService(f.store, models.WithClock(func() time.Time { return f.now }))
	result, err := commands.CreateAgentRun(f.ctx, request)
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	created := result.Run
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, models.AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: chatLoopWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, created.ID)
	}
	return claimed
}

// copied from conversation_loop_fixture_test.go
// bind resolves agent-1's binding for run.
func (f *chatLoopFixture) bind(run *models.AgentRun, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	return f.bindAgent(run, "agent-1", producer, actions...)
}

// copied from conversation_loop_fixture_test.go
// bindAgent resolves run's binding through the conversation turn runner, as
// the worker does: the runner adds the chat seams to a bare binding of
// agentID with producer and actions.
func (f *chatLoopFixture) bindAgent(run *models.AgentRun, agentID string, producer agentrun.StepProducer, actions ...capability.ModelAction) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	f.agents[run.ID] = &agentrun.TurnRunnerBinding{
		DeploymentID: agentID, DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(models.ModelStepProviderAnthropic), Model: "chat-model", ModelActions: actions,
		Producer: producer, Preparers: []agentrun.StepPreparer{runloop.NewRunStartPreparer(f.store, f.store)},
		// What ResolveControlAuthority grants the speaking Agent of a chat.
		Authority: agentrun.ControlAuthority{AgentID: agentID, CanAskUser: true, CanReact: true},
	}
	binding, err := f.runner.ResolveTurnRunner(f.ctx, run)
	if err != nil {
		f.t.Fatalf("resolve the chat binding of %s: %v", run.ID, err)
	}
	return binding
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) drive(run *models.AgentRun, binding *agentrun.TurnRunnerBinding) runloop.RunLoopExit {
	f.t.Helper()
	current, err := f.store.GetAgentRun(f.ctx, run.Scope, run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	exit := f.loop.Drive(f.ctx, runloop.RunLoopClaim{Run: current, WorkerID: chatLoopWorker, Binding: binding})
	if exit.Err != nil {
		f.t.Fatalf("drive %s: %v (exit %s)", run.ID, exit.Err, exit.Reason)
	}
	return exit
}

// copied from conversation_loop_fixture_test.go
// prepare runs the loop's phase 3 for run as the next step would.
func (f *chatLoopFixture) prepare(run *models.AgentRun, binding *agentrun.TurnRunnerBinding, step int64) *runloop.StepContextResult {
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
		Step: &models.AgentTurn{ID: fmt.Sprintf("%s-step-%d", run.ID, step), Sequence: step},
	}
	result, err := runloop.AppendStepContext(f.ctx, f.store, in, binding.Preparers, f.now)
	if err != nil {
		f.t.Fatalf("step context of %s: %v", run.ID, err)
	}
	return result
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) path(sessionID string) []*models.SessionEntry {
	f.t.Helper()
	path, err := f.store.LoadAgentSessionPath(f.ctx, f.conversation.Scope, sessionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return path.Entries
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) agentMessages() []*models.ChannelMessage {
	f.t.Helper()
	messages, err := f.service.ListChannelMessages(f.ctx, models.ChannelMessageFilter{Scope: f.conversation.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []*models.ChannelMessage
	for _, message := range messages {
		if message.Sender.Type != models.ConversationParticipantUser {
			out = append(out, message)
		}
	}
	return out
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) run(id string) *models.AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.conversation.Scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// copied from conversation_loop_fixture_test.go
// stepEntries appends one assistant entry with these tool calls (pending)
// for run, as a model step of the loop would.
func (f *chatLoopFixture) stepEntries(run *models.AgentRun, sessionID, stepID string, step int64, calls ...models.ModelToolCall) map[string]*models.SessionEntry {
	f.t.Helper()
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
	lease := f.lease()
	appended, err := f.store.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: sessionID, RunID: run.ID, Lease: &lease, Entries: batch, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append step: %v", err)
	}
	entries := map[string]*models.SessionEntry{}
	for _, entry := range appended.Entries {
		if entry.Type == models.SessionEntryTool {
			entries[entry.ToolCallID] = entry
		}
	}
	return entries
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) settle(run *models.AgentRun, entry *models.SessionEntry, to models.ToolEntryState, actionCallID string, result *models.ToolResult) *models.SessionEntry {
	f.t.Helper()
	transitioned, err := f.store.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: f.lease(),
		From: entry.State, ExpectedRevision: entry.Revision, To: to, ActionCallID: actionCallID, Result: result, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("settle %s: %v", entry.ID, err)
	}
	return transitioned.Entry
}

// copied from conversation_loop_fixture_test.go
func (f *chatLoopFixture) promote(run *models.AgentRun) {
	f.t.Helper()
	store, ok := f.store.(promotingChatLoopStore)
	if !ok {
		f.t.Fatalf("%T cannot promote", f.store)
	}
	result, err := store.PromoteConversationTask(f.ctx, models.ConversationTaskPromotionRequest{
		Scope: run.Scope, RunID: run.ID, Now: run.CreatedAt.Add(models.ConversationTaskForegroundTimeout),
	})
	if err != nil || result == nil {
		f.t.Fatalf("promote %s: %#v, %v", run.ID, result, err)
	}
}

// copied from conversation_loop_fixture_test.go
const chatLoopWorker = "chat-loop-worker"

// copied from conversation_loop_fixture_test.go
func forChatLoopStores(t *testing.T, options chatLoopOptions, test func(*testing.T, *chatLoopFixture)) {
	t.Helper()
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		test(t, newChatLoopFixture(t, base, options))
	})
}

// copied from conversation_loop_fixture_test.go
type chatLoopOptions struct {
	team          bool
	participation bool // RequireParticipationOptIn with participation off
}

// copied from conversation_loop_fixture_test.go
// chatLoopProducer is a model producer that records the session path each
// step renders from and answers with the next scripted text.
type chatLoopProducer struct {
	answers []string
	paths   [][]*models.SessionEntry
}

// copied from conversation_loop_fixture_test.go
func (*chatLoopProducer) Kind() models.StepProducerKind { return models.StepProducerModel }

// copied from conversation_loop_fixture_test.go
func (p *chatLoopProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	var entries []*models.SessionEntry
	if in.Path != nil {
		entries = append(entries, in.Path.Entries...)
	}
	p.paths = append(p.paths, entries)
	answer := "done"
	if len(p.answers) > 0 {
		answer, p.answers = p.answers[0], p.answers[1:]
	}
	return agentrun.StepOutput{Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "chat-model", Text: answer, StopReason: models.ModelStopEnd,
	}}, nil
}

// copied from conversation_loop_fixture_test.go
func chatLoopExpectShape(t *testing.T, entries []*models.SessionEntry, want ...string) {
	t.Helper()
	got := chatLoopShape(entries)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("path =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// copied from conversation_loop_fixture_test.go
func chatLoopEntryText(entry *models.SessionEntry) string {
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

// copied from conversation_loop_fixture_test.go
type chatLoopStore interface {
	chatLoopFinalStore
	models.KernelStore
	models.RunCommandStore
}

// copied from conversation_loop_fixture_test.go
// chatLoopActions is the governed call and approval store the seams read.
type chatLoopActions struct {
	mu        sync.Mutex
	calls     map[string]*models.ActionCall
	approvals map[string]*models.ApprovalCheckpoint
}

// copied from conversation_loop_fixture_test.go
func (a *chatLoopActions) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

// copied from conversation_loop_fixture_test.go
func (a *chatLoopActions) GetApproval(_ context.Context, _ models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	approval, ok := a.approvals[id]
	if !ok {
		return nil, models.ErrApprovalNotFound
	}
	copied := *approval
	return &copied, nil
}

// copied from conversation_loop_fixture_test.go
// chatLoopAgentTurns is the bare Agent binding bindAgent registered for each
// Run, by Run id.
type chatLoopAgentTurns map[string]*agentrun.TurnRunnerBinding

// copied from conversation_loop_fixture_test.go
func (a chatLoopAgentTurns) ResolveTurnRunner(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	binding, ok := a[run.ID]
	if !ok {
		return nil, fmt.Errorf("no Agent binding registered for Run %s", run.ID)
	}
	return binding, nil
}

// copied from conversation_loop_fixture_test.go
func chatLoopRunnerStore(t *testing.T, store chatLoopStore, actions *chatLoopActions) models.ConversationStore {
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

// copied from conversation_run_test.go
// conversationRunTestCoordinator routes every unaddressed message to agent-1.
func conversationRunTestCoordinator(t *testing.T, service *conversation.ConversationService) *conversation.ConversationCoordinator {
	t.Helper()
	participants := conversation.ConversationParticipantSourceFunc(func(context.Context, conversation.ConversationParticipantQuery) ([]conversation.ConversationParticipantBinding, error) {
		return []conversation.ConversationParticipantBinding{{
			Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	router := conversation.ConversationRouterFunc(func(context.Context, conversation.ConversationRouteRequest) (conversation.ConversationRouteDecision, error) {
		return conversation.ConversationRouteDecision{AgentID: "agent-1", Reason: "owns the canary"}, nil
	})
	coordinator, err := conversation.NewConversationCoordinator(service, participants, router, conversation.DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

// copied from conversation_loop_fixture_test.go
// promotingChatLoopStore promotes a chat Run past the foreground timeout.
type promotingChatLoopStore interface {
	PromoteConversationTask(ctx context.Context, req models.ConversationTaskPromotionRequest) (*models.ConversationTaskResult, error)
}

// copied from conversation_loop_fixture_test.go
func newChatLoopFixture(t *testing.T, base conversationtest.ThreadStartStore, options chatLoopOptions) *chatLoopFixture {
	t.Helper()
	store, ok := base.(chatLoopStore)
	if !ok {
		t.Fatalf("%T is not a kernel store with the session log", base)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "chat-loop-" + uuid.NewString()[:8]}
	service := conversation.NewConversationService(store)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	if options.team {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-1"}
	}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Chat loop", IdempotencyKey: "chat-loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := runloop.NewRunLoop(store, nil, nil, runloop.RunLoopConfig{
		PlanToolEntries: func(*agentrun.TurnRunnerBinding, *models.AgentSession, *models.AgentTurn, *models.SessionEntry, []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
			return nil, fmt.Errorf("chat loop tests make no tool calls")
		},
		PlainTextFinal: func(_ context.Context, cc agentrun.ControlContext, text string) (control.ControlResult, error) {
			return control.ControlResult{Kind: control.ControlFinal, Final: &agentrun.FinalAnswer{
				StepID: cc.Step.ID, PlainText: true, Answer: text, Output: map[string]interface{}{conversationrun.FinalOutputSummaryKey: text},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &chatLoopFixture{
		t: t, ctx: ctx, store: store, service: service, conversation: conversation, loop: loop, team: options.team,
		actions: &chatLoopActions{calls: map[string]*models.ActionCall{}, approvals: map[string]*models.ApprovalCheckpoint{}},
		now:     time.Now().UTC().Truncate(time.Millisecond),
		agents:  chatLoopAgentTurns{},
	}
	f.runner = f.newRunner(conversationrun.ConversationRunTurnRunnerConfig{RequireParticipationOptIn: options.participation})
	return f
}

// copied from conversation_loop_fixture_test.go
// chatLoopShape is the path as "type:detail" items: the message id of user
// entries, the note kind, the Run of assistant and tool entries.
func chatLoopShape(entries []*models.SessionEntry) []string {
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

// copied from conversation_loop_fixture_test.go
// chatLoopFinalStore is a copy of the conversation run tests' finalDeliveryStore; change them
// together.
type chatLoopFinalStore interface {
	conversationtest.ThreadStartStore
	models.AgentSessionStore
	models.ArtifactStore
}

// copied from conversation_loop_fixture_test.go
// sqliteChatLoopStore and postgresChatLoopStore are the fixture store as the
// turn runner sees it: the chat seams read governed calls and approvals from
// chatLoopActions, everything else from the store itself.
type sqliteChatLoopStore struct {
	*sqlite.Store
	actions *chatLoopActions
}

// copied from conversation_loop_fixture_test.go
func (s *sqliteChatLoopStore) GetActionCall(ctx context.Context, scope models.Scope, id string) (*models.ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

// copied from conversation_loop_fixture_test.go
func (s *sqliteChatLoopStore) GetApproval(ctx context.Context, scope models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}

// copied from conversation_loop_fixture_test.go
type postgresChatLoopStore struct {
	*postgres.Store
	actions *chatLoopActions
}

// copied from conversation_loop_fixture_test.go
func (s *postgresChatLoopStore) GetActionCall(ctx context.Context, scope models.Scope, id string) (*models.ActionCall, error) {
	return s.actions.GetActionCall(ctx, scope, id)
}

// copied from conversation_loop_fixture_test.go
func (s *postgresChatLoopStore) GetApproval(ctx context.Context, scope models.Scope, id string) (*models.ApprovalCheckpoint, error) {
	return s.actions.GetApproval(ctx, scope, id)
}
