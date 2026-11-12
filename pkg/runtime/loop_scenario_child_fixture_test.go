package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// childFixture is a loop fixture with the child seams the worker wires: the
// fork coordinator, collaboration with a Team catalog, the request inbox and
// the runbook definitions of operation children.
type childFixture struct {
	*looptest.Fixture
	forks    *agentrun.RunForkCoordinator
	collab   *collaboration.CollaborationService
	inbox    *collaboration.AgentRequestInboxReconciler
	runbooks map[string]*runbook.Definition // by definition id
	// authority adjusts a model Run's control authority before its claim (a
	// test grants delegation targets, task start or operations here).
	authority func(run *models.AgentRun, authority *agentrun.ControlAuthority)
	// claims counts claims per Run.
	claims map[string]int
}

type childTestStore interface {
	models.KernelStore
	models.RunDependencyStore
	models.CollaborationKernelStore
	models.AgentRequestInboxStore
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
			{ID: "coordinator", AgentDeploymentID: looptest.FixtureAgent},
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

func newChildFixture(t *testing.T, store models.KernelStore, opts ...looptest.Option) *childFixture {
	t.Helper()
	f := looptest.NewFixture(t, store, opts...)
	// Task admission checks the source lease against the wall clock
	// (conversationrun/conversation_task.go), so the fake clock starts at wall time.
	if ahead := time.Until(f.Clock.Now()); ahead < 0 {
		f.Clock.Advance(-ahead)
	}
	stores, ok := f.Raw.(childTestStore)
	if !ok {
		t.Fatalf("%T lacks the dependency and collaboration stores", f.Raw)
	}
	teams := childTeamStore{childTestStore: stores, childTeamCatalog: childTeamCatalog{scope: capability.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}}}
	collab := collaboration.NewCollaborationService(teams, models.WithClock(f.Clock.Now))
	inbox, err := collaboration.NewAgentRequestInboxReconciler(teams, models.WithClock(f.Clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	c := &childFixture{
		Fixture: f, forks: agentrun.NewRunForkCoordinator(stores, models.WithClock(f.Clock.Now)), collab: collab,
		inbox: inbox, runbooks: map[string]*runbook.Definition{}, claims: map[string]int{},
	}
	c.forks.SetAcceptedRunExecutionPreparer(childRunbookPreparer{c})
	if tasks, ok := f.Raw.(models.ConversationTaskKernelStore); ok {
		f.Control.Tasks = conversationrun.NewConversationTaskService(tasks, models.WithClock(f.Clock.Now))
	}
	f.Control.Forks = c.forks
	// The task completion review reads the task through the portfolio.
	f.Control.Portfolio = f.Raw
	// A task's completion review: approve (Purpose acknowledgement).
	review := &looptest.ModelRule{Name: "task-review", When: func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeAcknowledgement },
		Reply: func(looptest.StepView) models.ModelStepResult {
			return looptest.Say(`{"decision":"approve","reason":"The report answers the accepted goal."}`)
		}}
	f.Model.Mu.Lock()
	f.Model.Rules = append([]*looptest.ModelRule{review}, f.Model.Rules...)
	f.Model.Mu.Unlock()
	f.Control.Collaboration = collab
	return c
}

// childRunbookPreparer pins an operation child to its registered definition
// (the catalog's accepted-execution preparer).
type childRunbookPreparer struct{ c *childFixture }

func (p childRunbookPreparer) PrepareAcceptedRunExecution(_ context.Context, request *models.CreateAgentRunRequest) error {
	request.Context = maps.Clone(request.Context)
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	delete(request.Context, models.AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	id, _ := request.Context["runbookDefinitionId"].(string)
	definition := p.c.runbooks[id]
	if definition == nil {
		return models.ErrAcceptedRunExecution
	}
	request.Context[models.AcceptedRunExecutionContextKey] = &models.AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: looptest.FixtureDefinition, DefinitionVersion: "1",
		RunbookID: definition.ID, RunbookVersion: definition.Version, Entrypoint: request.Entrypoint,
	}
	return nil
}

// claimNext claims the next queued Run of the scope; nil when none is due.
func (c *childFixture) claimNext() *models.AgentRun {
	c.T.Helper()
	claimed, err := c.Store.ClaimNextAgentRun(c.Ctx, models.AgentRunClaim{
		Scope: c.Scope, WorkerID: c.WorkerID, Now: c.Clock.Now(), LeaseDuration: looptest.FixtureLease, AgingInterval: time.Minute,
	})
	if err != nil {
		c.T.Fatalf("claim: %v", err)
	}
	return claimed
}

// drive claims runID (it must be the next queued Run) and drives one claim.
func (c *childFixture) drive(runID string) (runloop.RunLoopExit, *models.AgentRun) {
	c.T.Helper()
	claimed := c.claimNext()
	if claimed == nil || claimed.ID != runID {
		got := "<none>"
		if claimed != nil {
			got = claimed.ID + " (" + string(claimed.Source) + ")"
		}
		want := c.MustRun(runID)
		c.T.Logf("want %s: status %s available %s now %s key %q", runID, want.Status, want.AvailableAt, c.Clock.Now(), want.ConcurrencyKey)
		c.T.Fatalf("claimed %s, want %s", got, runID)
	}
	return c.driveClaimed(claimed)
}

func (c *childFixture) driveClaimed(claimed *models.AgentRun) (runloop.RunLoopExit, *models.AgentRun) {
	c.T.Helper()
	c.claims[claimed.ID]++
	binding := c.bindingFor(claimed)
	// The resolver may create the run session at first claim.
	claimed = c.MustRun(claimed.ID)
	exit := c.Loop.Drive(c.Ctx, runloop.RunLoopClaim{Run: claimed, WorkerID: c.WorkerID, Binding: binding})
	if exit.Err != nil && exit.Reason != runloop.RunLoopExitRequeued {
		after := c.MustRun(claimed.ID)
		c.T.Logf("after: status %s lease %q output %v", after.Status, after.LeaseOwner, after.Output)
		c.T.Fatalf("claim of %s exited %s: %v\nlog: %v", claimed.ID, exit.Reason, exit.Err, c.entryTypesOf(claimed.ID))
	}
	run := c.MustRun(claimed.ID)
	c.resolveTerminal(run)
	return exit, run
}

// work is the worker: it claims and drives every due Run (advancing the clock
// to a requeue's AvailableAt) and reconciles the request inbox, until nothing
// is queued. It returns the claimed Run ids in order.
func (c *childFixture) work(maxClaims int) []string {
	c.T.Helper()
	var order []string
	for i := 0; ; i++ {
		c.reconcileInbox()
		claimed := c.claimNext()
		if claimed == nil {
			next := c.nextAvailable()
			if next == nil {
				return order
			}
			c.Clock.Advance(next.Sub(c.Clock.Now()) + time.Millisecond)
			continue
		}
		if i >= maxClaims {
			c.T.Fatalf("worker not idle after %d claims (claimed %s)", maxClaims, claimed.ID)
		}
		order = append(order, claimed.ID)
		c.driveClaimed(claimed)
	}
}

// nextAvailable is the earliest future AvailableAt of a queued Run.
func (c *childFixture) nextAvailable() *time.Time {
	runs, err := c.Raw.ListAgentRuns(c.Ctx, models.AgentRunFilter{Scope: c.Scope, Statuses: []models.AgentRunStatus{models.AgentRunStatusQueued}})
	if err != nil {
		c.T.Fatal(err)
	}
	var next *time.Time
	for _, run := range runs {
		if at := run.AvailableAt; at.After(c.Clock.Now()) && (next == nil || at.Before(*next)) {
			at := at
			next = &at
		}
	}
	return next
}

func (c *childFixture) reconcileInbox() {
	c.T.Helper()
	if _, err := c.inbox.Reconcile(c.Ctx, c.Scope, ""); err != nil {
		c.T.Fatalf("reconcile request inbox: %v", err)
	}
}

// resolveTerminal is the worker pool after a terminal claim: terminal
// reporting, the fork edge, the AgentRequest decision/review/child.
func (c *childFixture) resolveTerminal(run *models.AgentRun) {
	c.T.Helper()
	if !models.IsTerminalAgentRunStatus(run.Status) {
		return
	}
	tolerable := func(err error) bool {
		return err == nil || errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidAgentRequestState) || errors.Is(err, models.ErrDependencyConflict)
	}
	if err := conversationrun.ProjectTerminalRunReporting(c.Ctx, c.Chats, run); err != nil {
		c.T.Fatalf("terminal reporting of %s: %v", run.ID, err)
	}
	if run.ParentRunID != "" && run.Checkpoint["forkChild"] != nil {
		if _, err := c.forks.CompleteChild(c.Ctx, run, models.ActivityActor{Type: "worker", ID: c.WorkerID}); !tolerable(err) {
			c.T.Fatalf("complete fork child %s: %v", run.ID, err)
		}
	}
	if _, err := c.inbox.ResolveDecisionRun(c.Ctx, run); !tolerable(err) {
		c.T.Fatalf("resolve decision %s: %v", run.ID, err)
	}
	if _, err := c.inbox.ResolveCompletionReviewRun(c.Ctx, run); !tolerable(err) {
		c.T.Fatalf("resolve review %s: %v", run.ID, err)
	}
	if _, err := c.collab.ResolveTerminalAgentRequestChild(c.Ctx, run); !tolerable(err) {
		c.T.Fatalf("resolve request child %s: %v", run.ID, err)
	}
}

