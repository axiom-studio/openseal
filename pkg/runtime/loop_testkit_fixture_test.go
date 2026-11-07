package runtime

// Run-loop test kit, part 2 (run-loop section 12 §3.3): loopFixture drives the
// real RunLoop with the production bindings: the model step producer over the
// catalog preparer, the run-start preparer, the step tool dispatcher with the
// governed executor, the control executor and the host tool dispatcher, and
// the conversation final handler for chats. Only the model, the host tools,
// the action dispatcher and the action policy are fakes (part 1 and below).
//
// Scenarios assert what is visible: session entries, step rows, action calls,
// activity, channel messages and Run status. The kit never reads or writes
// run-level engine state (TestLoopTestkitHasNoCheckpointAccess).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

const (
	loopFixtureAgent      = "finn"
	loopFixtureUser       = "ana"
	loopFixtureWorker1    = "loop-worker-1"
	loopFixtureWorker2    = "loop-worker-2"
	loopFixtureLease      = time.Minute
	loopFixtureSkillID    = "fixture"
	loopFixtureSkillVer   = "1.0.0"
	loopFixtureBindingID  = "fixture-binding"
	loopFixtureDefinition = "finn-definition"
)

// ---------------------------------------------------------------------------
// Options

type loopFixtureConfig struct {
	rules         []*modelRule
	hostTools     map[string]hostToolHandler
	writeApproval bool
	contextWindow int64
	maxSteps      int
	team          []string
}

type loopOption func(*loopFixtureConfig)

func withRules(rules ...*modelRule) loopOption {
	return func(c *loopFixtureConfig) { c.rules = append(c.rules, rules...) }
}

// withHostTool answers the host tool operation name (HostToolRequest.Operation,
// e.g. "read_file"). A Run gets a chat Workspace, and so host tools, only when
// at least one host tool is registered.
func withHostTool(name string, h func(context.Context, HostToolRequest) (HostToolResult, error)) loopOption {
	return func(c *loopFixtureConfig) {
		if c.hostTools == nil {
			c.hostTools = map[string]hostToolHandler{}
		}
		c.hostTools[name] = h
	}
}

// withApprovalForWrites makes the policy allow reads and require approval
// (TTL 1h) for every write.
func withApprovalForWrites() loopOption {
	return func(c *loopFixtureConfig) { c.writeApproval = true }
}

func withContextWindow(tokens int64) loopOption {
	return func(c *loopFixtureConfig) { c.contextWindow = tokens }
}

// withMaxSteps bounds one claim (RunLoopConfig.MaxStepsPerClaim).
func withMaxSteps(n int) loopOption {
	return func(c *loopFixtureConfig) { c.maxSteps = n }
}

// withTeam makes chats Team chats: the conversation is owned by Team "team"
// and these Agents are its roster (the first one is the routed responder).
func withTeam(members ...string) loopOption {
	return func(c *loopFixtureConfig) { c.team = append(c.team, members...) }
}

// ---------------------------------------------------------------------------
// Fake action dispatcher and policy

type fakeDispatcher struct {
	t                     testing.TB
	mu                    sync.Mutex
	calls                 []ActionDispatchInput
	results               map[string]func(ActionDispatchInput) (map[string]interface{}, error) // by action name; overrides defaults
	block                 map[string]chan struct{}                                             // dispatch waits on it (crash scenario)
	inFlight, maxInFlight int
}

func newFakeDispatcher(t testing.TB) *fakeDispatcher {
	return &fakeDispatcher{t: t, results: map[string]func(ActionDispatchInput) (map[string]interface{}, error){}, block: map[string]chan struct{}{}}
}

// on overrides the result of one action.
func (d *fakeDispatcher) on(action string, result func(ActionDispatchInput) (map[string]interface{}, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results[action] = result
}

// hold makes dispatches of action wait until the returned channel is closed
// (or the dispatch context ends).
func (d *fakeDispatcher) hold(action string) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	release := make(chan struct{})
	d.block[action] = release
	return release
}

func (d *fakeDispatcher) Dispatch(ctx context.Context, in ActionDispatchInput) (map[string]interface{}, error) {
	action := ""
	if in.Bound != nil {
		action = in.Bound.Action.Name
	}
	d.mu.Lock()
	d.calls = append(d.calls, in)
	override := d.results[action]
	release := d.block[action]
	d.inFlight++
	if d.inFlight > d.maxInFlight {
		d.maxInFlight = d.inFlight
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inFlight--
		d.mu.Unlock()
	}()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if override != nil {
		return override(in)
	}
	id, _ := in.Arguments["id"].(string)
	switch action {
	case "lookup":
		return map[string]interface{}{"id": id, "status": "open", "title": "Issue " + id}, nil
	case "list_items":
		return map[string]interface{}{"items": []interface{}{"a", "b"}}, nil
	case "update_status":
		return map[string]interface{}{"id": id, "status": in.Arguments["status"]}, nil
	case "send":
		return map[string]interface{}{"sent": true}, nil
	case "explode":
		return nil, explicitActionResultFailure(map[string]interface{}{"success": false, "error": "the fixture service exploded"})
	}
	d.t.Errorf("fake dispatcher: unexpected action %q", action)
	return nil, fmt.Errorf("fake dispatcher: unknown action %q", action)
}

