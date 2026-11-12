package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRunForkCoordinatorCreatesConcurrentChildrenAndReplays(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (atomicForkStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (atomicForkStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "coordinator.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			scope := models.Scope{Kind: "tenant", ID: "7"}
			source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", ActingUser: "7", Goal: "parent", Source: models.RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := agentrun.NewAgentRunScheduler(store).ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
			if err != nil || claimed == nil || claimed.ID != source.ID {
				t.Fatalf("claim=%#v error=%v", claimed, err)
			}
			request := agentrun.CreateRunForkRequest{
				Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "research-wave",
				Policy: models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait},
				Branches: []agentrun.RunForkBranch{
					{ID: "forums", Goal: "Research forums", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "forums"}}},
					{ID: "reviews", Goal: "Research reviews", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "reviews"}}},
				},
				ContinuationCheckpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "join", "waiting": "research-wave"}},
			}
			coordinator := agentrun.NewRunForkCoordinator(store)
			created, err := coordinator.Create(t.Context(), request)
			if err != nil || created.DependencyGroup.Replayed || len(created.Children) != 2 {
				t.Fatalf("created=%#v error=%v", created, err)
			}
			if created.DependencyGroup.Source.Status != models.AgentRunStatusWaitingForDependency || created.DependencyGroup.Source.WakeCondition == nil || created.DependencyGroup.Source.WakeCondition.Reference != created.DependencyGroup.Group.ID {
				t.Fatalf("source=%#v", created.DependencyGroup.Source)
			}
			for _, child := range created.Children {
				if child.ParentRunID != source.ID || child.RootRunID != source.RootRunID || child.Source != models.RunSourceFork || child.Status != models.AgentRunStatusQueued || child.ConcurrencyKey == "" || child.ActingUser != "7" {
					t.Fatalf("child=%#v", child)
				}
			}
			replayed, err := coordinator.Create(t.Context(), request)
			if err != nil || !replayed.DependencyGroup.Replayed || len(replayed.Children) != 2 || replayed.Children[0].ID != created.Children[0].ID {
				t.Fatalf("replayed=%#v error=%v", replayed, err)
			}
		})
	}
}

func TestRunForkCoordinatorRequiresExplicitChildBudgets(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "7"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "parent", Source: models.RunSourceManual,
		Budget: &models.BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := agentrun.NewAgentRunScheduler(store).ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agentrun.NewRunForkCoordinator(store).Create(t.Context(), agentrun.CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "bounded",
		Policy:   models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches: []agentrun.RunForkBranch{{ID: "a", Goal: "A", Checkpoint: map[string]interface{}{}}, {ID: "b", Goal: "B", Checkpoint: map[string]interface{}{}}},
	})
	if err == nil {
		t.Fatal("budgeted fork accepted implicit unbounded child allocations")
	}
	children, listErr := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if listErr != nil || len(children) != 0 {
		t.Fatalf("implicit allocation created children=%#v error=%v", children, listErr)
	}
	request := agentrun.CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "bounded",
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches: []agentrun.RunForkBranch{
			{ID: "a", Goal: "A", Checkpoint: map[string]interface{}{}, Budget: &models.BudgetPolicy{MaxTurns: 6}},
			{ID: "b", Goal: "B", Checkpoint: map[string]interface{}{}, Budget: &models.BudgetPolicy{MaxTurns: 6}},
		},
	}
	if _, err = agentrun.NewRunForkCoordinator(store).Create(t.Context(), request); err == nil {
		t.Fatal("fork accepted child allocations beyond the parent ceiling")
	}
	children, listErr = store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if listErr != nil || len(children) != 0 {
		t.Fatalf("excessive allocation created children=%#v error=%v", children, listErr)
	}
	request.Branches[0].Budget.MaxTurns = 4
	request.Branches[1].Budget.MaxTurns = 4
	created, err := agentrun.NewRunForkCoordinator(store).Create(t.Context(), request)
	if err != nil || len(created.Children) != 2 || len(created.DependencyGroup.Source.BudgetAllocations) != 2 {
		t.Fatalf("bounded fork=%#v error=%v", created, err)
	}
}

func TestRunForkCoordinatorCarriesCallableRunbookEntrypoint(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "7"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "parent", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := agentrun.NewAgentRunScheduler(store).ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{
		Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := agentrun.NewRunForkCoordinator(store)
	coordinator.SetAcceptedRunExecutionPreparer(&acceptedRunInheritancePreparer{prepare: func(request *models.CreateAgentRunRequest) error {
		setAcceptedRunInheritancePin(request)
		return nil
	}})
	created, err := coordinator.Create(t.Context(), agentrun.CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision,
		WorkerID: "worker", ForkID: "runbook-collect",
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches: []agentrun.RunForkBranch{{
			ID: "operation", Goal: "Collect evidence", AssignedAgentID: "agent", Entrypoint: "collect",
			Context: map[string]interface{}{"release": "2.0.0"}, Checkpoint: map[string]interface{}{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Children) != 1 || created.Children[0].Kind != models.RunKindAgentWork || created.Children[0].Entrypoint != "collect" ||
		created.Children[0].Context["release"] != "2.0.0" {
		t.Fatalf("created=%#v", created)
	}
}

func TestRunForkCoordinatorDoesNotTransferPrivateContext(t *testing.T) {
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "7"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "lead", Goal: "Coordinate research", Source: models.RunSourceManual,
		Context: map[string]interface{}{"privateCredentialRef": "source-only", "privateBrief": "lead-only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := agentrun.NewAgentRunScheduler(store).ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := agentrun.CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "research-wave",
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches: []agentrun.RunForkBranch{
			{ID: "forums", AssignedAgentID: "researcher-a", Goal: "Research forums", Context: map[string]interface{}{"product": "A"}, Checkpoint: map[string]interface{}{}},
			{ID: "reviews", AssignedAgentID: "researcher-b", Goal: "Research reviews", Context: map[string]interface{}{"product": "B"}, Checkpoint: map[string]interface{}{}},
		},
		ContinuationCheckpoint: map[string]interface{}{},
	}
	created, err := agentrun.NewRunForkCoordinator(store).Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range created.Children {
		if child.Context["product"] == nil || child.Context["privateCredentialRef"] != nil || child.Context["privateBrief"] != nil {
			t.Fatalf("child context = %#v", child.Context)
		}
	}
}
