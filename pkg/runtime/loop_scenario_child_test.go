package runtime

// Run-loop scenarios, child sessions (run-loop section 12 §5.8, A3/A4): forks,
// delegation through decision/execution/review Runs, conversation tasks,
// foreground promotion and runbook operations, driven through the real
// RunLoop by the loop fixture plus a small worker: the claim of the next
// queued Run, the binding the worker's resolver gives it (ResolveLoopBinding
// shape: a Run bound to a session keeps it, any other Run gets its run session
// at first claim, seeded with its brief), and the terminal-child resolution
// the worker pool runs after a claim (fork edges, AgentRequest decisions,
// reviews and children, terminal reporting).
//
// The runbook scenarios read the runbook Run's engine state, so this file is
// exempt from TestLoopTestkitHasNoCheckpointAccess by name.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// ---------------------------------------------------------------------------
// Child fixture

const (
	childScout    = "scout"
	childReviewer = "rhea"
	childTeam     = "crew"
)

// childFixture is a loop fixture with the child seams the worker wires: the
// fork coordinator, collaboration with a Team catalog, the request inbox and
// the runbook definitions of operation children.
type childFixture struct {
	*loopFixture
	forks    *agentrun.RunForkCoordinator
	collab   *CollaborationService
	inbox    *AgentRequestInboxReconciler
	runbooks map[string]*runbook.Definition // by definition id
	// authority adjusts a model Run's control authority before its claim (a
	// test grants delegation targets, task start or operations here).
	authority func(run *AgentRun, authority *agentrun.ControlAuthority)
	// claims counts claims per Run.
	claims map[string]int
}

type childTestStore interface {
	KernelStore
	RunDependencyStore
	CollaborationKernelStore
	AgentRequestInboxStore
}

// childTeamCatalog is the Team catalog of the fixture's "crew": finn
// coordinates, scout is the specialist and rhea the reviewer. Delegated
// completions need review.
type childTeamCatalog struct {
	scope capability.ScopeReference
}

func (c childTeamCatalog) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if scope != c.scope || id != childTeam {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return &kernelteam.Deployment{
		ID: childTeam, Scope: c.scope, DefinitionID: childTeam, ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
		Roster: []kernelteam.RosterAssignment{
			{ID: "coordinator", AgentDeploymentID: loopFixtureAgent},
			{ID: "reviewer", AgentDeploymentID: childReviewer, Approver: true},
			{ID: "specialist", AgentDeploymentID: childScout},
		},
	}, nil
}

func (c childTeamCatalog) GetTeamDefinition(_ context.Context, id, version string) (*kernelteam.Definition, error) {
	if id != childTeam || version != "1" {
		return nil, kernelteam.ErrDefinitionNotFound
	}
	return &kernelteam.Definition{
		ID: childTeam, Version: "1",
		Delegation: kernelteam.DelegationPolicy{
			MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true,
			RequireAcceptance: true, RequireCompletionReview: true,
		},
	}, nil
}

// childTeamStore adds the crew's Team catalog to the store, as the
// production store with the Team registry does, so the collaboration services
// read Team rosters and delegation policies from it.
type childTeamStore struct {
	childTestStore
	childTeamCatalog
}

func newChildFixture(t *testing.T, store KernelStore, opts ...loopOption) *childFixture {
	t.Helper()
	f := newLoopFixture(t, store, opts...)
	// Task admission checks the source lease against the wall clock
	// (conversation_task.go), so the fake clock starts at wall time.
	if ahead := time.Until(f.clock.Now()); ahead < 0 {
		f.clock.Advance(-ahead)
	}
	stores, ok := f.raw.(childTestStore)
	if !ok {
		t.Fatalf("%T lacks the dependency and collaboration stores", f.raw)
	}
	teams := childTeamStore{childTestStore: stores, childTeamCatalog: childTeamCatalog{scope: capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}}}
	collab := NewCollaborationService(teams, models.WithClock(f.clock.Now))
	inbox, err := NewAgentRequestInboxReconciler(teams, models.WithClock(f.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	c := &childFixture{
		loopFixture: f, forks: agentrun.NewRunForkCoordinator(stores, models.WithClock(f.clock.Now)), collab: collab,
		inbox: inbox, runbooks: map[string]*runbook.Definition{}, claims: map[string]int{},
	}
	c.forks.SetAcceptedRunExecutionPreparer(childRunbookPreparer{c})
	if tasks, ok := f.raw.(ConversationTaskKernelStore); ok {
		f.control.Tasks = NewConversationTaskService(tasks, models.WithClock(f.clock.Now))
	}
	f.control.Forks = c.forks
	// The task completion review reads the task through the portfolio.
	f.control.Portfolio = f.raw
	// A task's completion review: approve (Purpose acknowledgement).
	review := &modelRule{name: "task-review", when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeAcknowledgement },
		reply: func(stepView) ModelStepResult {
			return say(`{"decision":"approve","reason":"The report answers the accepted goal."}`)
		}}
	f.model.mu.Lock()
	f.model.rules = append([]*modelRule{review}, f.model.rules...)
	f.model.mu.Unlock()
	f.control.Collaboration = collab
	return c
}

// childRunbookPreparer pins an operation child to its registered definition
// (the catalog's accepted-execution preparer).
type childRunbookPreparer struct{ c *childFixture }

func (p childRunbookPreparer) PrepareAcceptedRunExecution(_ context.Context, request *CreateAgentRunRequest) error {
	request.Context = maps.Clone(request.Context)
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	delete(request.Context, AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	id, _ := request.Context["runbookDefinitionId"].(string)
	definition := p.c.runbooks[id]
	if definition == nil {
		return ErrAcceptedRunExecution
	}
	request.Context[AcceptedRunExecutionContextKey] = &AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: loopFixtureDefinition, DefinitionVersion: "1",
		RunbookID: definition.ID, RunbookVersion: definition.Version, Entrypoint: request.Entrypoint,
	}
	return nil
}

// claimNext claims the next queued Run of the scope; nil when none is due.
func (c *childFixture) claimNext() *AgentRun {
	c.t.Helper()
	claimed, err := c.store.ClaimNextAgentRun(c.ctx, AgentRunClaim{
		Scope: c.scope, WorkerID: c.workerID, Now: c.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute,
	})
	if err != nil {
		c.t.Fatalf("claim: %v", err)
	}
	return claimed
}

// drive claims runID (it must be the next queued Run) and drives one claim.
func (c *childFixture) drive(runID string) (RunLoopExit, *AgentRun) {
	c.t.Helper()
	claimed := c.claimNext()
	if claimed == nil || claimed.ID != runID {
		got := "<none>"
		if claimed != nil {
			got = claimed.ID + " (" + string(claimed.Source) + ")"
		}
		want := c.mustRun(runID)
		c.t.Logf("want %s: status %s available %s now %s key %q", runID, want.Status, want.AvailableAt, c.clock.Now(), want.ConcurrencyKey)
		c.t.Fatalf("claimed %s, want %s", got, runID)
	}
	return c.driveClaimed(claimed)
}