// count is the number of dispatches of action ("" = all).
func (d *fakeDispatcher) count(action string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, in := range d.calls {
		if action == "" || (in.Bound != nil && in.Bound.Action.Name == action) {
			n++
		}
	}
	return n
}

func (d *fakeDispatcher) peakInFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.maxInFlight
}

// fixturePolicy allows reads; writes need approval when writesNeedApproval;
// deny refuses actions by name.
type fixturePolicy struct {
	mu                 sync.Mutex
	writesNeedApproval bool
	deny               map[string]bool
}

func (p *fixturePolicy) evaluate(_ context.Context, in ActionPolicyInput) (ActionPolicyDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if in.Bound != nil && p.deny[in.Bound.Action.Name] {
		return ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "the fixture policy denies " + in.Bound.Action.Name}, nil
	}
	if in.Bound == nil || in.Bound.Action.SideEffect == skill.SideEffectRead || !p.writesNeedApproval {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}
	return ActionPolicyDecision{
		Disposition: ActionDispositionRequireApproval, Reason: "writes need review",
		EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: loopFixtureUser}}, ApprovalTTL: time.Hour,
	}, nil
}

func (p *fixturePolicy) denyAction(action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deny == nil {
		p.deny = map[string]bool{}
	}
	p.deny[action] = true
}

// fixtureSkillDefinition is fixture@1.0.0 (section 12 §3.3).
func fixtureSkillDefinition() *skill.Definition {
	str := map[string]interface{}{"type": "string"}
	object := func(required []string, props map[string]interface{}) map[string]interface{} {
		schema := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": props}
		if len(required) > 0 {
			list := make([]interface{}, len(required))
			for i, name := range required {
				list[i] = name
			}
			schema["required"] = list
		}
		return schema
	}
	read := func(name, description string, schema map[string]interface{}) skill.Action {
		return skill.Action{Name: name, Description: description, SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: schema, Idempotency: skill.IdempotencySupported}
	}
	external := func(name, description string, schema map[string]interface{}) skill.Action {
		return skill.Action{Name: name, Description: description, SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal, InputSchema: schema, Idempotency: skill.IdempotencySupported}
	}
	return &skill.Definition{
		ID: loopFixtureSkillID, Version: loopFixtureSkillVer, Name: "Fixture",
		Transport: skill.TransportReference{Kind: "http", Endpoint: "https://fixture.invalid"},
		Actions: map[string]skill.Action{
			"lookup":     read("lookup", "Look up an issue", object([]string{"id"}, map[string]interface{}{"id": str})),
			"list_items": read("list_items", "List the items", object(nil, map[string]interface{}{})),
			"update_status": external("update_status", "Change an issue's status", object([]string{"id", "status"},
				map[string]interface{}{"id": str, "status": str})),
			"send": external("send", "Send a message", object([]string{"to", "text"},
				map[string]interface{}{"to": str, "text": str})),
			"explode": read("explode", "A read that always fails", object(nil, map[string]interface{}{})),
		},
	}
}

// ---------------------------------------------------------------------------
// Fixture

type loopFixture struct {
	t        *testing.T
	ctx      context.Context
	raw      KernelStore // the store under the crash wrapper
	store    KernelStore // the crash store, which every service writes through
	crashes  *crashStore // the store's crash wrapper (the spec's crash field; crash() is the method)
	scope    Scope       // {tenant, "loop"}
	clock    *fakeClock
	catalog  *skill.Catalog
	model    *scriptedModel
	tools    *fakeHostTools
	dispatch *fakeDispatcher
	policy   *fixturePolicy
	actions  *ActionCoordinator
	approve  *ApprovalCoordinator
	executor *ActionWorker
	loop     *RunLoop
	convs    *ConversationService
	workerID string // "loop-worker-1"; crash() switches to "loop-worker-2"

	config     loopFixtureConfig
	portfolio  *PortfolioService
	scheduler  *ConversationRunScheduler
	runner     *ConversationRunTurnRunner
	control    *ControlExecutor
	dispatcher *StepToolDispatcher
	offered    []capability.ModelAction
	// chats is the raw store's conversation side. Conversation services
	// write through it, not through the crash store: a crash stands for a
	// dead loop worker, and channel posts by people go on.
	chats loopFixtureChatStore
}

