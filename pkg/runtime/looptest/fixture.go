package looptest

// Run-loop test kit, part 2 (run-loop section 12 §3.3): Fixture drives the
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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

const (
	FixtureAgent      = "finn"
	FixtureUser       = "ana"
	FixtureWorker1    = "loop-worker-1"
	FixtureWorker2    = "loop-worker-2"
	FixtureLease      = time.Minute
	FixtureSkillID    = "fixture"
	FixtureSkillVer   = "1.0.0"
	FixtureBindingID  = "fixture-binding"
	FixtureDefinition = "finn-definition"
)

// ---------------------------------------------------------------------------
// Options

type FixtureConfig struct {
	rules         []*ModelRule
	hostTools     map[string]hostToolHandler
	writeApproval bool
	contextWindow int64
	maxSteps      int
	Team          []string
	credentials   action.CredentialResolver
}

type Option func(*FixtureConfig)

func WithRules(rules ...*ModelRule) Option {
	return func(c *FixtureConfig) { c.rules = append(c.rules, rules...) }
}

// WithHostTool answers the host tool operation name (HostToolRequest.Operation,
// e.g. "read_file"). A Run gets a chat Workspace, and so host tools, only when
// at least one host tool is registered.
func WithHostTool(name string, h func(context.Context, models.HostToolRequest) (models.HostToolResult, error)) Option {
	return func(c *FixtureConfig) {
		if c.hostTools == nil {
			c.hostTools = map[string]hostToolHandler{}
		}
		c.hostTools[name] = h
	}
}

// WithApprovalForWrites makes the policy allow reads and require approval
// (TTL 1h) for every write.
func WithApprovalForWrites() Option {
	return func(c *FixtureConfig) { c.writeApproval = true }
}

func WithContextWindow(tokens int64) Option {
	return func(c *FixtureConfig) { c.contextWindow = tokens }
}

// WithMaxSteps bounds one claim (RunLoopConfig.MaxStepsPerClaim).
func WithMaxSteps(n int) Option {
	return func(c *FixtureConfig) { c.maxSteps = n }
}

// WithTeam makes chats Team chats: the conversation is owned by Team "team"
// and these Agents are its roster (the first one is the routed responder).
func WithTeam(members ...string) Option {
	return func(c *FixtureConfig) { c.Team = append(c.Team, members...) }
}

// WithCredentials gives the action worker a credential resolver.
func WithCredentials(resolver action.CredentialResolver) Option {
	return func(c *FixtureConfig) { c.credentials = resolver }
}

// ---------------------------------------------------------------------------
// Fake action dispatcher and policy

type FakeDispatcher struct {
	t                     testing.TB
	Mu                    sync.Mutex
	Calls                 []agentrun.ActionDispatchInput
	results               map[string]func(agentrun.ActionDispatchInput) (map[string]interface{}, error) // by action name; overrides defaults
	block                 map[string]chan struct{}                                                      // dispatch waits on it (crash scenario)
	inFlight, maxInFlight int
}

func newFakeDispatcher(t testing.TB) *FakeDispatcher {
	return &FakeDispatcher{t: t, results: map[string]func(agentrun.ActionDispatchInput) (map[string]interface{}, error){}, block: map[string]chan struct{}{}}
}

// On overrides the result of one action.
func (d *FakeDispatcher) On(action string, result func(agentrun.ActionDispatchInput) (map[string]interface{}, error)) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.results[action] = result
}

// Hold makes dispatches of action wait until the returned channel is closed
// (or the dispatch context ends).
func (d *FakeDispatcher) Hold(action string) chan struct{} {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	release := make(chan struct{})
	d.block[action] = release
	return release
}

func (d *FakeDispatcher) Dispatch(ctx context.Context, in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	action := ""
	if in.Bound != nil {
		action = in.Bound.Action.Name
	}
	d.Mu.Lock()
	d.Calls = append(d.Calls, in)
	override := d.results[action]
	release := d.block[action]
	d.inFlight++
	if d.inFlight > d.maxInFlight {
		d.maxInFlight = d.inFlight
	}
	d.Mu.Unlock()
	defer func() {
		d.Mu.Lock()
		d.inFlight--
		d.Mu.Unlock()
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
		return nil, errors.New("the fixture service exploded")
	}
	d.t.Errorf("fake dispatcher: unexpected action %q", action)
	return nil, fmt.Errorf("fake dispatcher: unknown action %q", action)
}