func (c *childFixture) driveClaimed(claimed *AgentRun) (RunLoopExit, *AgentRun) {
	c.t.Helper()
	c.claims[claimed.ID]++
	binding := c.bindingFor(claimed)
	// The resolver may create the run session at first claim.
	claimed = c.mustRun(claimed.ID)
	exit := c.loop.Drive(c.ctx, RunLoopClaim{Run: claimed, WorkerID: c.workerID, Binding: binding})
	if exit.Err != nil && exit.Reason != RunLoopExitRequeued {
		after := c.mustRun(claimed.ID)
		c.t.Logf("after: status %s lease %q output %v", after.Status, after.LeaseOwner, after.Output)
		c.t.Fatalf("claim of %s exited %s: %v\nlog: %v", claimed.ID, exit.Reason, exit.Err, c.entryTypesOf(claimed.ID))
	}
	run := c.mustRun(claimed.ID)
	c.resolveTerminal(run)
	return exit, run
}

// work is the worker: it claims and drives every due Run (advancing the clock
// to a requeue's AvailableAt) and reconciles the request inbox, until nothing
// is queued. It returns the claimed Run ids in order.
func (c *childFixture) work(maxClaims int) []string {
	c.t.Helper()
	var order []string
	for i := 0; ; i++ {
		c.reconcileInbox()
		claimed := c.claimNext()
		if claimed == nil {
			next := c.nextAvailable()
			if next == nil {
				return order
			}
			c.clock.Advance(next.Sub(c.clock.Now()) + time.Millisecond)
			continue
		}
		if i >= maxClaims {
			c.t.Fatalf("worker not idle after %d claims (claimed %s)", maxClaims, claimed.ID)
		}
		order = append(order, claimed.ID)
		c.driveClaimed(claimed)
	}
}

// nextAvailable is the earliest future AvailableAt of a queued Run.
func (c *childFixture) nextAvailable() *time.Time {
	runs, err := c.raw.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, Statuses: []AgentRunStatus{AgentRunStatusQueued}})
	if err != nil {
		c.t.Fatal(err)
	}
	var next *time.Time
	for _, run := range runs {
		if at := run.AvailableAt; at.After(c.clock.Now()) && (next == nil || at.Before(*next)) {
			at := at
			next = &at
		}
	}
	return next
}

func (c *childFixture) reconcileInbox() {
	c.t.Helper()
	if _, err := c.inbox.Reconcile(c.ctx, c.scope, ""); err != nil {
		c.t.Fatalf("reconcile request inbox: %v", err)
	}
}

// resolveTerminal is the worker pool after a terminal claim: terminal
// reporting, the fork edge, the AgentRequest decision/review/child.
func (c *childFixture) resolveTerminal(run *AgentRun) {
	c.t.Helper()
	if !isTerminalAgentRunStatus(run.Status) {
		return
	}
	tolerable := func(err error) bool {
		return err == nil || errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidAgentRequestState) || errors.Is(err, ErrDependencyConflict)
	}
	if err := projectTerminalRunReporting(c.ctx, c.chats, run); err != nil {
		c.t.Fatalf("terminal reporting of %s: %v", run.ID, err)
	}
	if run.ParentRunID != "" && run.Checkpoint["forkChild"] != nil {
		if _, err := c.forks.CompleteChild(c.ctx, run, ActivityActor{Type: "worker", ID: c.workerID}); !tolerable(err) {
			c.t.Fatalf("complete fork child %s: %v", run.ID, err)
		}
	}
	if _, err := c.inbox.ResolveDecisionRun(c.ctx, run); !tolerable(err) {
		c.t.Fatalf("resolve decision %s: %v", run.ID, err)
	}
	if _, err := c.inbox.ResolveCompletionReviewRun(c.ctx, run); !tolerable(err) {
		c.t.Fatalf("resolve review %s: %v", run.ID, err)
	}
	if _, err := c.collab.ResolveTerminalAgentRequestChild(c.ctx, run); !tolerable(err) {
		c.t.Fatalf("resolve request child %s: %v", run.ID, err)
	}
}

// bindingFor is the claim's binding, the shape ResolveLoopBinding gives each
// kind: a chat reply binds to its lane through the conversation loop; a
// pinned runbook entrypoint is deterministic over an unrendered run session;
// every other Run is a model Run on its own session (A3).
func (c *childFixture) bindingFor(run *AgentRun) *agentrun.TurnRunnerBinding {
	c.t.Helper()
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if run.Entrypoint != "" {
		return c.runbookBinding(run)
	}
	// The fixture has no Agent catalog: a Team Run's eligible targets are
	// granted by the scenario's authority hook instead of the Team roster.
	authorityRun := run
	if run.Owner.Type == OwnerTypeTeam && run.Context[conversationRunContextConversationID] == nil {
		copied := *run
		copied.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: run.AssignedAgentID}
		authorityRun = &copied
	}
	control, err := ResolveControlAuthority(c.ctx, ControlAuthorityInput{
		Run: authorityRun, DeploymentID: run.AssignedAgentID, Runs: c.store, Conversations: c.chats,
	})
	if err != nil {
		c.t.Fatalf("resolve control authority: %v", err)
	}
	if c.authority != nil {
		c.authority(run, &control)
	}
	host := fakeStepHost{scriptedModel: c.model, fakeHostTools: c.tools}
	preparer := newCatalogModelStepPreparer(catalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
	})
	producer, err := NewModelStepProducer(host, preparer, c.store)
	if err != nil {
		c.t.Fatal(err)
	}
	binding := &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: loopFixtureAgent,
		DefinitionID: loopFixtureDefinition, DefinitionVersion: "1",
		ModelProvider: string(kitModelProvider), Model: kitModelName,
		Producer: producer, Host: host, Authority: control,
		Preparers: []agentrun.StepPreparer{newRunStartPreparer(c.store.(AgentSessionStore), c.store)},
		Tools:     c.dispatcher.ForClaim(LoopToolClaim{}),
	}
	_, decision := run.Context[agentrun.AgentRequestInboxContextKey]
	_, review := run.Context[agentrun.AgentRequestCompletionReviewContextKey]
	if !decision && !review {
		binding.ModelActions = append([]capability.ModelAction(nil), c.offered...)
	}
	if run.Kind == RunKindConversation && conversationID != "" && triggerID != "" {
		return c.conversationBinding(run, func(*AgentRun) *agentrun.TurnRunnerBinding { return binding })
	}
	binding.Session = c.runSession(run, true)
	return binding
}

