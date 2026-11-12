package agentruntest

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

// DependencyStore is a kernel store that also reconciles Run dependency
// groups atomically, as the SQLite and Postgres stores do.
type DependencyStore interface {
	models.KernelStore
	models.DependencyKernelStore
	models.RunDependencyReconciliationStore
}

// DependencyFixture is a claimed source Run forked into claimed branch Runs
// ("alpha", "beta", and "gamma" for quorum) that it waits on through one
// dependency group. Its clock is fixed at Now; each step it takes runs a few
// seconds later.
type DependencyFixture struct {
	Store    DependencyStore
	Scope    models.Scope
	GroupID  string
	SourceID string
	ChildIDs map[string]string
	Now      time.Time
}

// DependencySnapshot is the durable state of a fixture: its group, edges,
// source, children by branch and activity.
type DependencySnapshot struct {
	Group    *models.RunDependencyGroup
	Edges    []*models.RunDependency
	Source   *models.AgentRun
	Children map[string]*models.AgentRun
	Events   []*models.ActivityEvent
}

// NewDependencyFixture forks a source Run in a new tenant scope, joining all
// branches and waiting on failures.
func NewDependencyFixture(t *testing.T, store DependencyStore) *DependencyFixture {
	t.Helper()
	return NewDependencyFixtureInScope(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()})
}

// NewDependencyFixtureInScope is NewDependencyFixture in scope.
func NewDependencyFixtureInScope(t *testing.T, store DependencyStore, scope models.Scope) *DependencyFixture {
	t.Helper()
	return NewDependencyFixtureWithPolicy(t, store, scope, models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureWait})
}