// Count is the number of dispatches of action ("" = all).
func (d *FakeDispatcher) Count(action string) int {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	n := 0
	for _, in := range d.Calls {
		if action == "" || (in.Bound != nil && in.Bound.Action.Name == action) {
			n++
		}
	}
	return n
}

func (d *FakeDispatcher) peakInFlight() int {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	return d.maxInFlight
}

// fixturePolicy allows reads; writes need approval when writesNeedApproval;
// deny refuses actions by name.
type fixturePolicy struct {
	mu                 sync.Mutex
	writesNeedApproval bool
	deny               map[string]bool
}

func (p *fixturePolicy) evaluate(_ context.Context, in action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if in.Bound != nil && p.deny[in.Bound.Action.Name] {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionDeny, Reason: "the fixture policy denies " + in.Bound.Action.Name}, nil
	}
	if in.Bound == nil || in.Bound.Action.SideEffect == skill.SideEffectRead || !p.writesNeedApproval {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
	}
	return action.ActionPolicyDecision{
		Disposition: action.ActionDispositionRequireApproval, Reason: "writes need review",
		EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: FixtureUser}}, ApprovalTTL: time.Hour,
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
		ID: FixtureSkillID, Version: FixtureSkillVer, Name: "Fixture",
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

type Fixture struct {
	T        *testing.T
	Ctx      context.Context
	Raw      models.KernelStore // the store under the crash wrapper
	Store    models.KernelStore // the crash store, which every service writes through
	Crashes  *CrashStore        // the store's crash wrapper (the spec's crash field; crash() is the method)
	Scope    models.Scope       // {tenant, "loop"}
	Clock    *fakeClock
	Catalog  *skill.Catalog
	Model    *ScriptedModel
	Tools    *FakeHostTools
	Dispatch *FakeDispatcher
	policy   *fixturePolicy
	Actions  *action.ActionCoordinator
	Approve  *action.ApprovalCoordinator
	Executor *action.ActionWorker
	Loop     *runloop.RunLoop
	Convs    *runtimeconversation.ConversationService
	WorkerID string // "loop-worker-1"; crash() switches to "loop-worker-2"

	Config     FixtureConfig
	Portfolio  *portfolio.PortfolioService
	Scheduler  *conversationrun.ConversationRunScheduler
	Runner     *conversationrun.ConversationRunTurnRunner
	Control    *control.ControlExecutor
	Dispatcher *runloop.StepToolDispatcher
	Offered    []capability.ModelAction
	// chats is the raw store's conversation side. Conversation services
	// write through it, not through the crash store: a crash stands for a
	// dead loop worker, and channel posts by people go on.
	Chats loopFixtureChatStore
}

// loopFixtureChatStore is what the chat services need of the store.
type loopFixtureChatStore interface {
	models.KernelStore
	models.ConversationStore
	models.RunCommandStore
}

// loopFixtureCapabilities pins the Skills the scenarios bind, so they call
// their actions without loading them first (loading is always deferred).
var loopFixtureCapabilities = modelstep.CapabilitySetConfig{PinnedSkillIDs: []string{
	FixtureSkillID, models.SkillManagementSkillID, "mail", "openseal.connector",
}}