// loopFixtureChatStore is what the chat services need of the store.
type loopFixtureChatStore interface {
	KernelStore
	ConversationStore
	RunCommandStore
}

// newLoopFixture builds the services over store (wrapped in a crash store)
// and registers fixture@1.0.0 for Agent "finn".
func newLoopFixture(t *testing.T, store KernelStore, opts ...loopOption) *loopFixture {
	t.Helper()
	config := loopFixtureConfig{contextWindow: kitDefaultContextWindow}
	for _, opt := range opts {
		opt(&config)
	}
	crash := newCrashStore(t, store)
	f := &loopFixture{
		t: t, ctx: context.Background(), raw: store, store: crash, crashes: crash,
		scope:    Scope{Kind: "tenant", ID: "loop"},
		clock:    newFakeClock(time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)),
		model:    newScriptedModel(t, config.rules...),
		tools:    newFakeHostTools(t),
		dispatch: newFakeDispatcher(t),
		policy:   &fixturePolicy{writesNeedApproval: config.writeApproval},
		workerID: loopFixtureWorker1,
		config:   config,
	}
	f.model.setContextWindow(config.contextWindow)
	for name, handler := range config.hostTools {
		f.tools.handle(name, handler)
	}

	f.catalog = skill.NewCatalog()
	if err := f.catalog.Register(f.ctx, fixtureSkillDefinition()); err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: loopFixtureBindingID, Scope: skillScope, DeploymentID: loopFixtureAgent, SkillID: loopFixtureSkillID, SkillVersion: loopFixtureSkillVer,
		AllowedActions: []string{"lookup", "list_items", "update_status", "send", "explode"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	offered, err := f.catalog.ListModelActions(f.ctx, skillScope, loopFixtureAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 5 {
		t.Fatalf("fixture offers %d actions, want 5", len(offered))
	}
	f.offered = offered

	chats, ok := store.(loopFixtureChatStore)
	if !ok {
		t.Fatalf("%T is not a conversation store", store)
	}
	f.chats = chats
	f.convs = NewConversationService(chats)
	f.convs.now = f.clock.Now
	f.portfolio = NewPortfolioService(f.store)
	f.portfolio.now = f.clock.Now
	scheduler, err := NewConversationRunScheduler(chats, chats, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.conversations.now = f.clock.Now
	scheduler.approvals.now = f.clock.Now
	f.scheduler = scheduler
	runner, err := NewConversationRunTurnRunner(chats, f.coordinator(), ConversationRunTurnRunnerConfig{AttachmentContent: &answerVisualContent{}})
	if err != nil {
		t.Fatal(err)
	}
	runner.now = f.clock.Now
	runner.conversations.now = f.clock.Now
	f.runner = runner
	f.build()
	return f
}

// coordinator routes every chat message to the fixture's acting Agent: finn,
// or the first Team member.
func (f *loopFixture) coordinator() *ConversationCoordinator {
	f.t.Helper()
	members := f.config.team
	if len(members) == 0 {
		members = []string{loopFixtureAgent}
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		bindings := make([]ConversationParticipantBinding, 0, len(members))
		for _, member := range members {
			bindings = append(bindings, ConversationParticipantBinding{
				Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: member}, SemanticRoles: []string{"assistant"},
			})
		}
		return bindings, nil
	})
	router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
		return ConversationRouteDecision{AgentID: members[0], Reason: "the fixture routes to the first member"}, nil
	})
	coordinator, err := NewConversationCoordinator(f.convs, participants, router, DefaultConversationCoordinatorConfig())
	if err != nil {
		f.t.Fatal(err)
	}
	return coordinator
}