// bindingFor is the claim's binding, the shape ResolveLoopBinding gives each
// kind: a chat reply binds to its lane through the conversation loop; a
// pinned runbook entrypoint is deterministic over an unrendered run session;
// every other Run is a model Run on its own session (A3).
func (c *childFixture) bindingFor(run *models.AgentRun) *agentrun.TurnRunnerBinding {
	c.T.Helper()
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	if run.Entrypoint != "" {
		return c.runbookBinding(run)
	}
	// The fixture has no Agent catalog: a Team Run's eligible targets are
	// granted by the scenario's authority hook instead of the Team roster.
	authorityRun := run
	if run.Owner.Type == models.OwnerTypeTeam && run.Context[models.ConversationRunConversationIDContextKey] == nil {
		copied := *run
		copied.Owner = models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: run.AssignedAgentID}
		authorityRun = &copied
	}
	control, err := control.ResolveControlAuthority(c.Ctx, control.ControlAuthorityInput{
		Run: authorityRun, DeploymentID: run.AssignedAgentID, Runs: c.Store, Conversations: c.Chats,
	})
	if err != nil {
		c.T.Fatalf("resolve control authority: %v", err)
	}
	if c.authority != nil {
		c.authority(run, &control)
	}
	host := looptest.FakeStepHost{ScriptedModel: c.Model, FakeHostTools: c.Tools}
	preparer := runloop.NewCatalogModelStepPreparer(runloop.CatalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
	})
	producer, err := modelstep.NewModelStepProducer(host, preparer, c.Store)
	if err != nil {
		c.T.Fatal(err)
	}
	binding := &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: looptest.FixtureAgent,
		DefinitionID: looptest.FixtureDefinition, DefinitionVersion: "1",
		ModelProvider: string(looptest.ModelProvider), Model: looptest.ModelName,
		Producer: producer, Host: host, Authority: control,
		Preparers: []agentrun.StepPreparer{runloop.NewRunStartPreparer(c.Store.(models.AgentSessionStore), c.Store)},
		Tools:     c.Dispatcher.ForClaim(runloop.LoopToolClaim{}),
	}
	_, decision := run.Context[agentrun.AgentRequestInboxContextKey]
	_, review := run.Context[agentrun.AgentRequestCompletionReviewContextKey]
	if !decision && !review {
		binding.ModelActions = append([]capability.ModelAction(nil), c.Offered...)
	}
	if run.Kind == models.RunKindConversation && conversationID != "" && triggerID != "" {
		return c.ConversationBinding(run, func(*models.AgentRun) *agentrun.TurnRunnerBinding { return binding })
	}
	binding.Session = c.runSession(run, true)
	return binding
}