// NewFixture builds the services over store (wrapped in a crash store)
// and registers fixture@1.0.0 for Agent "finn".
func NewFixture(t *testing.T, store models.KernelStore, opts ...Option) *Fixture {
	t.Helper()
	config := FixtureConfig{contextWindow: DefaultContextWindow}
	for _, opt := range opts {
		opt(&config)
	}
	crash := NewCrashStore(t, store)
	f := &Fixture{
		T: t, Ctx: context.Background(), Raw: store, Store: crash, Crashes: crash,
		Scope:    models.Scope{Kind: "tenant", ID: "loop"},
		Clock:    NewFakeClock(loopFixtureStart()),
		Model:    NewScriptedModel(t, config.rules...),
		Tools:    NewFakeHostTools(t),
		Dispatch: newFakeDispatcher(t),
		policy:   &fixturePolicy{writesNeedApproval: config.writeApproval},
		WorkerID: FixtureWorker1,
		Config:   config,
	}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), f.Scope)
	f.Model.SetContextWindow(config.contextWindow)
	for name, handler := range config.hostTools {
		f.Tools.Handle(name, handler)
	}

	f.Catalog = skill.NewCatalog()
	if err := f.Catalog.Register(f.Ctx, fixtureSkillDefinition()); err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
	if err := f.Catalog.Bind(f.Ctx, &skill.Binding{
		ID: FixtureBindingID, Scope: skillScope, DeploymentID: FixtureAgent, SkillID: FixtureSkillID, SkillVersion: FixtureSkillVer,
		AllowedActions: []string{"lookup", "list_items", "update_status", "send", "explode"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	offered, err := f.Catalog.ListModelActions(f.Ctx, skillScope, FixtureAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 5 {
		t.Fatalf("fixture offers %d actions, want 5", len(offered))
	}
	f.Offered = offered

	chats, ok := store.(loopFixtureChatStore)
	if !ok {
		t.Fatalf("%T is not a conversation store", store)
	}
	f.Chats = chats
	clock := models.WithClock(f.Clock.Now)
	f.Convs = runtimeconversation.NewConversationService(chats, clock)
	f.Portfolio = portfolio.NewPortfolioService(f.Store, clock)
	scheduler, err := conversationrun.NewConversationRunScheduler(chats, chats, conversationrun.ConversationRunSchedulerConfig{}, clock)
	if err != nil {
		t.Fatal(err)
	}
	f.Scheduler = scheduler
	runner, err := conversationrun.NewConversationRunTurnRunner(chats, f.Coordinator(), conversationrun.ConversationRunTurnRunnerConfig{
		AttachmentContent: &AnswerVisualContent{}, AgentTurns: runtimeconversation.TurnRunnerResolverFunc(f.AgentBinding),
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	f.Runner = runner
	f.Build()
	return f
}

// loopFixtureStart is the fake clock's start: the wall clock, to the second.
// The conversation stores check a reply guard's lease against the wall clock
// (replyRunGuardHolds gets time.Now), so a fake clock behind wall time would
// make every lease the fixture grants look expired at the reply commit. The
// fake clock only moves forward from here, so it never falls behind.
func loopFixtureStart() time.Time { return time.Now().UTC().Truncate(time.Second) }

// Coordinator routes every chat message to the fixture's acting Agent: finn,
// or the first Team member.
func (f *Fixture) Coordinator() *runtimeconversation.ConversationCoordinator {
	f.T.Helper()
	members := f.Config.Team
	if len(members) == 0 {
		members = []string{FixtureAgent}
	}
	participants := runtimeconversation.ConversationParticipantSourceFunc(func(context.Context, runtimeconversation.ConversationParticipantQuery) ([]runtimeconversation.ConversationParticipantBinding, error) {
		bindings := make([]runtimeconversation.ConversationParticipantBinding, 0, len(members))
		for _, member := range members {
			bindings = append(bindings, runtimeconversation.ConversationParticipantBinding{
				Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: member}, SemanticRoles: []string{"assistant"},
			})
		}
		return bindings, nil
	})
	router := runtimeconversation.ConversationRouterFunc(func(context.Context, runtimeconversation.ConversationRouteRequest) (runtimeconversation.ConversationRouteDecision, error) {
		return runtimeconversation.ConversationRouteDecision{AgentID: members[0], Reason: "the fixture routes to the first member"}, nil
	})
	coordinator, err := runtimeconversation.NewConversationCoordinator(f.Convs, participants, router, runtimeconversation.DefaultConversationCoordinatorConfig())
	if err != nil {
		f.T.Fatal(err)
	}
	return coordinator
}

// Build wires the worker-bound services: the coordinators, the action
// worker, the governed and control executors, the tool dispatcher and the
// loop. crash() calls it again for the second worker.
func (f *Fixture) Build() {
	f.T.Helper()
	policy := action.ActionPolicyEvaluatorFunc(f.policy.evaluate)
	clock := models.WithClock(f.Clock.Now)
	f.Actions = action.NewActionCoordinator(f.Store, f.Store, f.Catalog, policy, nil, clock)
	f.Approve = action.NewApprovalCoordinator(f.Store, f.Store, action.ApprovalAuthorizerFunc(func(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error { return nil }), clock)
	f.Executor = action.NewActionWorker(f.Store, f.Catalog, f.Config.credentials, action.ActionDispatcherFunc(f.Dispatch.Dispatch), clock)
	governed, err := runloop.NewGovernedToolExecutor(runloop.GovernedToolExecutorConfig{
		Actions: f.Actions, Approvals: f.Approve, Worker: f.Executor, Store: f.Store, Now: f.Clock.Now,
	})
	if err != nil {
		f.T.Fatal(err)
	}
	activity := activity.NewRunActivityService(f.Store, f.Store, models.WithClock(f.Clock.Now))
	f.Control = &control.ControlExecutor{
		Portfolio: f.Store, Sessions: f.Store.(models.AgentSessionStore), Activity: activity,
		Conversations: f.Convs, Reporting: f.Chats, Now: f.Clock.Now,
	}
	if tasks, ok := f.Store.(models.ConversationTaskKernelStore); ok {
		f.Control.Tasks = conversationrun.NewConversationTaskService(tasks)
	}
	f.Dispatcher, err = runloop.NewStepToolDispatcher(runloop.StepToolDispatcherConfig{
		Sessions: f.Store.(models.AgentSessionStore), Turns: f.Store, Actions: f.Store, Coordinator: f.Actions,
		Governed: governed, Control: f.Control, Host: modelstep.NewHostToolDispatcher(f.Tools, f.Actions, nil), Now: f.Clock.Now,
	})
	if err != nil {
		f.T.Fatal(err)
	}
	f.Loop, err = runloop.NewRunLoop(f.Store, f.Actions, nil, runloop.RunLoopConfig{
		StepLeaseDuration: FixtureLease, MaxStepsPerClaim: f.Config.maxSteps, Now: f.Clock.Now,
		PlanToolEntries: f.planToolEntries, PlainTextFinal: f.Control.PlainTextFinal,
	})
	if err != nil {
		f.T.Fatal(err)
	}
}

// planToolEntries is the loop's planner seam over the dispatcher's planners.
func (f *Fixture) planToolEntries(binding *agentrun.TurnRunnerBinding, session *models.AgentSession, step *models.AgentTurn, assistant *models.SessionEntry, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
	if assistant != nil {
		return runloop.PlanToolEntries(binding, assistant)
	}
	if session == nil || step == nil {
		return nil, errors.New("deterministic tool entries need a session and a step")
	}
	run, err := f.Raw.GetAgentRun(f.Ctx, step.Scope, step.RunID)
	if err != nil {
		return nil, err
	}
	return runloop.PlanToolCallEntries(binding, run, step, session.ID, calls)
}

// ---------------------------------------------------------------------------
// Starting work

// StartChat creates a chat owned by Agent "finn" (Team "team" WithTeam), posts
// text as user "ana" and schedules the Run through the production scheduler.
// A Team chat returns the routed member's Run.
func (f *Fixture) StartChat(text string) (*models.Conversation, *models.AgentRun) {
	f.T.Helper()
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: FixtureAgent}
	if len(f.Config.Team) > 0 {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}
	}
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: owner, Title: "Loop fixture chat", IdempotencyKey: "loop-chat-" + fmt.Sprint(f.Clock.Now().UnixNano()),
	})
	if err != nil {
		f.T.Fatalf("create conversation: %v", err)
	}
	message := f.Post(conversation.ID, FixtureUser, text)
	run := f.ChatRun(conversation, message)
	if conversation, err = f.Convs.GetConversation(f.Ctx, f.Scope, conversation.ID); err != nil {
		f.T.Fatal(err)
	}
	return conversation, run
}

// ChatRun is the Run that answers message: the scheduled Agent Run, or for a
// Team chat the member Run the routing Run would start (the routing producer
// is not on the loop yet).
func (f *Fixture) ChatRun(conversation *models.Conversation, message *models.ChannelMessage) *models.AgentRun {
	f.T.Helper()
	if len(f.Config.Team) == 0 {
		result, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversation.ID, message.ID)
		if err != nil || result == nil || result.Run == nil {
			f.T.Fatalf("schedule chat message: %#v, %v", result, err)
		}
		return result.Run
	}
	member := f.Config.Team[0]
	request := runtimeconversation.ConversationAgentRunRequest(conversation, message)
	request.AssignedAgentID = member
	threadRoot, _ := request.Context["threadRootMessageId"].(string)
	request.ConcurrencyKey = models.ConversationRunLane(conversation.ID, threadRoot, member)
	request.SessionID = models.ConversationSessionID(f.Scope, request.ConcurrencyKey)
	request.IdempotencyKey = "member-" + member + "-" + message.ID
	created, err := f.Portfolio.CreateAgentRun(f.Ctx, request)
	if err != nil {
		f.T.Fatalf("create member run: %v", err)
	}
	return created
}