// build wires the worker-bound services: the coordinators, the action
// worker, the governed and control executors, the tool dispatcher and the
// loop. crash() calls it again for the second worker.
func (f *loopFixture) build() {
	f.t.Helper()
	policy := ActionPolicyEvaluatorFunc(f.policy.evaluate)
	f.actions = NewActionCoordinator(f.store, f.store, f.catalog, policy)
	f.actions.now = f.clock.Now
	f.approve = NewApprovalCoordinator(f.store, f.store, ApprovalAuthorizerFunc(func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error { return nil }))
	f.approve.now = f.clock.Now
	f.executor = NewActionWorker(f.store, f.catalog, nil, ActionDispatcherFunc(f.dispatch.Dispatch))
	f.executor.now = f.clock.Now
	governed, err := NewGovernedToolExecutor(GovernedToolExecutorConfig{
		Actions: f.actions, Approvals: f.approve, Worker: f.executor, Store: f.store, Now: f.clock.Now,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	activity := NewRunActivityService(f.store, f.store)
	activity.now = f.clock.Now
	f.control = &ControlExecutor{
		Portfolio: f.store, Sessions: f.store.(AgentSessionStore), Activity: activity,
		Conversations: f.convs, Reporting: f.chats, Now: f.clock.Now,
	}
	if tasks, ok := f.store.(ConversationTaskKernelStore); ok {
		f.control.Tasks = NewConversationTaskService(tasks)
	}
	f.dispatcher, err = NewStepToolDispatcher(StepToolDispatcherConfig{
		Sessions: f.store.(AgentSessionStore), Turns: f.store, Actions: f.store, Coordinator: f.actions,
		Governed: governed, Control: f.control, Host: NewHostToolDispatcher(f.tools, f.actions, nil), Now: f.clock.Now,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.loop, err = NewRunLoop(f.store, f.actions, nil, RunLoopConfig{
		StepLeaseDuration: loopFixtureLease, MaxStepsPerClaim: f.config.maxSteps, Now: f.clock.Now,
		PlanToolEntries: f.planToolEntries, PlainTextFinal: f.control.PlainTextFinal,
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// planToolEntries is the loop's planner seam over the dispatcher's planners.
func (f *loopFixture) planToolEntries(binding *TurnRunnerBinding, session *AgentSession, step *AgentTurn, assistant *SessionEntry, calls []LoopToolCall) ([]*SessionEntry, error) {
	if assistant != nil {
		return PlanToolEntries(binding, assistant)
	}
	if session == nil || step == nil {
		return nil, errors.New("deterministic tool entries need a session and a step")
	}
	run, err := f.raw.GetAgentRun(f.ctx, step.Scope, step.RunID)
	if err != nil {
		return nil, err
	}
	return PlanToolCallEntries(binding, run, step, session.ID, calls)
}

// ---------------------------------------------------------------------------
// Starting work

// startChat creates a chat owned by Agent "finn" (Team "team" withTeam), posts
// text as user "ana" and schedules the Run through the production scheduler.
// A Team chat returns the routed member's Run.
func (f *loopFixture) startChat(text string) (*Conversation, *AgentRun) {
	f.t.Helper()
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent}
	if len(f.config.team) > 0 {
		owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}
	}
	conversation, _, err := f.convs.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.scope, Owner: owner, Title: "Loop fixture chat", IdempotencyKey: "loop-chat-" + fmt.Sprint(f.clock.Now().UnixNano()),
	})
	if err != nil {
		f.t.Fatalf("create conversation: %v", err)
	}
	message := f.post(conversation.ID, loopFixtureUser, text)
	run := f.chatRun(conversation, message)
	if conversation, err = f.convs.GetConversation(f.ctx, f.scope, conversation.ID); err != nil {
		f.t.Fatal(err)
	}
	return conversation, run
}

// chatRun is the Run that answers message: the scheduled Agent Run, or for a
// Team chat the member Run the routing Run would start (the routing producer
// is not on the loop yet).
func (f *loopFixture) chatRun(conversation *Conversation, message *ChannelMessage) *AgentRun {
	f.t.Helper()
	if len(f.config.team) == 0 {
		result, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversation.ID, message.ID)
		if err != nil || result == nil || result.Run == nil {
			f.t.Fatalf("schedule chat message: %#v, %v", result, err)
		}
		return result.Run
	}
	member := f.config.team[0]
	request := conversationAgentRunRequest(conversation, message)
	request.AssignedAgentID = member
	request.ConcurrencyKey = conversationRunLane(conversation.ID, "", member)
	request.SessionID = conversationSessionID(f.scope, request.ConcurrencyKey)
	request.IdempotencyKey = "member-" + member + "-" + message.ID
	created, err := f.portfolio.CreateAgentRun(f.ctx, request)
	if err != nil {
		f.t.Fatalf("create member run: %v", err)
	}
	return created
}

// startRun creates an objective Run of kind for finn with goal.
func (f *loopFixture) startRun(kind RunKind, goal string) *AgentRun {
	f.t.Helper()
	created, err := f.portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
		Scope: f.scope, Kind: kind, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent}, AssignedAgentID: loopFixtureAgent,
		Goal: goal, Source: RunSourceObjective,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	return created
}

// postUser posts text as sender and lets the production scheduler see it
// (a new Run, or steering for the lane's running Run).
func (f *loopFixture) postUser(conversationID, sender, text string) *ChannelMessage {
	f.t.Helper()
	message := f.post(conversationID, sender, text)
	if len(f.config.team) == 0 {
		if _, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversationID, message.ID); err != nil {
			f.t.Fatalf("schedule message: %v", err)
		}
	}
	return message
}