// runSession is the Run's session: the one it is bound to, else a fresh,
// parentless run session created at this first claim (seeded with the Run's
// brief for a model Run).
func (c *childFixture) runSession(run *models.AgentRun, seeded bool) *models.AgentSession {
	c.T.Helper()
	sessions := c.Store.(models.AgentSessionStore)
	if run.SessionID != "" {
		session, err := models.ConversationRunSession(c.Ctx, sessions, run)
		if err != nil {
			c.T.Fatalf("run session of %s: %v", run.ID, err)
		}
		return session
	}
	var initial []*models.SessionEntry
	if seeded {
		entries, err := routine.SessionSeedEntries(c.Ctx, c.Store, nil, nil, run)
		if err != nil {
			c.T.Fatal(err)
		}
		initial = entries
	}
	now := c.Clock.Now()
	opened, err := sessions.CreateRunAgentSession(c.Ctx, models.CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID,
		Lease: models.AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}, InitialEntries: initial, Now: now,
	})
	if err != nil {
		c.T.Fatalf("create run session of %s: %v", run.ID, err)
	}
	return opened.Session
}

func (c *childFixture) runbookBinding(run *models.AgentRun) *agentrun.TurnRunnerBinding {
	c.T.Helper()
	id, _ := run.Context["runbookDefinitionId"].(string)
	definition := c.runbooks[id]
	if definition == nil {
		c.T.Fatalf("runbook Run %s pins unknown definition %q", run.ID, id)
	}
	producer, err := routine.NewRunbookStepProducer(definition, run.Entrypoint, c.Store)
	if err != nil {
		c.T.Fatal(err)
	}
	control, err := control.ResolveControlAuthority(c.Ctx, control.ControlAuthorityInput{Run: run, DeploymentID: run.AssignedAgentID, Deterministic: true})
	if err != nil {
		c.T.Fatal(err)
	}
	return &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: looptest.FixtureAgent,
		DefinitionID: looptest.FixtureDefinition, DefinitionVersion: "1",
		Producer: producer, Authority: control, Session: c.runSession(run, false),
		Tools: c.Dispatcher.ForClaim(runloop.LoopToolClaim{}),
	}
}