// runSession is the Run's session: the one it is bound to, else a fresh,
// parentless run session created at this first claim (seeded with the Run's
// brief for a model Run).
func (c *childFixture) runSession(run *AgentRun, seeded bool) *AgentSession {
	c.t.Helper()
	sessions := c.store.(AgentSessionStore)
	if run.SessionID != "" {
		session, err := conversationRunSession(c.ctx, sessions, run)
		if err != nil {
			c.t.Fatalf("run session of %s: %v", run.ID, err)
		}
		return session
	}
	var initial []*SessionEntry
	if seeded {
		entries, err := loopRunSessionSeedEntries(c.ctx, c.store, nil, nil, run)
		if err != nil {
			c.t.Fatal(err)
		}
		initial = entries
	}
	now := c.clock.Now()
	opened, err := sessions.CreateRunAgentSession(c.ctx, CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID,
		Lease: AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}, InitialEntries: initial, Now: now,
	})
	if err != nil {
		c.t.Fatalf("create run session of %s: %v", run.ID, err)
	}
	return opened.Session
}

func (c *childFixture) runbookBinding(run *AgentRun) *agentrun.TurnRunnerBinding {
	c.t.Helper()
	id, _ := run.Context["runbookDefinitionId"].(string)
	definition := c.runbooks[id]
	if definition == nil {
		c.t.Fatalf("runbook Run %s pins unknown definition %q", run.ID, id)
	}
	producer, err := NewRunbookStepProducer(definition, run.Entrypoint, c.store)
	if err != nil {
		c.t.Fatal(err)
	}
	control, err := ResolveControlAuthority(c.ctx, ControlAuthorityInput{Run: run, DeploymentID: run.AssignedAgentID, Deterministic: true})
	if err != nil {
		c.t.Fatal(err)
	}
	return &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: loopFixtureAgent,
		DefinitionID: loopFixtureDefinition, DefinitionVersion: "1",
		Producer: producer, Authority: control, Session: c.runSession(run, false),
		Tools: c.dispatcher.ForClaim(LoopToolClaim{}),
	}
}

// ---------------------------------------------------------------------------
// Reading

func (c *childFixture) entryTypesOf(runID string) []string {
	if c.mustRun(runID).SessionID == "" {
		return nil
	}
	return c.entryTypes(runID)
}

func (c *childFixture) sessionByID(id string) *AgentSession {
	c.t.Helper()
	session, err := c.raw.(AgentSessionStore).GetAgentSession(c.ctx, c.scope, id)
	if err != nil {
		c.t.Fatalf("get session %s: %v", id, err)
	}
	return session
}

// sessionEntries is the session's own entries (not its parent's), in order.
func (c *childFixture) sessionEntries(sessionID string) []*SessionEntry {
	c.t.Helper()
	entries, err := c.raw.(AgentSessionStore).ListAgentSessionEntries(c.ctx, SessionEntryFilter{Scope: c.scope, SessionID: sessionID, Limit: 1000})
	if err != nil {
		c.t.Fatal(err)
	}
	return entries
}

// path is the session's rendered path (the parent up to the fork, then its own).
func (c *childFixture) path(sessionID string) []*SessionEntry {
	c.t.Helper()
	path, err := c.raw.(AgentSessionStore).LoadAgentSessionPath(c.ctx, c.scope, sessionID)
	if err != nil {
		c.t.Fatal(err)
	}
	return path.Entries
}

// toolEntry is the Run's newest tool entry named name, in its own session.
func (c *childFixture) toolEntry(runID, name string) *SessionEntry {
	c.t.Helper()
	entries := c.sessionEntries(c.mustRun(runID).SessionID)
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Type == SessionEntryTool && e.Tool != nil && e.Tool.Name == name && e.RunID == runID {
			return e
		}
	}
	c.t.Fatalf("run %s has no %s entry: %v", runID, name, c.entryTypes(runID))
	return nil
}

func (c *childFixture) toolEntries(runID, name string) []*SessionEntry {
	c.t.Helper()
	var out []*SessionEntry
	for _, e := range c.sessionEntries(c.mustRun(runID).SessionID) {
		if e.Type == SessionEntryTool && e.Tool != nil && e.Tool.Name == name && e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

func (c *childFixture) children(parentID string) []*AgentRun {
	c.t.Helper()
	runs, err := c.raw.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, ParentRunID: parentID})
	if err != nil {
		c.t.Fatal(err)
	}
	return runs
}

// requestsFor is the model requests the scripted model got for runID.
func (c *childFixture) requestsFor(runID string) []ModelStepRequest {
	var out []ModelStepRequest
	for _, req := range c.model.calls("") {
		if req.RunID == runID {
			out = append(out, req)
		}
	}
	return out
}

func childResultText(entry *SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return kitPartsText(entry.Tool.Result.Content)
}

func childResultJSON(t *testing.T, entry *SessionEntry) map[string]interface{} {
	t.Helper()
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(childResultText(entry)), &value); err != nil {
		t.Fatalf("result of %s is not JSON: %q", entry.ID, childResultText(entry))
	}
	return value
}