func (f *loopFixture) post(conversationID, sender, text string) *ChannelMessage {
	f.t.Helper()
	conversation, err := f.convs.GetConversation(f.ctx, f.scope, conversationID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.convs.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: sender}, Intent: MessageIntentQuestion,
		Content: text, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: fmt.Sprintf("post-%s-%d", sender, conversation.Revision),
	})
	if err != nil {
		f.t.Fatalf("post message: %v", err)
	}
	return posted.Message
}

// ---------------------------------------------------------------------------
// Driving

// claim claims the next Run of the scope as the current worker; it fails the
// test when another Run (or none) is claimed.
func (f *loopFixture) claim(runID string) *AgentRun {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
		Scope: f.scope, WorkerID: f.workerID, Now: f.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute,
	})
	if err != nil {
		f.t.Fatalf("claim %s: %v", runID, err)
	}
	if claimed == nil || claimed.ID != runID {
		got := "<none>"
		if claimed != nil {
			got = claimed.ID
		}
		f.t.Fatalf("claim %s claimed %s", runID, got)
	}
	return claimed
}

// runClaim claims the Run and drives one claim of the loop.
func (f *loopFixture) runClaim(runID string) (LoopExit, *AgentRun) {
	f.t.Helper()
	return f.runClaimContext(f.ctx, runID)
}

// LoopExit is the loop's exit of one claim (RunLoopExit).
type LoopExit = RunLoopExit

func (f *loopFixture) runClaimContext(ctx context.Context, runID string) (LoopExit, *AgentRun) {
	f.t.Helper()
	claimed := f.claim(runID)
	binding := f.binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.mustRun(runID)
	}
	exit := f.loop.Drive(ctx, RunLoopClaim{Run: claimed, WorkerID: f.workerID, Binding: binding})
	return exit, f.mustRun(runID)
}

// driveToRest repeats claims while the Run is back in the queue (yield,
// requeue, a park that was already woken), advancing the clock to a requeue's
// AvailableAt. It returns the Run once it is terminal or waiting.
func (f *loopFixture) driveToRest(runID string, maxClaims int) *AgentRun {
	f.t.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := f.runClaim(runID)
		if exit.Err != nil && exit.Reason != RunLoopExitRequeued {
			f.t.Fatalf("claim %d of %s exited %s: %v", i+1, runID, exit.Reason, exit.Err)
		}
		if run.Status != AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(f.clock.Now()) {
			f.clock.Advance(run.AvailableAt.Sub(f.clock.Now()) + time.Millisecond)
		}
	}
	run := f.mustRun(runID)
	f.t.Fatalf("run %s not at rest after %d claims: %s", runID, maxClaims, run.Status)
	return run
}

// binding is the claim's TurnRunnerBinding, built the way the worker's
// resolver will: the model producer over the catalog preparer, the run-start
// preparer (plus chat catch-up for chats), the claim tool executor, the
// control authority, and the conversation final handler for chats. A first
// claim opens the Run's session.
func (f *loopFixture) binding(run *AgentRun) *TurnRunnerBinding {
	f.t.Helper()
	sessions := f.store.(AgentSessionStore)
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	chat := conversationID != ""
	session := f.openSession(run, conversationID)

	var authority *workspace.Authority
	if len(f.config.hostTools) > 0 {
		workspaceID := conversationID
		if workspaceID == "" {
			workspaceID = "workspace-" + run.ID
		}
		authority = workspace.ChatAuthority(workspaceID)
	}
	control, err := ResolveControlAuthority(f.ctx, ControlAuthorityInput{
		Run: run, DeploymentID: run.AssignedAgentID, Runs: f.store, Conversations: f.chats,
	})
	if err != nil {
		f.t.Fatalf("resolve control authority: %v", err)
	}
	preparer := newCatalogModelStepPreparer(catalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
		HostTools:         HostToolSpecs(authority),
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
		Tools:     f.dispatcher.ForClaim(LoopToolClaim{Workspace: authority}),
	}
	if chat {
		binding.Preparers = append(binding.Preparers, &kitChatPreparer{f: f})
		binding.Final = newConversationFinalHandler(f.runner, sessions, f.store, f.store)
	}
	return binding
}