// StartRun creates an objective Run of kind for finn with goal.
func (f *Fixture) StartRun(kind models.RunKind, goal string) *models.AgentRun {
	f.T.Helper()
	created, err := f.Portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
		Scope: f.Scope, Kind: kind, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: FixtureAgent}, AssignedAgentID: FixtureAgent,
		Goal: goal, Source: models.RunSourceObjective,
	})
	if err != nil {
		f.T.Fatalf("create run: %v", err)
	}
	return created
}

// PostUser posts text as sender and lets the production scheduler see it
// (a new Run, or steering for the lane's running Run).
func (f *Fixture) PostUser(conversationID, sender, text string) *models.ChannelMessage {
	f.T.Helper()
	message := f.Post(conversationID, sender, text)
	if len(f.Config.Team) == 0 {
		if _, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversationID, message.ID); err != nil {
			f.T.Fatalf("schedule message: %v", err)
		}
	}
	return message
}

func (f *Fixture) Post(conversationID, sender, text string) *models.ChannelMessage {
	f.T.Helper()
	conversation, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversationID)
	if err != nil {
		f.T.Fatal(err)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: sender}, Intent: models.MessageIntentQuestion,
		Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		IdempotencyKey: fmt.Sprintf("post-%s-%d", sender, conversation.Revision),
	})
	if err != nil {
		f.T.Fatalf("post message: %v", err)
	}
	return posted.Message
}