func entryIDs(entries []*SessionEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func entryViewIDs(entries []SessionEntryView) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// isForkBranch reports whether a request belongs to a fork branch: its first
// user entry after the parent's path is the branch brief.
func isForkBranch(v stepView) bool {
	for _, text := range v.UserTexts() {
		if strings.Contains(text, "one branch of a parallel task") {
			return true
		}
	}
	return false
}

func branchGoal(v stepView) string {
	for _, text := range v.UserTexts() {
		if i := strings.Index(text, "Goal:\n"); i >= 0 && strings.Contains(text, "one branch of a parallel task") {
			goal := text[i+len("Goal:\n"):]
			if j := strings.Index(goal, "\n"); j >= 0 {
				goal = goal[:j]
			}
			return strings.TrimSpace(goal)
		}
	}
	return ""
}

func childForkCall(goals ...string) tc {
	branches := make([]interface{}, len(goals))
	for i, goal := range goals {
		branches[i] = map[string]interface{}{"goal": goal}
	}
	return call(string(ControlToolFork), map[string]any{"branches": branches})
}

// ---------------------------------------------------------------------------
// Forks

func TestLoopForkCreatesChildSessionAndResumesWithItsFinalText(t *testing.T) {
	eachStore(t, "child_fork", func(t *testing.T, store KernelStore) {
		answers := map[string]string{"count items": "2 items", "count boxes": "3 boxes"}
		c := newChildFixture(t, store, withRules(
			&modelRule{name: "branch", when: isForkBranch, reply: func(v stepView) ModelStepResult {
				return say(answers[branchGoal(v)])
			}},
			&modelRule{name: "fork", times: 1, reply: func(stepView) ModelStepResult {
				return act("Counting in parallel.", childForkCall("count items", "count boxes"))
			}},
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				content, isError, ok := v.ToolResult(string(ControlToolFork))
				if !ok || isError || !strings.Contains(content, "2 items") {
					return say("The count failed.")
				}
				return say("There are 2 items and 3 boxes.")
			}},
		))
		parent := c.startRun(RunKindAgentWork, "Count the stock")

		// The fork parks the parent and releases its claim.
		if exit, _ := c.drive(parent.ID); exit.Reason != RunLoopExitParked {
			t.Fatalf("parent exit = %s", exit.Reason)
		}
		waiting := c.mustRun(parent.ID)
		if waiting.Status != AgentRunStatusWaitingForDependency || waiting.LeaseOwner != "" {
			t.Fatalf("parent = %s lease %q", waiting.Status, waiting.LeaseOwner)
		}
		forkEntry := c.toolEntry(parent.ID, string(ControlToolFork))
		if forkEntry.State != ToolEntryAwaitingChild {
			t.Fatalf("fork entry = %s", forkEntry.State)
		}
		parentPath := c.path(waiting.SessionID)
		var assistant *SessionEntry
		for _, e := range parentPath {
			if e.Type == SessionEntryAssistant && e.StepID == forkEntry.StepID {
				assistant = e
			}
		}
		if assistant == nil {
			t.Fatalf("no forking assistant entry in %v", entryIDs(parentPath))
		}
		children := c.children(parent.ID)
		if len(children) != 2 {
			t.Fatalf("children = %d, want 2", len(children))
		}
		// A3: the child session forks just before the forking assistant entry.
		var beforeFork []string
		for _, e := range parentPath {
			if e.Sequence < assistant.Sequence {
				beforeFork = append(beforeFork, e.ID)
			}
		}
		for _, child := range children {
			if child.SessionID == "" || child.SessionID == waiting.SessionID {
				t.Fatalf("child %s session = %q", child.ID, child.SessionID)
			}
			session := c.sessionByID(child.SessionID)
			if session.ParentSessionID != waiting.SessionID || session.ForkSequence != assistant.Sequence-1 ||
				session.ForkEntryID != beforeFork[len(beforeFork)-1] || session.OwnerRunID != child.ID {
				t.Fatalf("child session = parent %q fork %d/%q owner %q; want %q %d/%q",
					session.ParentSessionID, session.ForkSequence, session.ForkEntryID, session.OwnerRunID,
					waiting.SessionID, assistant.Sequence-1, beforeFork[len(beforeFork)-1])
			}
		}

		c.work(10)
		done := c.mustRun(parent.ID)
		c.mustStatus(done, AgentRunStatusCompleted)

		// Each child's first request is the parent path up to the fork point,
		// then its own brief; it never sees the fork decision or later parent
		// entries.
		for _, child := range children {
			requests := c.requestsFor(child.ID)
			if len(requests) != 1 {
				t.Fatalf("child %s made %d requests", child.ID, len(requests))
			}
			ids := entryViewIDs(requests[0].Entries)
			if len(ids) < len(beforeFork)+1 || strings.Join(ids[:len(beforeFork)], ",") != strings.Join(beforeFork, ",") {
				t.Fatalf("child path = %v, want prefix %v", ids, beforeFork)
			}
			own := map[string]bool{}
			for _, e := range c.sessionEntries(child.SessionID) {
				if e.RunID != child.ID {
					t.Fatalf("child %s session holds entry %s of run %s", child.ID, e.ID, e.RunID)
				}
				own[e.ID] = true
			}
			for _, id := range ids[len(beforeFork):] {
				if !own[id] {
					t.Fatalf("child %s path holds %s after the fork point, which is not its own", child.ID, id)
				}
			}
			for _, id := range ids {
				if id == assistant.ID || id == forkEntry.ID {
					t.Fatalf("child path holds the fork step entry %s", id)
				}
			}
			if branchGoal(stepView{req: requests[0]}) != child.Goal {
				t.Fatalf("child %s brief goal = %q, want %q", child.ID, branchGoal(stepView{req: requests[0]}), child.Goal)
			}
		}

		// The parent's tool entry completed with the children's final texts.
		forkEntry = c.toolEntry(parent.ID, string(ControlToolFork))
		if forkEntry.State != ToolEntryCompleted || forkEntry.Tool.Result.IsError {
			t.Fatalf("fork entry = %s %q", forkEntry.State, childResultText(forkEntry))
		}
		value := childResultJSON(t, forkEntry)
		branches, _ := value["branches"].([]interface{})
		if len(branches) != 2 {
			t.Fatalf("fork result = %s", childResultText(forkEntry))
		}
		for i, want := range []string{"2 items", "3 boxes"} {
			if branch := branches[i].(map[string]interface{}); branch["answer"] != want || branch["status"] != string(AgentRunStatusCompleted) {
				t.Fatalf("branch %d = %v", i, branch)
			}
		}
		if got := done.Output[finalOutputSummaryKey]; got != "There are 2 items and 3 boxes." {
			t.Fatalf("parent answer = %v", done.Output)
		}
		if c.claims[parent.ID] != 2 {
			t.Fatalf("parent claims = %d, want 2", c.claims[parent.ID])
		}
	})
}

func TestLoopTwoParallelForksJoinBeforeResume(t *testing.T) {
	eachStore(t, "child_fork_join", func(t *testing.T, store KernelStore) {
		c := newChildFixture(t, store, withRules(
			&modelRule{name: "branch", when: isForkBranch, reply: func(v stepView) ModelStepResult {
				return say("done: " + branchGoal(v))
			}},
			// Two fork calls in one step: the kernel runs one settle-phase
			// call per assistant entry and refuses the second.
			&modelRule{name: "fork", times: 1, reply: func(stepView) ModelStepResult {
				return act("Splitting the survey.", childForkCall("survey north", "survey south"), childForkCall("survey east", "survey west"))
			}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return say("Both surveys are done.") }},
		))
		parent := c.startRun(RunKindAgentWork, "Survey both regions")
		c.drive(parent.ID)
		children := c.children(parent.ID)
		if len(children) != 2 {
			t.Fatalf("children = %d", len(children))
		}
		forks := c.toolEntries(parent.ID, string(ControlToolFork))
		if len(forks) != 2 || forks[0].State != ToolEntryAwaitingChild || forks[1].State != ToolEntryError ||
			!strings.Contains(childResultText(forks[1]), "control_one_settle_call") {
			t.Fatalf("fork entries = %v / %q", c.entryTypes(parent.ID), childResultText(forks[len(forks)-1]))
		}
		// The first child finishes; the parent stays parked.
		first := children[0]
		c.drive(first.ID)
		c.mustStatus(first, AgentRunStatusCompleted)
		if got := c.mustRun(parent.ID); got.Status != AgentRunStatusWaitingForDependency {
			t.Fatalf("parent woke after one of two branches: %s", got.Status)
		}
		if entry := c.toolEntries(parent.ID, string(ControlToolFork))[0]; entry.State != ToolEntryAwaitingChild {
			t.Fatalf("fork entry after one branch = %s", entry.State)
		}
		if n := len(c.requestsFor(parent.ID)); n != 1 {
			t.Fatalf("parent made %d model requests before the join", n)
		}
		c.drive(children[1].ID)
		if got := c.mustRun(parent.ID); got.Status != AgentRunStatusQueued {
			t.Fatalf("parent after both branches = %s", got.Status)
		}
		c.work(4)
		c.mustStatus(parent, AgentRunStatusCompleted)
		forks = c.toolEntries(parent.ID, string(ControlToolFork))
		entry := forks[0]
		branches, _ := childResultJSON(t, entry)["branches"].([]interface{})
		if entry.State != ToolEntryCompleted || len(branches) != 2 || forks[1].State != ToolEntryError || forks[0].Sequence > forks[1].Sequence {
			t.Fatalf("fork entries = %s %s / %s", entry.State, childResultText(entry), forks[1].State)
		}
		// Branch results come back in call order, not finish order.
		for i, want := range []string{"done: survey north", "done: survey south"} {
			if branch := branches[i].(map[string]interface{}); branch["answer"] != want || branch["branch"] != fmt.Sprintf("branch-%d", i+1) {
				t.Fatalf("branch %d = %v", i, branch)
			}
		}
		// The resumed step saw the joined result.
		requests := c.requestsFor(parent.ID)
		if len(requests) != 2 {
			t.Fatalf("parent requests = %d", len(requests))
		}
		results := (stepView{req: requests[1]}).ToolResultsSinceUser()
		if len(results) != 2 || results[0].Tool.Result.IsError || !results[1].Tool.Result.IsError ||
			!strings.Contains(kitPartsText(results[0].Tool.Result.Content), "survey north") ||
			!strings.Contains(kitPartsText(results[0].Tool.Result.Content), "survey south") {
			t.Fatalf("resumed step: %s", stepView{req: requests[1]}.describe())
		}
	})
}