// openSession returns the Run's session, creating it on the first claim: a
// conversation session for a chat Run, else a run session seeded with the
// goal. (The worker's first-claim session creation does the same.)
func (f *loopFixture) openSession(run *AgentRun, conversationID string) *AgentSession {
	f.t.Helper()
	sessions := f.store.(AgentSessionStore)
	now := f.clock.Now()
	lease := AgentRunLeaseGuard{WorkerID: f.workerID, Now: now}
	switch {
	case run.SessionID != "" && conversationID == "":
		session, err := sessions.GetAgentSession(f.ctx, f.scope, run.SessionID)
		if err != nil {
			f.t.Fatalf("get session: %v", err)
		}
		return session
	case conversationID != "":
		opened, err := sessions.OpenConversationAgentSession(f.ctx, OpenConversationAgentSessionRequest{
			Scope: f.scope, ConversationID: conversationID, AgentID: run.AssignedAgentID, RunID: run.ID, Lease: lease, Now: now,
		})
		if err != nil {
			f.t.Fatalf("open conversation session: %v", err)
		}
		return opened.Session
	}
	goal := &SessionEntry{
		ID: SessionEntryID(run.ID, "goal"), Scope: f.scope, RunID: run.ID, Type: SessionEntryUser, SourceKey: "goal",
		User: &UserEntry{Origin: UserEntryGoal, Author: SessionAuthor{Type: "user", ID: loopFixtureUser},
			Content: []ContentPart{{Type: ContentPartText, Text: run.Goal}}},
	}
	created, err := sessions.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
		Scope: f.scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, InitialEntries: []*SessionEntry{goal}, Now: now,
	})
	if err != nil {
		f.t.Fatalf("create run session: %v", err)
	}
	return created.Session
}

// kitChatPreparer is the chat catch-up preparer: the visible channel messages
// up to the trigger (and any steering the step consumes) become user entries
// in the step's context append, through the production catch-up.
type kitChatPreparer struct{ f *loopFixture }

func (p *kitChatPreparer) BeforeStep(ctx context.Context, in StepInput, steering SteeringPlan) (*StepContextAppend, error) {
	f := p.f
	run, session := in.Run, in.Session
	if run == nil || session == nil {
		return nil, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := f.convs.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	trigger, err := f.convs.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return nil, err
	}
	catchUp := &conversationCatchUp{conversations: f.convs, runs: f.store}
	seed := in.Path == nil || len(in.Path.Entries) == 0
	result, err := catchUp.collect(ctx, catchUpRequest{
		Run: run, Session: session, Conversation: conversation, Trigger: trigger,
		Viewer:  ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}},
		Through: max(trigger.Sequence, steering.ThroughMessageSequence), Seed: seed, Step: in.Step.Sequence,
	})
	if err != nil {
		return nil, err
	}
	return result.stepContextAppend(session), nil
}

// decide resolves the Run's single pending approval as ana.
func (f *loopFixture) decide(runID string, decision ApprovalDecision, reason string) {
	f.t.Helper()
	pending, err := f.store.ListApprovals(f.ctx, ApprovalFilter{Scope: f.scope, RunID: runID, Status: []ApprovalStatus{ApprovalStatusPending}})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(pending) != 1 {
		f.t.Fatalf("run %s has %d pending approvals, want 1", runID, len(pending))
	}
	approval := pending[0]
	if _, err := f.approve.Resolve(f.ctx, ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "decision-" + approval.ID,
		Decision: decision, Principal: ApprovalPrincipal{Type: "user", ID: loopFixtureUser}, Reason: reason,
	}); err != nil {
		f.t.Fatalf("resolve approval %s: %v", approval.ID, err)
	}
}

// crash kills the current worker mid-claim: arm the store, cancel the claim
// ctx, wait for the claim to return, disarm, let the leases expire and
// rebuild the worker-bound services as "loop-worker-2" over the same store.
func (f *loopFixture) crash(ctxCancel context.CancelFunc, done <-chan struct{}) {
	f.t.Helper()
	f.crashes.arm()
	if ctxCancel != nil {
		ctxCancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Fatal("crashed claim did not return")
		}
	}
	f.crashes.disarm()
	f.clock.Advance(loopFixtureLease + time.Second)
	f.workerID = loopFixtureWorker2
	f.build()
}

// ---------------------------------------------------------------------------
// Reading what happened

func (f *loopFixture) mustRun(runID string) *AgentRun {
	f.t.Helper()
	run, err := f.raw.GetAgentRun(f.ctx, f.scope, runID)
	if err != nil || run == nil {
		f.t.Fatalf("get run %s: %#v, %v", runID, run, err)
	}
	return run
}