// ---------------------------------------------------------------------------
// Driving

// Claim claims the next Run of the scope as the current worker; it fails the
// test when another Run (or none) is claimed.
func (f *Fixture) Claim(runID string) *models.AgentRun {
	f.T.Helper()
	claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Scope, WorkerID: f.WorkerID, Now: f.Clock.Now(), LeaseDuration: FixtureLease, AgingInterval: time.Minute,
	})
	if err != nil {
		f.T.Fatalf("claim %s: %v", runID, err)
	}
	if claimed == nil || claimed.ID != runID {
		got := "<none>"
		if claimed != nil {
			got = claimed.ID
		}
		f.T.Fatalf("claim %s claimed %s", runID, got)
	}
	return claimed
}

// RunClaim claims the Run and drives one claim of the loop.
func (f *Fixture) RunClaim(runID string) (runloop.RunLoopExit, *models.AgentRun) {
	f.T.Helper()
	return f.RunClaimContext(f.Ctx, runID)
}

func (f *Fixture) RunClaimContext(ctx context.Context, runID string) (runloop.RunLoopExit, *models.AgentRun) {
	f.T.Helper()
	claimed := f.Claim(runID)
	binding := f.Binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.MustRun(runID)
	}
	exit := f.Loop.Drive(ctx, runloop.RunLoopClaim{Run: claimed, WorkerID: f.WorkerID, Binding: binding})
	return exit, f.MustRun(runID)
}

// DriveToRest repeats claims while the Run is back in the queue (yield,
// requeue, a park that was already woken), advancing the clock to a requeue's
// AvailableAt. It returns the Run once it is terminal or waiting.
func (f *Fixture) DriveToRest(runID string, maxClaims int) *models.AgentRun {
	f.T.Helper()
	for i := 0; i < maxClaims; i++ {
		exit, run := f.RunClaim(runID)
		if exit.Err != nil && exit.Reason != runloop.RunLoopExitRequeued {
			f.T.Fatalf("claim %d of %s exited %s: %v", i+1, runID, exit.Reason, exit.Err)
		}
		if run.Status != models.AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(f.Clock.Now()) {
			f.Clock.Advance(run.AvailableAt.Sub(f.Clock.Now()) + time.Millisecond)
		}
	}
	run := f.MustRun(runID)
	f.T.Fatalf("run %s not at rest after %d claims: %s", runID, maxClaims, run.Status)
	return run
}