func TestLoopChildFailureIsAToolError(t *testing.T) {
	eachStore(t, "child_fork_failure", func(t *testing.T, store KernelStore) {
		c := newChildFixture(t, store, withRules(
			&modelRule{name: "branch-fail", when: func(v stepView) bool { return isForkBranch(v) && branchGoal(v) == "read the ledger" },
				err: NewTurnHostFailure("step_invalid_request", "the ledger cannot be read", false)},
			&modelRule{name: "branch", when: isForkBranch, reply: func(stepView) ModelStepResult { return say("Inventory counted.") }},
			&modelRule{name: "fork", times: 1, reply: func(stepView) ModelStepResult {
				return act("Checking both.", childForkCall("read the ledger", "count the inventory"))
			}},
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				if _, isError, _ := v.ToolResult(string(ControlToolFork)); isError {
					return say("One check failed.")
				}
				return say("All checks passed.")
			}},
		))
		parent := c.startRun(RunKindAgentWork, "Check the books")
		c.work(12)
		c.mustStatus(parent, AgentRunStatusCompleted)
		var failed *AgentRun
		for _, child := range c.children(parent.ID) {
			if child.Goal == "read the ledger" {
				failed = c.mustRun(child.ID)
			}
		}
		if failed == nil || failed.Status != AgentRunStatusFailed || failed.FailureExplanation == nil || failed.FailureExplanation.Message == "" {
			t.Fatalf("failed child = %#v", failed)
		}
		entry := c.toolEntry(parent.ID, string(ControlToolFork))
		if entry.State != ToolEntryError || !entry.Tool.Result.IsError {
			t.Fatalf("fork entry = %s error=%v", entry.State, entry.Tool.Result.IsError)
		}
		if text := childResultText(entry); !strings.Contains(text, failed.FailureExplanation.Message) && !strings.Contains(text, failed.Error) {
			t.Fatalf("fork result %q lacks the child's failure %q", text, failed.FailureExplanation.Message)
		}
		if got := c.mustRun(parent.ID).Output[finalOutputSummaryKey]; got != "One check failed." {
			t.Fatalf("parent answer = %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Delegation: decision, execution and review Runs

func TestLoopDelegationRunsDecisionExecutionAndReviewInChildSessions(t *testing.T) {
	eachStore(t, "child_delegation", func(t *testing.T, store KernelStore) {
		c := newChildFixture(t, store, withRules(
			&modelRule{name: "decision", when: func(v stepView) bool { return v.HasTool(string(ControlToolRequestDecision)) },
				reply: func(stepView) ModelStepResult {
					return act("", call(string(ControlToolRequestDecision), map[string]any{"decision": "accept", "message": "I can check the release."}))
				}},
			&modelRule{name: "review", when: func(v stepView) bool { return v.HasTool(string(ControlToolCompletionReview)) },
				reply: func(stepView) ModelStepResult {
					return act("", call(string(ControlToolCompletionReview), map[string]any{"decision": "approve", "message": "The check lists every open issue."}))
				}},
			&modelRule{name: "execution", when: func(v stepView) bool {
				for _, text := range v.UserTexts() {
					if strings.Contains(text, "Requested work. Goal:") {
						return true
					}
				}
				return false
			}, reply: func(stepView) ModelStepResult { return say("Release 7 has 2 open issues.") }},
			&modelRule{name: "delegate", times: 1, reply: func(stepView) ModelStepResult {
				return act("Asking scout.", call(string(ControlToolDelegate), map[string]any{"agentId": childScout, "goal": "Check release 7"}))
			}},
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				content, _, _ := v.ToolResult(string(ControlToolDelegate))
				return say("Scout says: " + content)
			}},
		))
		c.authority = func(run *AgentRun, a *agentrun.ControlAuthority) {
			if run.AssignedAgentID == loopFixtureAgent && a.EffectiveFinalMode() == agentrun.FinalModeDefault {
				a.DelegateTargets = []agentrun.HostedAgentTarget{{ID: childScout, DisplayName: "Scout"}}
			}
		}
		created, err := c.portfolio.CreateAgentRun(c.ctx, CreateAgentRunRequest{
			Scope: c.scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: childTeam}, AssignedAgentID: loopFixtureAgent,
			Goal: "Get release 7 checked", Source: RunSourceObjective,
		})
		if err != nil {
			t.Fatal(err)
		}
		order := c.work(12)
		parent := c.mustRun(created.ID)
		c.mustStatus(parent, AgentRunStatusCompleted)

		requests, err := c.collab.ListAgentRequests(c.ctx, AgentRequestFilter{Scope: c.scope, SourceRunID: parent.ID, Limit: 10})
		if err != nil || len(requests) != 1 {
			t.Fatalf("requests = %d, %v", len(requests), err)
		}
		request := requests[0]
		if request.Status != AgentRequestStatusCompleted {
			t.Fatalf("request = %s", request.Status)
		}
		// Every claimed Run but the parent is one of the request's Runs.
		var decision, execution, review *AgentRun
		for _, id := range order {
			run := c.mustRun(id)
			switch run.Source {
			case RunSourceRequestDecision:
				decision = run
			case RunSourceCompletionReview:
				review = run
			case RunSourceRequest:
				execution = run
			}
		}
		if decision == nil || execution == nil || review == nil {
			t.Fatalf("claimed runs %v lack a decision/execution/review Run", order)
		}
		if decision.AssignedAgentID != childScout || execution.AssignedAgentID != childScout || review.AssignedAgentID != childReviewer ||
			execution.ID != request.ChildRunID {
			t.Fatalf("assignments: decision %s execution %s review %s (request child %s)",
				decision.AssignedAgentID, execution.AssignedAgentID, review.AssignedAgentID, request.ChildRunID)
		}
		for _, run := range []*AgentRun{decision, review} {
			if run.Budget == nil || run.Budget.MaxTurns != 12 {
				t.Fatalf("%s budget = %#v, want 12 turns", run.Source, run.Budget)
			}
			if run.Status != AgentRunStatusCompleted {
				t.Fatalf("%s = %s", run.Source, run.Status)
			}
		}
		// One session per Run. Cross-Agent Runs get fresh parentless
		// sessions (A3): another Agent never sees the requester's context.
		seen := map[string]string{}
		for _, run := range []*AgentRun{parent, decision, execution, review} {
			if run.SessionID == "" {
				t.Fatalf("%s %s has no session", run.Source, run.ID)
			}
			if other, dup := seen[run.SessionID]; dup {
				t.Fatalf("runs %s and %s share session %s", other, run.ID, run.SessionID)
			}
			seen[run.SessionID] = run.ID
			session := c.sessionByID(run.SessionID)
			if session.OwnerRunID != run.ID || session.ParentSessionID != "" || session.AgentID != run.AssignedAgentID {
				t.Fatalf("%s session = owner %s parent %q agent %s", run.Source, session.OwnerRunID, session.ParentSessionID, session.AgentID)
			}
			for _, e := range c.path(run.SessionID) {
				if e.RunID != run.ID {
					t.Fatalf("%s session holds entry %s of run %s", run.Source, e.ID, e.RunID)
				}
			}
		}
		// Decision and review Runs are offered only their typed final tool.
		for _, run := range []*AgentRun{decision, review} {
			for _, req := range c.requestsFor(run.ID) {
				for _, tool := range req.Tools {
					if tool.Name != string(ControlToolRequestDecision) && tool.Name != string(ControlToolCompletionReview) {
						t.Fatalf("%s offered %s", run.Source, tool.Name)
					}
				}
			}
		}
		// The review's verdict is on the request; the parent resumes only
		// after it, and its tool result is the execution's final text.
		if len(request.CompletionReviews) != 1 || !request.CompletionReviews[0].Approve ||
			request.CompletionReviews[0].Reviewer.ID != childReviewer || !strings.Contains(request.CompletionReviews[0].Summary, "every open issue") {
			t.Fatalf("completion reviews = %#v", request.CompletionReviews)
		}
		entry := c.toolEntry(parent.ID, string(ControlToolDelegate))
		value := childResultJSON(t, entry)
		if entry.State != ToolEntryCompleted || value["status"] != "completed" || value["answer"] != "Release 7 has 2 open issues." ||
			value["agentId"] != childScout || value["requestId"] != request.ID {
			t.Fatalf("delegate entry = %s %s", entry.State, childResultText(entry))
		}
		if steps := c.requestsFor(parent.ID); len(steps) != 2 {
			t.Fatalf("parent made %d model requests, want 2 (delegate, answer)", len(steps))
		}
		if got, _ := parent.Output[finalOutputSummaryKey].(string); !strings.Contains(got, "Release 7 has 2 open issues.") {
			t.Fatalf("parent answer = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Conversation tasks and promotion

func TestLoopConversationTaskStartsWithoutPark(t *testing.T) {
	eachStore(t, "child_task", func(t *testing.T, store KernelStore) {
		const ack = "On it, I will compile the report in the background."
		c := newChildFixture(t, store, withRules(
			&modelRule{name: "task-work", when: func(v stepView) bool {
				for _, text := range v.UserTexts() {
					if strings.Contains(text, "Compile the quarterly report") && !strings.Contains(text, "compile the quarterly report please") {
						return true
					}
				}
				return false
			}, reply: func(stepView) ModelStepResult { return say("The quarterly report is ready: revenue grew 4%.") }},
			&modelRule{name: "start", times: 1, reply: func(v stepView) ModelStepResult {
				return act("", call(string(ControlToolStartTask), map[string]any{"goal": "Compile the quarterly report", "acknowledgment": ack}))
			}},
			&modelRule{name: "follow-up", reply: func(v stepView) ModelStepResult { return say("Revenue grew 4%, as the task reported.") }},
		))
		c.authority = func(run *AgentRun, a *agentrun.ControlAuthority) {
			if run.Kind == RunKindConversation && run.Context[ConversationTaskContextKey] == nil {
				conversationID, _ := run.Context[conversationRunContextConversationID].(string)
				threadRootID, _ := run.Context["threadRootMessageId"].(string)
				a.TaskStart = &HostedConversationTaskContext{ConversationID: conversationID, ThreadRootID: threadRootID, CanStart: true}
			}
		}
		conversation, source := c.startChat("compile the quarterly report please")

		// One claim: the tool entry completes and the Run ends with the ack.
		exit, ended := c.drive(source.ID)
		if ended.Status != AgentRunStatusCompleted {
			t.Fatalf("source = %s (exit %s)", ended.Status, exit.Reason)
		}
		start := c.toolEntry(source.ID, string(ControlToolStartTask))
		if start.State != ToolEntryCompleted || start.Tool.Result.IsError || !strings.Contains(strings.ToLower(childResultText(start)), "started") {
			t.Fatalf("start entry = %s %q", start.State, childResultText(start))
		}
		if len(c.requestsFor(source.ID)) != 1 {
			t.Fatalf("source made %d model requests", len(c.requestsFor(source.ID)))
		}
		messages := c.messages(conversation.ID)
		if len(messages) != 2 || messages[1].Content != ack || messages[1].Sender.ID != loopFixtureAgent {
			t.Fatalf("channel after start = %s", kitDescribeMessages(messages))
		}

		// The task Run's session forks the chat session at the tool call.
		tasks, err := c.chats.(ConversationTaskKernelStore).ListConversationTasks(c.ctx, ConversationTaskFilter{
			Scope: c.scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 10,
		})
		if err != nil || len(tasks) != 1 || tasks[0].WorkRun == nil {
			t.Fatalf("tasks = %#v, %v", tasks, err)
		}
		work := c.mustRun(tasks[0].WorkRun.ID)
		chat := c.mustRun(source.ID).SessionID
		session := c.sessionByID(work.SessionID)
		assistant := c.sessionEntries(chat)
		var forkingAssistant *SessionEntry
		for _, e := range assistant {
			if e.Type == SessionEntryAssistant && e.StepID == start.StepID {
				forkingAssistant = e
			}
		}
		if forkingAssistant == nil || session.ParentSessionID != chat || session.ForkSequence != forkingAssistant.Sequence-1 || session.OwnerRunID != work.ID {
			t.Fatalf("task session = parent %q fork %d owner %s (chat %s)", session.ParentSessionID, session.ForkSequence, session.OwnerRunID, chat)
		}

		// The task works on its own session and reports to the chat.
		c.work(6)
		c.mustStatus(work, AgentRunStatusCompleted)
		for _, e := range c.sessionEntries(work.SessionID) {
			if e.RunID != work.ID {
				t.Fatalf("task session holds entry %s of run %s", e.ID, e.RunID)
			}
		}
		messages = c.messages(conversation.ID)
		var report *ChannelMessage
		for _, m := range messages {
			if strings.Contains(m.Content, "revenue grew 4%") {
				report = m
			}
		}
		if report == nil {
			t.Fatalf("no task report in %s", kitDescribeMessages(messages))
		}

		// The next chat Run catches the report up as a user entry from the
		// task's Agent.
		c.postUser(conversation.ID, loopFixtureUser, "what did it find?")
		c.work(4)
		var nextRun *AgentRun
		for _, run := range c.listRuns() {
			if run.Kind == RunKindConversation && run.ID != source.ID && run.Context[ConversationTaskContextKey] == nil {
				nextRun = run
			}
		}
		if nextRun == nil {
			t.Fatal("the follow-up message started no Run")
		}
		c.mustStatus(nextRun, AgentRunStatusCompleted)
		if nextRun.SessionID != chat {
			t.Fatalf("follow-up session = %s, want the chat session %s", nextRun.SessionID, chat)
		}
		first := c.requestsFor(nextRun.ID)[0]
		// The chat session's user entry for the report is attributed to the
		// task's Agent; the model sees it in the message header (a channel
		// message renders its sender there, not as an entry author).
		var caught *SessionEntry
		for _, e := range c.sessionEntries(chat) {
			if e.Type == SessionEntryUser && e.User != nil && e.User.MessageID == report.ID {
				caught = e
			}
		}
		if caught == nil || caught.RunID != nextRun.ID || caught.User.Author.Type != string(ConversationParticipantAgent) ||
			caught.User.Author.ID != loopFixtureAgent {
			t.Fatalf("report catch-up entry = %#v", caught)
		}
		var rendered string
		for _, text := range (stepView{req: first}).UserTexts() {
			if strings.Contains(text, "revenue grew 4%") {
				rendered = text
			}
		}
		if header, _, _ := strings.Cut(rendered, "\n"); !strings.Contains(header, loopFixtureAgent) || !strings.HasPrefix(header, "[Agent") {
			t.Fatalf("report renders as %q, want a header from the task's Agent", rendered)
		}
	})
}

func (c *childFixture) listRuns() []*AgentRun {
	c.t.Helper()
	runs, err := c.raw.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope})
	if err != nil {
		c.t.Fatal(err)
	}
	return runs
}

func TestLoopForegroundPromotionForksTheSession(t *testing.T) {
	eachStore(t, "child_promotion", func(t *testing.T, store KernelStore) {
		var c *childFixture
		var promoted *ConversationTaskResult
		var slowRunID string
		c = newChildFixture(t, store, withRules(
			&modelRule{name: "slow", times: 1, when: func(v stepView) bool { return strings.Contains(v.LastUserText(), "audit") },
				before: func(ctx context.Context, v stepView) error {
					// The step outlives the foreground timeout; the promoter
					// moves the Run off the lane.
					c.clock.Advance(ConversationTaskForegroundTimeout)
					run := c.mustRun(slowRunID)
					result, err := c.raw.(promotingChatLoopStore).PromoteConversationTask(ctx, ConversationTaskPromotionRequest{
						Scope: run.Scope, RunID: run.ID, Now: run.CreatedAt.Add(ConversationTaskForegroundTimeout),
					})
					if err != nil || result == nil {
						return fmt.Errorf("promote: %#v, %v", result, err)
					}
					promoted = result
					return nil
				},
				reply: func(v stepView) ModelStepResult {
					return act("Auditing the ledger.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "ledger"}))
				}},
			&modelRule{name: "audit-done", when: func(v stepView) bool { return len(v.ToolResultsSinceUser()) > 0 },
				reply: func(stepView) ModelStepResult { return say("The audit found no gaps.") }},
			&modelRule{name: "hello", when: func(v stepView) bool { return strings.Contains(v.LastUserText(), "hello") },
				reply: func(stepView) ModelStepResult { return say("Hi Ana.") }},
		))
		conversation, run := c.startChat("please audit the ledger")
		slowRunID = run.ID
		lane := run.SessionID
		if lane == "" {
			lane = conversationSessionID(c.scope, run.ConcurrencyKey)
		}
		c.driveToRestChild(run.ID, 4)
		if promoted == nil || promoted.Task == nil {
			t.Fatal("the Run was not promoted")
		}
		done := c.mustRun(run.ID)
		c.mustStatus(done, AgentRunStatusCompleted)
		childSession := done.SessionID
		if childSession == "" || childSession == lane || done.ConcurrencyKey == run.ConcurrencyKey {
			t.Fatalf("promoted Run session %q lane %q key %q", childSession, lane, done.ConcurrencyKey)
		}
		child := c.sessionByID(childSession)
		if child.ParentSessionID != lane || child.OwnerRunID != run.ID {
			t.Fatalf("promotion session = parent %q owner %q", child.ParentSessionID, child.OwnerRunID)
		}
		// The promoted Run's work after promotion is in the child session.
		var childTypes []string
		for _, e := range c.sessionEntries(childSession) {
			if e.RunID != run.ID {
				t.Fatalf("child session holds entry %s of run %s", e.ID, e.RunID)
			}
			childTypes = append(childTypes, string(e.Type))
		}
		if !strings.Contains(strings.Join(childTypes, ","), "assistant,tool") {
			t.Fatalf("child session = %v, want the step's assistant and tool entries", childTypes)
		}
		// The promoted work still answers in the chat, once.
		answers := 0
		for _, m := range c.messages(conversation.ID) {
			if strings.Contains(m.Content, "The audit found no gaps.") {
				answers++
			}
		}
		if answers != 1 {
			t.Fatalf("audit answers in the chat = %d: %s", answers, kitDescribeMessages(c.messages(conversation.ID)))
		}
		chatBefore := c.sessionEntries(lane)
		for _, e := range chatBefore {
			if e.RunID == run.ID && (e.Type == SessionEntryAssistant || e.Type == SessionEntryTool) {
				t.Fatalf("chat session holds the promoted Run's %s entry %s", e.Type, e.ID)
			}
		}

		// A new message starts a new Run on the chat session.
		c.postUser(conversation.ID, loopFixtureUser, "hello again")
		c.work(4)
		var next *AgentRun
		for _, r := range c.listRuns() {
			if r.Kind == RunKindConversation && r.ID != run.ID {
				next = r
			}
		}
		if next == nil {
			t.Fatal("the new message started no Run")
		}
		c.mustStatus(next, AgentRunStatusCompleted)
		if next.SessionID != lane {
			t.Fatalf("new Run session = %s, want the chat session %s", next.SessionID, lane)
		}
		for _, e := range c.sessionEntries(lane) {
			if e.Sequence <= chatBefore[len(chatBefore)-1].Sequence {
				continue
			}
			if e.RunID != next.ID {
				t.Fatalf("chat session gained entry %s of run %s after promotion", e.ID, e.RunID)
			}
		}
		for _, e := range c.sessionEntries(childSession) {
			if e.RunID != run.ID {
				t.Fatalf("child session gained entry %s of run %s", e.ID, e.RunID)
			}
		}
	})
}

// driveToRestChild is driveToRest through the child worker's binding.
func (c *childFixture) driveToRestChild(runID string, maxClaims int) *AgentRun {
	c.t.Helper()
	for i := 0; i < maxClaims; i++ {
		_, run := c.drive(runID)
		if run.Status != AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(c.clock.Now()) {
			c.clock.Advance(run.AvailableAt.Sub(c.clock.Now()) + time.Millisecond)
		}
	}
	c.t.Fatalf("run %s not at rest after %d claims", runID, maxClaims)
	return nil
}

// ---------------------------------------------------------------------------
// Runbook operations

var childEngageOperation = agentrun.HostedRunbookOperation{
	DefinitionID: "def-engage", DefinitionVersion: "1", Entrypoint: "engage-now",
	Name: "Engage accounts", Description: "Send the reviewed engagement sequence.",
	InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"account"},
		"properties": map[string]interface{}{"account": map[string]interface{}{"type": "string"}},
	},
}

func childEngageRunbook() *runbook.Definition {
	return &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: childEngageOperation.DefinitionID, Version: childEngageOperation.DefinitionVersion, Name: "Engage",
		Entrypoints: map[string]string{childEngageOperation.Entrypoint: "done"},
		Steps: map[string]runbook.Step{
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"summary": runbookLiteral("Engaged acme.")}}},
		},
	}
}