func (f *loopFixture) session(runID string) *Session {
	f.t.Helper()
	run := f.mustRun(runID)
	if run.SessionID == "" {
		f.t.Fatalf("run %s has no session", runID)
	}
	session, err := f.raw.(AgentSessionStore).GetAgentSession(f.ctx, f.scope, run.SessionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return session
}

// Session is the agent session log of a Run.
type Session = AgentSession

// entries is the Run's full session, ordered by sequence.
func (f *loopFixture) entries(runID string) []*SessionEntry {
	f.t.Helper()
	session := f.session(runID)
	var out []*SessionEntry
	var after int64
	for {
		page, err := f.raw.(AgentSessionStore).ListAgentSessionEntries(f.ctx, SessionEntryFilter{
			Scope: f.scope, SessionID: session.ID, AfterSequence: after, Limit: 1000,
		})
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, page...)
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].Sequence
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// entryTypes is the log shape: one string per entry, "tool:<state>" for tool
// entries.
func (f *loopFixture) entryTypes(runID string) []string {
	f.t.Helper()
	entries := f.entries(runID)
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = string(entry.Type)
		if entry.Type == SessionEntryTool {
			out[i] = "tool:" + string(entry.State)
		}
	}
	return out
}

func (f *loopFixture) steps(runID string) []*AgentTurn {
	f.t.Helper()
	steps, err := f.raw.ListAgentTurns(f.ctx, AgentTurnFilter{Scope: f.scope, RunID: runID})
	if err != nil {
		f.t.Fatal(err)
	}
	return steps
}

func (f *loopFixture) calls(runID string) []*ActionCall {
	f.t.Helper()
	calls, err := f.raw.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: runID})
	if err != nil {
		f.t.Fatal(err)
	}
	return calls
}

// events is the Run's activity in sequence order, of these types when given.
// It pages ascending and filters here: the stores' ascending reads take only
// the Run and the sequence.
func (f *loopFixture) events(runID string, types ...string) []*ActivityEvent {
	f.t.Helper()
	want := map[string]bool{}
	for _, eventType := range types {
		want[eventType] = true
	}
	const page = 500
	var out []*ActivityEvent
	var after int64
	for {
		events, err := f.raw.ListActivity(f.ctx, ActivityFilter{Scope: f.scope, RunID: runID, AfterSequence: after, Limit: page})
		if err != nil {
			f.t.Fatal(err)
		}
		for _, event := range events {
			if len(want) == 0 || want[event.EventType] {
				out = append(out, event)
			}
		}
		if len(events) < page {
			return out
		}
		after = events[len(events)-1].Sequence
	}
}

// messages is the conversation's channel, oldest first.
func (f *loopFixture) messages(conversationID string) []*ChannelMessage {
	f.t.Helper()
	messages, err := f.convs.ListChannelMessages(f.ctx, ChannelMessageFilter{Scope: f.scope, ConversationID: conversationID, Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	return messages
}

func (f *loopFixture) mustStatus(run *AgentRun, want AgentRunStatus) {
	f.t.Helper()
	current := f.mustRun(run.ID)
	if current.Status != want {
		f.t.Fatalf("run %s status = %s, want %s (failure: %#v)\nlog: %v", run.ID, current.Status, want, current.FailureExplanation, f.entryTypes(run.ID))
	}
}

// ---------------------------------------------------------------------------
// Kit self-tests of the fixture

// TestLoopTestkitHasNoCheckpointAccess keeps the kit and the scenarios on
// what the model sees: none of them may touch run-level engine state or the
// old hosted turn form. The child scenario drives runbook engine state and is
// exempt by name.
func TestLoopTestkitHasNoCheckpointAccess(t *testing.T) {
	forbidden := []string{
		"Continuation" + "Checkpoint",
		".Check" + "point[",
		"HostedTurn" + "Form",
		"_open" + "seal",
		"_at" + "las",
	}
	exempt := map[string]bool{"loop_scenario_child_test.go": true}
	var files []string
	for _, pattern := range []string{"loop_testkit_*_test.go", "loop_testkit_test.go", "loop_scenario_*_test.go"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) < 3 {
		t.Fatalf("kit files = %v, want at least the model, fixture and self-test files", files)
	}
	for _, name := range files {
		if exempt[name] {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range forbidden {
			if strings.Contains(string(raw), needle) {
				t.Errorf("%s contains %q: the kit must not read old-path state", name, needle)
			}
		}
	}
}

func TestLoopTestkitFixtureObjectiveRunReadsThenAnswers(t *testing.T) {
	eachStore(t, "kit_fixture_objective", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "lookup", times: 1,
				reply: func(v stepView) ModelStepResult {
					return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
				}},
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				for _, e := range v.ToolResultsSinceUser() {
					if strings.Contains(kitPartsText(e.Tool.Result.Content), "Issue 7") {
						return finalAnswer(map[string]any{"answer": "Issue 7 is open."})
					}
				}
				return say("I could not find it.")
			}},
		))
		run := f.startRun(RunKindAgentWork, "Check issue 7")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if got, want := strings.Join(f.entryTypes(run.ID), ","), "user,assistant,tool:completed,assistant,tool:completed"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		if f.dispatch.count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.dispatch.count("lookup"))
		}
		if calls := f.calls(run.ID); len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded {
			t.Fatalf("action calls = %#v", calls)
		}
		if steps := f.steps(run.ID); len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if len(f.events(run.ID, "turn.narration")) != 1 {
			t.Fatalf("narration events = %d, want 1", len(f.events(run.ID, "turn.narration")))
		}
		if users := f.model.calls(ModelStepPurposeStep)[0]; !strings.Contains(stepView{req: users}.LastUserText(), "Check issue 7") {
			t.Fatalf("first step did not see the goal: %s", stepView{req: users}.describe())
		}
	})
}