// Binding is the claim's TurnRunnerBinding, built the way the worker's
// resolver will. A chat Run goes through the conversation turn runner, which
// adds the conversation loop seams (catch-up preparer, read receipts, tool
// check, artifact receipts and the observed final delivery) on top of
// AgentBinding; any other Run gets AgentBinding alone.
func (f *Fixture) Binding(run *models.AgentRun) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	if conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string); conversationID == "" {
		binding, _ := f.AgentBinding(f.Ctx, run)
		return binding
	}
	binding, err := f.Runner.ResolveTurnRunner(f.Ctx, run)
	if err != nil {
		f.T.Fatalf("resolve conversation turn runner: %v", err)
	}
	return binding
}

// ConversationBinding is a chat Run's binding as a conversation turn runner
// resolves it when agent gives the Run's own Agent binding: the runner adds
// the conversation loop seams on top. Scenarios use it for the Runs whose
// Agent binding the fixture cannot build (Team members, threads, children).
func (f *Fixture) ConversationBinding(run *models.AgentRun, agent func(*models.AgentRun) *agentrun.TurnRunnerBinding) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	runner, err := conversationrun.NewConversationRunTurnRunner(f.Chats, f.Coordinator(), conversationrun.ConversationRunTurnRunnerConfig{
		AttachmentContent: &AnswerVisualContent{},
		AgentTurns: runtimeconversation.TurnRunnerResolverFunc(func(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
			return agent(run), nil
		}),
	}, models.WithClock(f.Clock.Now))
	if err != nil {
		f.T.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(f.Ctx, run)
	if err != nil {
		f.T.Fatalf("resolve conversation turn runner: %v", err)
	}
	return binding
}

// AgentBinding is the Agent's own TurnRunnerBinding, as the catalog resolver
// builds it: the model producer over the catalog preparer, the run-start
// preparer, the claim tool executor, the control authority and the kernel
// compactor (with its budget). A first claim opens the Run's session. It is
// the fixture's AgentTurns resolver for the conversation turn runner.
func (f *Fixture) AgentBinding(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	f.T.Helper()
	sessions := f.Store.(models.AgentSessionStore)
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	session := f.openSession(run, conversationID)

	var authority *workspace.Authority
	if len(f.Config.hostTools) > 0 {
		workspaceID := conversationID
		if workspaceID == "" {
			workspaceID = "workspace-" + run.ID
		}
		authority = workspace.ChatAuthority(workspaceID)
	}
	input := control.ControlAuthorityInput{Run: run, DeploymentID: run.AssignedAgentID, Runs: f.Store, Conversations: f.Chats}
	if len(f.Config.Team) > 0 {
		input.Catalog = loopFixtureTeamCatalog{members: f.Config.Team}
	}
	control, err := control.ResolveControlAuthority(f.Ctx, input)
	if err != nil {
		f.T.Fatalf("resolve control authority: %v", err)
	}
	preparer := runloop.NewCatalogModelStepPreparer(runloop.CatalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
		HostTools:         modelstep.HostToolSpecs(authority),
		Capabilities:      loopFixtureCapabilities,
	})
	host := FakeStepHost{ScriptedModel: f.Model, FakeHostTools: f.Tools}
	producer, err := modelstep.NewModelStepProducer(host, preparer, f.Store)
	if err != nil {
		f.T.Fatal(err)
	}
	binding := &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: FixtureAgent,
		DefinitionID: FixtureDefinition, DefinitionVersion: "1",
		ModelProvider: string(ModelProvider), Model: ModelName,
		ModelActions: append([]capability.ModelAction(nil), f.Offered...),
		Producer:     producer, Session: session, Host: host, Authority: control,
		Compactor: &modelstep.LoopCompactor{
			Host: host, Sessions: sessions, Requests: preparer,
			Budget: modelstep.NewRunActivityCompactionBudget(f.Store, activity.NewRunActivityService(f.Store, f.Store), f.Clock.Now),
			Now:    f.Clock.Now,
		},
		Preparers: []agentrun.StepPreparer{runloop.NewRunStartPreparer(sessions, f.Store)},
		Tools:     f.Dispatcher.ForClaim(runloop.LoopToolClaim{Workspace: authority, CapabilityConfig: loopFixtureCapabilities}),
	}
	return binding, nil
}

// loopFixtureTeamCatalog is the Team roster the control authority reads for a
// Team-owned Run. It serves only the Team deployment: the fixture's members
// are not delegation targets of each other (the rest of the catalog is nil).
type loopFixtureTeamCatalog struct {
	conversationrun.AgentTurnCatalog
	members []string
}