func newRunbookChildFixture(t *testing.T, store KernelStore) *childFixture {
	t.Helper()
	c := newChildFixture(t, store, withRules(
		&modelRule{name: "operation", times: 1, reply: func(stepView) ModelStepResult {
			return act("Starting the engagement.", call(string(ControlToolRunOperation), map[string]any{
				"entrypoint": childEngageOperation.Entrypoint, "arguments": map[string]any{"account": "acme"},
			}))
		}},
		&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
			content, isError, _ := v.ToolResult(string(ControlToolRunOperation))
			if isError {
				return say("The engagement failed.")
			}
			return say("Done: " + content)
		}},
	))
	c.runbooks[childEngageOperation.DefinitionID] = childEngageRunbook()
	c.authority = func(run *AgentRun, a *agentrun.ControlAuthority) {
		if run.Entrypoint == "" {
			a.Operations = []agentrun.HostedRunbookOperation{childEngageOperation}
		}
	}
	return c
}

func TestLoopRunbookStartParksForChild(t *testing.T) {
	eachStore(t, "child_runbook_start", func(t *testing.T, store KernelStore) {
		c := newRunbookChildFixture(t, store)
		parent := c.startRun(RunKindAgentWork, "Engage acme")
		if exit, _ := c.drive(parent.ID); exit.Reason != RunLoopExitParked {
			t.Fatalf("parent exit = %s", exit.Reason)
		}
		c.mustStatus(parent, AgentRunStatusWaitingForDependency)
		entry := c.toolEntry(parent.ID, string(ControlToolRunOperation))
		if entry.State != ToolEntryAwaitingChild {
			t.Fatalf("operation entry = %s %q", entry.State, childResultText(entry))
		}
		children := c.children(parent.ID)
		if len(children) != 1 || children[0].Entrypoint != childEngageOperation.Entrypoint {
			t.Fatalf("children = %#v", children)
		}
		child := children[0]
		order := c.work(6)
		if len(order) != 2 || order[0] != child.ID || order[1] != parent.ID {
			t.Fatalf("claims = %v, want the runbook child then the parent", order)
		}
		c.mustStatus(child, AgentRunStatusCompleted)
		c.mustStatus(parent, AgentRunStatusCompleted)
		entry = c.toolEntry(parent.ID, string(ControlToolRunOperation))
		value := childResultJSON(t, entry)
		if entry.State != ToolEntryCompleted || value["status"] != string(AgentRunStatusCompleted) || value["runId"] != child.ID ||
			value["entrypoint"] != childEngageOperation.Entrypoint {
			t.Fatalf("operation entry = %s %s", entry.State, childResultText(entry))
		}
		if got, _ := c.mustRun(parent.ID).Output[finalOutputSummaryKey].(string); !strings.HasPrefix(got, "Done: ") {
			t.Fatalf("parent answer = %q", got)
		}
	})
}