func TestLoopTestkitFixtureChatPostsTheAnswer(t *testing.T) {
	eachStore(t, "kit_fixture_chat", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "hello",
			when:  func(v stepView) bool { return strings.Contains(v.LastUserText(), "hello") },
			reply: func(stepView) ModelStepResult { return say("Hi Ana.") }}))
		conversation, run := f.startChat("hello finn")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		messages := f.messages(conversation.ID)
		if len(messages) != 2 || messages[1].Sender.ID != loopFixtureAgent || !strings.Contains(messages[1].Content, "Hi Ana.") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
		if first := (stepView{req: f.model.calls(ModelStepPurposeStep)[0]}); !strings.Contains(first.LastUserText(), "hello finn") {
			t.Fatalf("first step did not see the trigger: %s", first.describe())
		}
	})
}

func TestLoopTestkitFixtureApprovalParksAndResumes(t *testing.T) {
	eachStore(t, "kit_fixture_approval", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "update", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Closing it.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "7", "status": "closed"}, "Close issue 7"))
			}},
			&modelRule{name: "done", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Closed issue 7."}) }},
		))
		run := f.startRun(RunKindAgentWork, "Close issue 7")
		parked := f.driveToRest(run.ID, 3)
		f.mustStatus(parked, AgentRunStatusWaitingForApproval)
		if f.dispatch.count("update_status") != 0 {
			t.Fatal("the write ran before approval")
		}
		f.decide(run.ID, ApprovalDecisionApprove, "go ahead")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", f.dispatch.count("update_status"))
		}
	})
}

func TestLoopTestkitFixtureCrashResumesOnSecondWorker(t *testing.T) {
	eachStore(t, "kit_fixture_crash", func(t *testing.T, store KernelStore) {
		entered := make(chan struct{})
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "lookup", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "9"}))
			}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Issue 9 is open."}) }},
		))
		release := f.dispatch.hold("lookup")
		f.dispatch.on("lookup", func(in ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"id": "9", "status": "open", "title": "Issue 9"}, nil
		})
		go func() {
			for f.dispatch.count("lookup") == 0 {
				time.Sleep(time.Millisecond)
			}
			close(entered)
		}()
		run := f.startRun(RunKindAgentWork, "Check issue 9")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runClaimContext(ctx, run.ID)
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch never started")
		}
		f.crash(cancel, done)
		close(release)
		if f.workerID != loopFixtureWorker2 {
			t.Fatalf("worker after crash = %s", f.workerID)
		}
		final := f.driveToRest(run.ID, 4)
		f.mustStatus(final, AgentRunStatusCompleted)
		if final.LeaseOwner != "" && final.LeaseOwner != loopFixtureWorker2 {
			t.Fatalf("lease owner = %s", final.LeaseOwner)
		}
	})
}

// reviewedCall calls an external (reviewed) governed action: its arguments go
// in the review envelope the governed schema offers, with summary as the
// review summary and content.
func reviewedCall(name string, args map[string]any, summary string) tc {
	return call(name, map[string]any{
		"arguments":     args,
		"reviewContext": map[string]any{"summary": summary, "content": summary},
	})
}

// kitGovernedTool is the offered tool name of a fixture action.
func kitGovernedTool(v stepView, action string) string {
	for _, tool := range v.req.Tools {
		if strings.HasSuffix(tool.Name, action) && strings.HasPrefix(tool.Name, governedToolPrefix) {
			return tool.Name
		}
	}
	return governedToolPrefix + action
}

func kitDescribeMessages(messages []*ChannelMessage) string {
	parts := make([]string, len(messages))
	for i, m := range messages {
		raw, _ := json.Marshal(map[string]string{"from": m.Sender.ID, "text": m.Content})
		parts[i] = string(raw)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