func (c *childFixture) entryTypesOf(runID string) []string {
	if c.MustRun(runID).SessionID == "" {
		return nil
	}
	return c.EntryTypes(runID)
}

func (c *childFixture) sessionByID(id string) *models.AgentSession {
	c.T.Helper()
	session, err := c.Raw.(models.AgentSessionStore).GetAgentSession(c.Ctx, c.Scope, id)
	if err != nil {
		c.T.Fatalf("get session %s: %v", id, err)
	}
	return session
}

// sessionEntries is the session's own entries (not its parent's), in order.
func (c *childFixture) sessionEntries(sessionID string) []*models.SessionEntry {
	c.T.Helper()
	entries, err := c.Raw.(models.AgentSessionStore).ListAgentSessionEntries(c.Ctx, models.SessionEntryFilter{Scope: c.Scope, SessionID: sessionID, Limit: 1000})
	if err != nil {
		c.T.Fatal(err)
	}
	return entries
}

// path is the session's rendered path (the parent up to the fork, then its own).
func (c *childFixture) path(sessionID string) []*models.SessionEntry {
	c.T.Helper()
	path, err := c.Raw.(models.AgentSessionStore).LoadAgentSessionPath(c.Ctx, c.Scope, sessionID)
	if err != nil {
		c.T.Fatal(err)
	}
	return path.Entries
}

// toolEntry is the Run's newest tool entry named name, in its own session.
func (c *childFixture) toolEntry(runID, name string) *models.SessionEntry {
	c.T.Helper()
	entries := c.sessionEntries(c.MustRun(runID).SessionID)
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Type == models.SessionEntryTool && e.Tool != nil && e.Tool.Name == name && e.RunID == runID {
			return e
		}
	}
	c.T.Fatalf("run %s has no %s entry: %v", runID, name, c.EntryTypes(runID))
	return nil
}

func (c *childFixture) toolEntries(runID, name string) []*models.SessionEntry {
	c.T.Helper()
	var out []*models.SessionEntry
	for _, e := range c.sessionEntries(c.MustRun(runID).SessionID) {
		if e.Type == models.SessionEntryTool && e.Tool != nil && e.Tool.Name == name && e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

func (c *childFixture) children(parentID string) []*models.AgentRun {
	c.T.Helper()
	runs, err := c.Raw.ListAgentRuns(c.Ctx, models.AgentRunFilter{Scope: c.Scope, ParentRunID: parentID})
	if err != nil {
		c.T.Fatal(err)
	}
	return runs
}

// requestsFor is the model requests the scripted model got for runID.
func (c *childFixture) requestsFor(runID string) []models.ModelStepRequest {
	var out []models.ModelStepRequest
	for _, req := range c.Model.Calls("") {
		if req.RunID == runID {
			out = append(out, req)
		}
	}
	return out
}

func childResultText(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return looptest.PartsText(entry.Tool.Result.Content)
}

func childResultJSON(t *testing.T, entry *models.SessionEntry) map[string]interface{} {
	t.Helper()
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(childResultText(entry)), &value); err != nil {
		t.Fatalf("result of %s is not JSON: %q", entry.ID, childResultText(entry))
	}
	return value
}

func entryIDs(entries []*models.SessionEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func entryViewIDs(entries []models.SessionEntryView) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// isForkBranch reports whether a request belongs to a fork branch: its first
// user entry after the parent's path is the branch brief.
func isForkBranch(v looptest.StepView) bool {
	for _, text := range v.UserTexts() {
		if strings.Contains(text, "one branch of a parallel task") {
			return true
		}
	}
	return false
}

func branchGoal(v looptest.StepView) string {
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

func childForkCall(goals ...string) looptest.ToolCall {
	branches := make([]interface{}, len(goals))
	for i, goal := range goals {
		branches[i] = map[string]interface{}{"goal": goal}
	}
	return looptest.Call(string(models.ControlToolFork), map[string]any{"branches": branches})
}
