package runtime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRunForkCoordinatorCreatesConcurrentChildrenAndReplays(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (atomicForkStore, func())
	}{
		{name: "memory", open: func(*testing.T) (atomicForkStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (atomicForkStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "coordinator.db"))
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
			scope := Scope{Kind: "tenant", ID: "7"}
			source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "parent", Source: RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := NewAgentRunScheduler(store).ClaimNext(t.Context(), AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
			if err != nil || claimed == nil || claimed.ID != source.ID {
				t.Fatalf("claim=%#v error=%v", claimed, err)
			}
			request := CreateRunForkRequest{
				Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "research-wave",
				Policy: RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait},
				Branches: []RunForkBranch{
					{ID: "forums", Goal: "Research forums", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "forums"}}},
					{ID: "reviews", Goal: "Research reviews", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "reviews"}}},
				},
				ContinuationCheckpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "join", "waiting": "research-wave"}},
			}
			coordinator := NewRunForkCoordinator(store)
			created, err := coordinator.Create(t.Context(), request)
			if err != nil || created.DependencyGroup.Replayed || len(created.Children) != 2 {
				t.Fatalf("created=%#v error=%v", created, err)
			}
			if created.DependencyGroup.Source.Status != AgentRunStatusWaitingForDependency || created.DependencyGroup.Source.WakeCondition == nil || created.DependencyGroup.Source.WakeCondition.Reference != created.DependencyGroup.Group.ID {
				t.Fatalf("source=%#v", created.DependencyGroup.Source)
			}
			for _, child := range created.Children {
				if child.ParentRunID != source.ID || child.RootRunID != source.RootRunID || child.Source != RunSourceFork || child.Status != AgentRunStatusQueued || child.ConcurrencyKey == "" {
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
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "parent", Source: RunSourceManual,
		Budget: &BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAgentRunScheduler(store).ClaimNext(t.Context(), AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunForkCoordinator(store).Create(t.Context(), CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "bounded",
		Policy:   RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{{ID: "a", Goal: "A", Checkpoint: map[string]interface{}{}}, {ID: "b", Goal: "B", Checkpoint: map[string]interface{}{}}},
	})
	if err == nil {
		t.Fatal("budgeted fork accepted implicit unbounded child allocations")
	}
	children, listErr := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if listErr != nil || len(children) != 0 {
		t.Fatalf("implicit allocation created children=%#v error=%v", children, listErr)
	}
	request := CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "bounded",
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{
			{ID: "a", Goal: "A", Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 6}},
			{ID: "b", Goal: "B", Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 6}},
		},
	}
	if _, err = NewRunForkCoordinator(store).Create(t.Context(), request); err == nil {
		t.Fatal("fork accepted child allocations beyond the parent ceiling")
	}
	children, listErr = store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if listErr != nil || len(children) != 0 {
		t.Fatalf("excessive allocation created children=%#v error=%v", children, listErr)
	}
	request.Branches[0].Budget.MaxTurns = 4
	request.Branches[1].Budget.MaxTurns = 4
	created, err := NewRunForkCoordinator(store).Create(t.Context(), request)
	if err != nil || len(created.Children) != 2 || len(created.DependencyGroup.Source.BudgetAllocations) != 2 {
		t.Fatalf("bounded fork=%#v error=%v", created, err)
	}
}

func TestRunForkCoordinatorCarriesCallableRunbookEntrypoint(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "parent", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAgentRunScheduler(store).ClaimNext(t.Context(), AgentRunClaimRequest{
		Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewRunForkCoordinator(store).Create(t.Context(), CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision,
		WorkerID: "worker", ForkID: "runbook-collect",
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{{
			ID: "operation", Goal: "Collect evidence", AssignedAgentID: "agent", Entrypoint: "collect",
			Context: map[string]interface{}{"release": "2.0.0"}, Checkpoint: map[string]interface{}{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Children) != 1 || created.Children[0].Kind != RunKindAgentWork || created.Children[0].Entrypoint != "collect" ||
		created.Children[0].Context["release"] != "2.0.0" {
		t.Fatalf("created=%#v", created)
	}
}

func TestRunForkCoordinatorPreservesInitiativeWithoutTransferringPrivateContext(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "lead", Goal: "Coordinate research", Source: RunSourceManual,
		Context: map[string]interface{}{"initiativeId": "initiative-1", "privateCredentialRef": "source-only", "privateBrief": "lead-only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAgentRunScheduler(store).ClaimNext(t.Context(), AgentRunClaimRequest{Scope: scope, WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRunForkRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "worker", ForkID: "research-wave",
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{
			{ID: "forums", AssignedAgentID: "researcher-a", Goal: "Research forums", Context: map[string]interface{}{"product": "A"}, Checkpoint: map[string]interface{}{}},
			{ID: "reviews", AssignedAgentID: "researcher-b", Goal: "Research reviews", Context: map[string]interface{}{"product": "B"}, Checkpoint: map[string]interface{}{}},
		},
		ContinuationCheckpoint: map[string]interface{}{},
	}
	drift := request
	drift.ForkID = "drifted-wave"
	drift.Branches = append([]RunForkBranch(nil), request.Branches...)
	drift.Branches[0].Context = map[string]interface{}{"initiativeId": "another-initiative"}
	if _, err = NewRunForkCoordinator(store).Create(t.Context(), drift); err == nil {
		t.Fatal("fork replaced authoritative Initiative lineage")
	}
	created, err := NewRunForkCoordinator(store).Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range created.Children {
		if child.Context["initiativeId"] != "initiative-1" || child.Context["product"] == nil || child.Context["privateCredentialRef"] != nil || child.Context["privateBrief"] != nil {
			t.Fatalf("child context = %#v", child.Context)
		}
	}
}