// TestDeterministicRunbookHasNoRenderedSession is §5.8's
// TestDeterministicRunbookHasNoSession under A3: a deterministic Run gets a
// run session that is never rendered. No model step runs for it, its session
// holds no model entries and is not forked from anything, and its engine
// state stays in the Run.
func TestDeterministicRunbookHasNoRenderedSession(t *testing.T) {
	eachStore(t, "child_runbook_session", func(t *testing.T, store KernelStore) {
		c := newRunbookChildFixture(t, store)
		parent := c.startRun(RunKindAgentWork, "Engage acme")
		c.work(6)
		c.mustStatus(parent, AgentRunStatusCompleted)
		children := c.children(parent.ID)
		if len(children) != 1 {
			t.Fatalf("children = %d", len(children))
		}
		child := c.mustRun(children[0].ID)
		c.mustStatus(child, AgentRunStatusCompleted)
		if n := len(c.requestsFor(child.ID)); n != 0 {
			t.Fatalf("the runbook Run made %d model requests", n)
		}
		if child.SessionID == "" {
			t.Fatal("the runbook Run has no run session")
		}
		session := c.sessionByID(child.SessionID)
		if session.ParentSessionID != "" || session.OwnerRunID != child.ID {
			t.Fatalf("runbook session = parent %q owner %q", session.ParentSessionID, session.OwnerRunID)
		}
		for _, e := range c.sessionEntries(child.SessionID) {
			if e.Type == SessionEntryAssistant || e.Type == SessionEntryUser {
				t.Fatalf("runbook session holds a %s entry", e.Type)
			}
		}
		if _, err := RunbookTraceFromCheckpoint(child.Checkpoint); err != nil || len(child.Checkpoint) == 0 {
			t.Fatalf("runbook engine state is not in the Run: %v", err)
		}
	})
}