// NewDependencyFixtureWithPolicy forks a source Run in scope with policy.
// Forks offer all/any joins; a quorum group is created through the
// dependency coordinator over independently created branch Runs.
func NewDependencyFixtureWithPolicy(t *testing.T, store DependencyStore, scope models.Scope, policy models.RunDependencyPolicy) *DependencyFixture {
	t.Helper()
	fixture := &DependencyFixture{Store: store, Scope: scope, ChildIDs: make(map[string]string), Now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	portfolioService := portfolio.NewService(store, models.WithClock(func() time.Time { return fixture.Now }))
	source, err := portfolioService.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: fixture.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead", Goal: "Join two independently running branches", Source: models.RunSourceManual, Checkpoint: map[string]interface{}{"committed": "preserve"}, Budget: &models.BudgetPolicy{MaxTurns: 10}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.SourceID = source.ID
	schedulerClock := func() time.Time { return fixture.Now.Add(time.Second) }
	scheduler := agentrun.NewScheduler(store, models.WithClock(func() time.Time { return schedulerClock() }))
	claimed, err := scheduler.ClaimNext(t.Context(), agentrun.ClaimRequest{Scope: fixture.Scope, WorkerID: "source-worker", LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != source.ID {
		t.Fatalf("claim source=%#v, %v", claimed, err)
	}
	forks := agentrun.NewRunForkCoordinator(store, models.WithClock(func() time.Time { return fixture.Now.Add(2 * time.Second) }))
	branches := []agentrun.RunForkBranch{
		{ID: "alpha", Goal: "Branch alpha", Checkpoint: map[string]interface{}{"branch": "alpha"}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
		{ID: "beta", Goal: "Branch beta", Checkpoint: map[string]interface{}{"branch": "beta"}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
	}
	if policy.Mode == models.FanInModeQuorum {
		branches = append(branches, agentrun.RunForkBranch{ID: "gamma", Goal: "Branch gamma", Checkpoint: map[string]interface{}{"branch": "gamma"}, Budget: &models.BudgetPolicy{MaxTurns: 2}})
	}
	var group *models.RunDependencyResult
	if policy.Mode == models.FanInModeQuorum {
		// Forks expose all/any today. The general dependency coordinator owns
		// quorum groups and accepts normally created independent target Runs.
		dependencies := make([]models.RunDependencySpec, 0, len(branches))
		for _, branch := range branches {
			child, err := portfolioService.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
				Scope: fixture.Scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
				Goal: branch.Goal, Source: models.RunSourceRequest, Checkpoint: branch.Checkpoint, Budget: branch.Budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			dependencies = append(dependencies, models.RunDependencySpec{ID: branch.ID, TargetRunID: child.ID, Kind: models.RunDependencyKindRun})
		}
		coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return fixture.Now.Add(2 * time.Second) }))
		group, err = coordinator.CreateRunDependencyGroup(t.Context(), models.CreateRunDependencyGroupRequest{
			Scope: fixture.Scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision,
			Policy: policy, Dependencies: dependencies, Actor: models.ActivityActor{Type: "worker", ID: "source-worker"},
		})
	} else {
		var created *agentrun.RunForkResult
		created, err = forks.Create(t.Context(), agentrun.CreateRunForkRequest{
			Scope: fixture.Scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "source-worker", ForkID: "retirement-wave",
			Policy: policy, Branches: branches, ContinuationCheckpoint: map[string]interface{}{"committed": "preserve", "continuation": "join"},
		})
		if err == nil {
			group = created.DependencyGroup
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	fixture.GroupID = group.Group.ID
	for _, edge := range group.Dependencies {
		fixture.ChildIDs[edge.ID] = edge.TargetRunID
	}
	if len(fixture.ChildIDs) != len(branches) || fixture.ChildIDs["alpha"] == "" || fixture.ChildIDs["beta"] == "" {
		t.Fatalf("fork edges=%#v", group.Dependencies)
	}
	schedulerClock = func() time.Time { return fixture.Now.Add(3 * time.Second) }
	for range len(branches) {
		child, err := scheduler.ClaimNext(t.Context(), agentrun.ClaimRequest{Scope: fixture.Scope, WorkerID: "child-worker", LeaseDuration: time.Minute})
		if err != nil || child == nil || child.ParentRunID != source.ID {
			t.Fatalf("claim child=%#v, %v", child, err)
		}
	}
	return fixture
}

// Coordinator returns a dependency coordinator on the fixture's store.
func (f *DependencyFixture) Coordinator() *portfolio.DependencyCoordinator {
	coordinator := portfolio.NewDependencyCoordinator(f.Store, models.WithClock(func() time.Time { return f.Now.Add(10 * time.Second) }))
	return coordinator
}

// Request asks to reconcile the group at its current and its source's
// current revision.
func (f *DependencyFixture) Request(t *testing.T) portfolio.ReconcileRunDependencyGroupRequest {
	t.Helper()
	snapshot := f.Snapshot(t)
	return portfolio.ReconcileRunDependencyGroupRequest{Scope: f.Scope, GroupID: f.GroupID, ExpectedGroupRevision: snapshot.Group.Revision, ExpectedSourceRevision: snapshot.Source.Revision, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}, Visibility: models.ActivityVisibilityScope}
}

// Snapshot reads the fixture's durable state.
func (f *DependencyFixture) Snapshot(t *testing.T) DependencySnapshot {
	t.Helper()
	group, err := f.Store.GetRunDependencyGroup(t.Context(), f.Scope, f.GroupID)
	if err != nil || group == nil {
		t.Fatalf("get group=%#v, %v", group, err)
	}
	edges, err := f.Store.ListRunDependencies(t.Context(), f.Scope, f.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	source, err := f.Store.GetAgentRun(t.Context(), f.Scope, f.SourceID)
	if err != nil || source == nil {
		t.Fatalf("get source=%#v, %v", source, err)
	}
	children := make(map[string]*models.AgentRun, len(f.ChildIDs))
	for branch, id := range f.ChildIDs {
		child, err := f.Store.GetAgentRun(t.Context(), f.Scope, id)
		if err != nil || child == nil {
			t.Fatalf("get child=%#v, %v", child, err)
		}
		children[branch] = child
	}
	events, err := f.Store.ListActivity(t.Context(), models.ActivityFilter{Scope: f.Scope, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return DependencySnapshot{Group: group, Edges: edges, Source: source, Children: children, Events: events}
}

// FinishChild moves the branch's Run to status with a durable output, as
// its worker would, and returns it as stored.
func (f *DependencyFixture) FinishChild(t *testing.T, branch string, status models.AgentRunStatus) *models.AgentRun {
	t.Helper()
	child, err := f.Store.GetAgentRun(t.Context(), f.Scope, f.ChildIDs[branch])
	if err != nil || child == nil {
		t.Fatalf("get child=%#v, %v", child, err)
	}
	service := activity.NewRunActivityService(f.Store, f.Store, models.WithClock(func() time.Time { return f.Now.Add(4 * time.Second) }))
	errorText := ""
	if status != models.AgentRunStatusCompleted {
		errorText = "durable " + string(status) + " child outcome"
	}
	_, _, err = service.TransitionRun(t.Context(), f.Scope, child.ID, activity.RunTransitionRequest{ExpectedRevision: child.Revision, Status: status, Actor: models.ActivityActor{Type: "worker", ID: "child-worker"}, LeaseOwner: "child-worker", Checkpoint: map[string]interface{}{"branch": branch, "committed": "terminal"}, Output: map[string]interface{}{"message": "actual " + branch + " result", "nested": map[string]interface{}{"verified": true}}, Error: errorText, BudgetUsageDelta: &models.BudgetUsage{Turns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	child, err = f.Store.GetAgentRun(t.Context(), f.Scope, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

// ResolveChild resolves the branch's edge directly from child's outcome.
func (f *DependencyFixture) ResolveChild(t *testing.T, branch string, child *models.AgentRun) *models.RunDependencyResult {
	t.Helper()
	edge := DependencyEdge(t, f.Snapshot(t).Edges, branch)
	result, err := f.Coordinator().ResolveRunDependency(t.Context(), portfolio.ResolveRunDependencyRequest{Scope: f.Scope, GroupID: f.GroupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: DependencyState(child.Status), Result: DependencyChildResult(edge.ID, child), Error: child.Error, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
	if err != nil || result == nil {
		t.Fatalf("resolve child=%#v, %v", result, err)
	}
	return result
}

// Command applies an operator command to the source Run and returns it as
// stored.
func (f *DependencyFixture) Command(t *testing.T, kind agentrun.CommandKind) *models.AgentRun {
	t.Helper()
	source, err := f.Store.GetAgentRun(t.Context(), f.Scope, f.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	commands := agentrun.NewRunCommandService(f.Store, models.WithClock(func() time.Time { return f.Now.Add(5 * time.Second) }))
	result, err := commands.CommandAgentRun(t.Context(), agentrun.CommandRequest{Scope: f.Scope, RunID: f.SourceID, ExpectedRevision: source.Revision, Kind: kind, Actor: models.ActivityActor{Type: "user", ID: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	// Fetch the backend's canonical JSON representation for cross-store oracles.
	source, err = f.Store.GetAgentRun(t.Context(), f.Scope, result.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// TerminateSource ends the source Run with status: canceled by command,
// completed or failed through a run transition.
func (f *DependencyFixture) TerminateSource(t *testing.T, status models.AgentRunStatus) {
	t.Helper()
	if status == models.AgentRunStatusCanceled {
		f.Command(t, agentrun.CommandCancel)
		return
	}
	service := activity.NewRunActivityService(f.Store, f.Store, models.WithClock(func() time.Time { return f.Now.Add(5 * time.Second) }))
	source := f.Snapshot(t).Source
	if status == models.AgentRunStatusCompleted {
		var err error
		source, _, err = service.TransitionRun(t.Context(), f.Scope, source.ID, activity.RunTransitionRequest{ExpectedRevision: source.Revision, Status: models.AgentRunStatusRunning, Actor: models.ActivityActor{Type: "user", ID: "operator"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	errorText := ""
	if status == models.AgentRunStatusFailed {
		errorText = "parent execution failed"
	}
	if _, _, err := service.TransitionRun(t.Context(), f.Scope, source.ID, activity.RunTransitionRequest{ExpectedRevision: source.Revision, Status: status, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Error: errorText}); err != nil {
		t.Fatal(err)
	}
}

// AssertSnapshot fails the test unless the durable state equals want.
func (f *DependencyFixture) AssertSnapshot(t *testing.T, want DependencySnapshot) {
	t.Helper()
	if got := f.Snapshot(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconciliation mutated durable facts on rejection or replay:\nwant=%#v\ngot=%#v", want, got)
	}
}

// AssertReplayUnchanged replays request three times and fails the test if
// a replay emits work or changes the durable state from want.
func (f *DependencyFixture) AssertReplayUnchanged(t *testing.T, request portfolio.ReconcileRunDependencyGroupRequest, want DependencySnapshot) {
	t.Helper()
	for range 3 {
		result, err := f.Coordinator().ReconcileRunDependencyGroup(t.Context(), request)
		if err != nil || result == nil || !result.Replayed || len(result.Events) != 0 || result.Evaluation.Wake {
			t.Fatalf("replay made work or emitted a wake: %#v, %v", result, err)
		}
		f.AssertSnapshot(t, want)
	}
}

// AssertWaitingWork fails the test unless the scope lists want waiting
// groups; for one, it must be the fixture's group at its current revisions
// and ready as given (default not ready).
func (f *DependencyFixture) AssertWaitingWork(t *testing.T, want int, ready ...bool) {
	t.Helper()
	work, err := f.Store.ListWaitingRunDependencyGroups(t.Context(), f.Scope, "", 100)
	if err != nil || len(work) != want {
		t.Fatalf("waiting work=%#v, %v; want %d", work, err, want)
	}
	if want == 1 {
		snapshot := f.Snapshot(t)
		wantReady := false
		if len(ready) != 0 {
			wantReady = ready[0]
		}
		if work[0].GroupID != f.GroupID || work[0].SourceRunID != f.SourceID || work[0].GroupRevision != snapshot.Group.Revision || work[0].SourceRevision != snapshot.Source.Revision || work[0].Scope != f.Scope || work[0].Ready != wantReady {
			t.Fatalf("waiting projection is not exact: %#v", work[0])
		}
	}
}

// DependencyEdge returns the edge with id, failing the test if none.
func DependencyEdge(t *testing.T, edges []*models.RunDependency, id string) *models.RunDependency {
	t.Helper()
	for _, edge := range edges {
		if edge.ID == id {
			return edge
		}
	}
	t.Fatalf("dependency %s not found in %#v", id, edges)
	return nil
}

// DependencyState is the edge state a child Run with status resolves to.
func DependencyState(status models.AgentRunStatus) models.RunDependencyState {
	switch status {
	case models.AgentRunStatusCompleted:
		return models.RunDependencyStateSatisfied
	case models.AgentRunStatusFailed:
		return models.RunDependencyStateFailed
	default:
		return models.RunDependencyStateCanceled
	}
}

// DependencyChildResult is the result an edge records for child.
func DependencyChildResult(branch string, child *models.AgentRun) map[string]interface{} {
	return map[string]interface{}{"branchId": branch, "checkpoint": child.Checkpoint, "output": child.Output}
}