func (c loopFixtureTeamCatalog) GetTeamDeployment(_ context.Context, scope skill.ScopeReference, id string) (*kernelteam.Deployment, error) {
	roster := make([]kernelteam.RosterAssignment, 0, len(c.members))
	for _, member := range c.members {
		roster = append(roster, kernelteam.RosterAssignment{AgentDeploymentID: member})
	}
	return &kernelteam.Deployment{ID: id, Status: kernelteam.DeploymentActive, Roster: roster}, nil
}

func (loopFixtureTeamCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return nil, errors.New("the fixture Team has no definition")
}

// openSession returns the Run's session, creating it on the first claim: a
// conversation session for a chat Run, else a run session seeded with the
// goal. (The worker's first-claim session creation does the same.)
func (f *Fixture) openSession(run *models.AgentRun, conversationID string) *models.AgentSession {
	f.T.Helper()
	sessions := f.Store.(models.AgentSessionStore)
	now := f.Clock.Now()
	lease := models.AgentRunLeaseGuard{WorkerID: f.WorkerID, Now: now}
	switch {
	case run.SessionID != "" && conversationID == "":
		session, err := sessions.GetAgentSession(f.Ctx, f.Scope, run.SessionID)
		if err != nil {
			f.T.Fatalf("get session: %v", err)
		}
		return session
	case conversationID != "":
		// External-channel and thread Runs work in their thread lane.
		threadRoot, _ := run.Context["threadRootMessageId"].(string)
		opened, err := sessions.OpenConversationAgentSession(f.Ctx, models.OpenConversationAgentSessionRequest{
			Scope: f.Scope, ConversationID: conversationID, ThreadRootID: threadRoot, AgentID: run.AssignedAgentID, RunID: run.ID, Lease: lease, Now: now,
		})
		if err != nil {
			f.T.Fatalf("open conversation session: %v", err)
		}
		return opened.Session
	}
	goal := &models.SessionEntry{
		ID: models.SessionEntryID(run.ID, "goal"), Scope: f.Scope, RunID: run.ID, Type: models.SessionEntryUser, SourceKey: "goal",
		User: &models.UserEntry{Origin: models.UserEntryGoal, Author: models.SessionAuthor{Type: "user", ID: FixtureUser},
			Content: []models.ContentPart{{Type: models.ContentPartText, Text: run.Goal}}},
	}
	created, err := sessions.CreateRunAgentSession(f.Ctx, models.CreateRunAgentSessionRequest{
		Scope: f.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, InitialEntries: []*models.SessionEntry{goal}, Now: now,
	})
	if err != nil {
		f.T.Fatalf("create run session: %v", err)
	}
	return created.Session
}

// Decide resolves the Run's single pending approval as ana.
func (f *Fixture) Decide(runID string, decision models.ApprovalDecision, reason string) {
	f.T.Helper()
	pending, err := f.Store.ListApprovals(f.Ctx, models.ApprovalFilter{Scope: f.Scope, RunID: runID, Status: []models.ApprovalStatus{models.ApprovalStatusPending}})
	if err != nil {
		f.T.Fatal(err)
	}
	if len(pending) != 1 {
		f.T.Fatalf("run %s has %d pending approvals, want 1", runID, len(pending))
	}
	approval := pending[0]
	if _, err := f.Approve.Resolve(f.Ctx, action.ResolveApprovalRequest{
		Scope: f.Scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "decision-" + approval.ID,
		Decision: decision, Principal: models.ApprovalPrincipal{Type: "user", ID: FixtureUser}, Reason: reason,
	}); err != nil {
		f.T.Fatalf("resolve approval %s: %v", approval.ID, err)
	}
}

// Crash kills the current worker mid-claim: arm the store, cancel the claim
// ctx, wait for the claim to return, disarm, let the leases expire and
// rebuild the worker-bound services as "loop-worker-2" over the same store.
func (f *Fixture) Crash(ctxCancel context.CancelFunc, done <-chan struct{}) {
	f.T.Helper()
	f.Crashes.Arm()
	if ctxCancel != nil {
		ctxCancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.T.Fatal("crashed claim did not return")
		}
	}
	f.Crashes.Disarm()
	f.Clock.Advance(FixtureLease + time.Second)
	f.WorkerID = FixtureWorker2
	f.Build()
}
