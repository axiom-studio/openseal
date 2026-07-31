package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestSQLiteAgentDefinitionActivationAdvancesRunbookAtomicallyAndRollsBack(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runbook-activation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := kernelagent.NewRegistryWithStore(store)
	for _, item := range []struct {
		version string
		turns   int64
	}{{"1.0.0", 2}, {"1.1.0", 7}} {
		definition := sqliteAgentDefinition(item.version)
		definition.Runbook = sqliteAgentRunbook(item.version, item.turns)
		if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "runbook-activation"}
	deployment, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "operator", Scope: scope, DefinitionID: "operator", ActiveVersion: "1.0.0", RolloutStatus: kernelagent.RolloutActive,
		Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: owner, Title: "Operate", Goal: "Operate safely", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	definitionV1, _ := registry.GetDefinition(ctx, "operator", "1.0.0")
	trigger := definitionV1.Runbook.Triggers["hourly"]
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "operator-hourly", Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: owner, ObjectiveID: objective.ID,
		AssignedAgentID: deployment.ID, DefinitionID: definitionV1.Runbook.ID, DefinitionVersion: definitionV1.Runbook.Version,
		TriggerID: "hourly", Trigger: trigger, Budget: runbookBudgetPolicyValue(trigger.Budget), Status: RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, _, err := registry.ActivateDefinition(ctx, scope, deployment.ID, "1.1.0", deployment.Revision, "user", "admin", "increase bounded work")
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := store.GetRunbookActivation(ctx, Scope{Kind: scope.Kind, ID: scope.ID}, activation.ID)
	if err != nil || advanced.DefinitionVersion != "1.1.0" || advanced.Budget == nil || advanced.Budget.MaxTurns != 7 || advanced.Revision != activation.Revision+1 {
		t.Fatalf("advanced Runbook activation = %#v, %v", advanced, err)
	}

	rolledBack, _, err := registry.RollbackDefinition(ctx, scope, deployment.ID, updated.Revision, "user", "admin", "restore prior behavior")
	if err != nil || rolledBack.ActiveVersion != "1.0.0" {
		t.Fatalf("rollback = %#v, %v", rolledBack, err)
	}
	restored, err := store.GetRunbookActivation(ctx, Scope{Kind: scope.Kind, ID: scope.ID}, activation.ID)
	if err != nil || restored.DefinitionVersion != "1.0.0" || restored.Budget == nil || restored.Budget.MaxTurns != 2 || restored.Revision != advanced.Revision+1 {
		t.Fatalf("restored Runbook activation = %#v, %v", restored, err)
	}
}

func TestAgentRunbookActivationSyncRetiresRemovedTriggersAndRejectsUnplacedOnes(t *testing.T) {
	now := time.Now().UTC()
	deployment := &kernelagent.AgentDeployment{ID: "operator", UpdatedAt: now}
	current := &RunbookActivation{
		ID: "daily", Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"},
		ObjectiveID: "objective-one", AssignedAgentID: "operator", DefinitionID: "operator-runbook", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "operate", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
		Status:  RunbookActivationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	withoutTrigger := sqliteAgentDefinition("2")
	withoutTrigger.Runbook = sqliteAgentRunbook("2", 5)
	delete(withoutTrigger.Runbook.Triggers, "hourly")
	retired, err := reconcileAgentRunbookActivations([]*RunbookActivation{current}, withoutTrigger, deployment, now.Add(time.Minute))
	if err != nil || len(retired) != 1 || retired[0].Status != RunbookActivationRetired || retired[0].NextRunAt != nil {
		t.Fatalf("retired activations = %#v, %v", retired, err)
	}
	withUnplacedTrigger := sqliteAgentDefinition("3")
	withUnplacedTrigger.Runbook = sqliteAgentRunbook("3", 7)
	if _, err := reconcileAgentRunbookActivations(nil, withUnplacedTrigger, deployment, now); err == nil {
		t.Fatal("an unplaced Runbook trigger should not be created by a definition-only amendment")
	}
}

func TestAgentRunbookActivationSyncUsesActiveScheduleReplacement(t *testing.T) {
	now := time.Now().UTC()
	deployment := &kernelagent.AgentDeployment{ID: "operator", UpdatedAt: now}
	definition := sqliteAgentDefinition("2")
	definition.Runbook = sqliteAgentRunbook("2", 5)
	retired := &RunbookActivation{
		ID: "hourly-original", Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"},
		ObjectiveID: "objective-one", AssignedAgentID: "operator", DefinitionID: definition.Runbook.ID, DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: definition.Runbook.Triggers["hourly"], Status: RunbookActivationRetired, Revision: 3, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute),
	}
	replacement := cloneRunbookActivation(retired)
	replacement.ID = "hourly-replacement"
	replacement.Status = RunbookActivationActive
	replacement.Revision = 1
	replacement.CreatedAt = now
	replacement.UpdatedAt = now

	synchronized, err := reconcileAgentRunbookActivations([]*RunbookActivation{retired, replacement}, definition, deployment, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(synchronized) != 1 || synchronized[0].ID != replacement.ID || synchronized[0].Status != RunbookActivationActive || synchronized[0].DefinitionVersion != "2" {
		t.Fatalf("synchronized replacement = %#v", synchronized)
	}
}

func TestSQLiteAgentDefinitionsDeploymentsAndActivationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	registry := kernelagent.NewRegistryWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if _, err := registry.RegisterDefinition(context.Background(), sqliteAgentDefinition(version)); err != nil {
			t.Fatal(err)
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	deployment, _, err := registry.CreateDeployment(context.Background(), &kernelagent.AgentDeployment{
		ID: "operator", Scope: scope, DefinitionID: "operator", ActiveVersion: "1.0.0", RolloutStatus: kernelagent.RolloutActive,
		Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := registry.ActivateDefinition(context.Background(), scope, deployment.ID, "1.1.0", deployment.Revision, "user", "admin", "roll forward")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := kernelagent.NewRegistryWithStore(reopened)
	definition, err := restarted.GetDefinition(context.Background(), "operator", "1.0.0")
	if err != nil || definition.Digest == "" {
		t.Fatalf("restored definition = %#v, %v", definition, err)
	}
	restored, err := restarted.GetDeployment(context.Background(), scope, deployment.ID)
	if err != nil || restored.ActiveVersion != "1.1.0" || restored.Revision != updated.Revision {
		t.Fatalf("restored deployment = %#v, %v", restored, err)
	}
	history, err := restarted.ListActivations(context.Background(), scope, deployment.ID)
	if err != nil || len(history) != 2 || history[1].Reason != "roll forward" {
		t.Fatalf("restored activations = %#v, %v", history, err)
	}
	rolledBack, _, err := restarted.RollbackDefinition(context.Background(), scope, deployment.ID, restored.Revision, "user", "admin", "rollback")
	if err != nil || rolledBack.ActiveVersion != "1.0.0" {
		t.Fatalf("restart rollback = %#v, %v", rolledBack, err)
	}
}

func TestSQLiteAgentDefinitionCompilationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compilations.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	registry := kernelagent.NewRegistryWithStore(store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	recorded, err := registry.RecordCompilation(context.Background(), &kernelagent.DefinitionCompilation{
		ID: "candidate-one", Scope: scope, DeploymentID: "agent-39", DefinitionID: "definition-39", CandidateVersion: "legacy-1",
		Source: kernelagent.CompilationSource{Kind: "visual_graph", ID: "library-7", Version: "1", Digest: "sha256:source"},
		Status: kernelagent.CompilationFailed, Diagnostics: []kernelagent.CompilationDiagnostic{{Path: "nodes.telegram", Code: "action.unavailable", Message: "Telegram action is unavailable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	values, err := kernelagent.NewRegistryWithStore(reopened).ListCompilations(context.Background(), scope, "agent-39")
	if err != nil || len(values) != 1 || values[0].ID != recorded.ID || values[0].Diagnostics[0].NodeID != "" {
		t.Fatalf("restarted compilations = %#v, %v", values, err)
	}
}

func TestSQLiteAgentDeploymentRevisionRaceHasOneWinner(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := kernelagent.NewRegistryWithStore(store)
	for _, version := range []string{"1", "2", "3"} {
		if _, err := registry.RegisterDefinition(context.Background(), sqliteAgentDefinition(version)); err != nil {
			t.Fatal(err)
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "race"}
	deployment, _, err := registry.CreateDeployment(context.Background(), &kernelagent.AgentDeployment{
		ID: "operator", Scope: scope, DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		Environment: "prod", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "test", "creator", "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, version := range []string{"2", "3"} {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			_, _, updateErr := registry.ActivateDefinition(context.Background(), scope, deployment.ID, version, deployment.Revision, "test", version, "race")
			errorsSeen <- updateErr
		}(version)
	}
	wg.Wait()
	close(errorsSeen)
	succeeded, conflicted := 0, 0
	for updateErr := range errorsSeen {
		if updateErr == nil {
			succeeded++
		} else if errors.Is(updateErr, kernelagent.ErrRevisionConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected race error: %v", updateErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("race results succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	history, err := registry.ListActivations(context.Background(), scope, deployment.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("race activation history = %#v, %v", history, err)
	}
}

func TestSQLiteAgentDeploymentAndActivationRollbackAtomically(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := capability.ScopeReference{Kind: "tenant", ID: "atomic"}
	now := time.Now().UTC()
	deployment := &kernelagent.AgentDeployment{ID: "agent", Scope: scope, DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Environment: "prod", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	initial := kernelagent.DefinitionActivation{ID: "same-id", Scope: scope, DeploymentID: "agent", DefinitionID: "operator", ToVersion: "1", DeploymentRevision: 1, ActorType: "test", ActorID: "test", CreatedAt: now}
	if err := store.CreateDeployment(context.Background(), deployment, initial); err != nil {
		t.Fatal(err)
	}
	changed := *deployment
	changed.ActiveVersion = "2"
	changed.Revision = 2
	duplicate := initial
	duplicate.DeploymentRevision = 2
	if err := store.UpdateDeployment(context.Background(), &changed, 1, duplicate); err == nil {
		t.Fatal("duplicate activation id should roll back deployment update")
	}
	restored, err := store.GetDeployment(context.Background(), scope, deployment.ID)
	if err != nil || restored.ActiveVersion != "1" || restored.Revision != 1 {
		t.Fatalf("failed transaction changed deployment: %#v, %v", restored, err)
	}
}

func TestSQLiteDefinitionAmendmentSurvivesRestartAndActivatesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amendments.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	registry := kernelagent.NewRegistryWithStore(store)
	base := sqliteAgentDefinition("1")
	base.Runbook = sqliteAgentRunbook("1", 2)
	base.Amendments = kernelagent.AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"runbook", "systemPrompt"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}}
	registered, err := registry.RegisterDefinition(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "amendment"}
	deployment, _, err := registry.CreateDeployment(context.Background(), &kernelagent.AgentDeployment{ID: "operator", Scope: scope, DefinitionID: base.ID, ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Environment: "prod", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID}
	objective, err := NewPortfolioService(store).CreateObjective(context.Background(), CreateObjectiveRequest{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: owner, Title: "Operate", Goal: "Operate safely", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := registered.Runbook.Triggers["hourly"]
	runbookActivation, err := NewRunbookActivationService(store).Create(context.Background(), CreateRunbookActivationRequest{
		ID: "operator-hourly", Scope: Scope{Kind: scope.Kind, ID: scope.ID}, Owner: owner, ObjectiveID: objective.ID,
		AssignedAgentID: deployment.ID, DefinitionID: registered.Runbook.ID, DefinitionVersion: registered.Runbook.Version,
		TriggerID: "hourly", Trigger: trigger, Budget: runbookBudgetPolicyValue(trigger.Budget), Status: RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := *registered
	candidate.Version, candidate.SystemPrompt, candidate.Digest, candidate.CreatedAt = "2", "Operate safely and cite evidence.", "", time.Time{}
	candidate.Runbook = sqliteAgentRunbook("2", 5)
	amendment, err := registry.ProposeAmendment(context.Background(), kernelagent.ProposeAmendmentRequest{Scope: scope, DeploymentID: deployment.ID, Candidate: &candidate, ProposerType: "agent", ProposerID: "operator", Rationale: "Evidence improves reviewability"})
	if err != nil || amendment.Status != kernelagent.AmendmentAwaitingApproval {
		t.Fatalf("propose amendment = %#v, %v", amendment, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := kernelagent.NewRegistryWithStore(reopened)
	restored, err := restarted.GetAmendment(context.Background(), scope, amendment.ID)
	if err != nil || restored.Status != kernelagent.AmendmentAwaitingApproval {
		t.Fatalf("restored amendment = %#v, %v", restored, err)
	}
	listed, err := restarted.ListAmendments(context.Background(), scope, deployment.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != amendment.ID {
		t.Fatalf("listed restarted amendments = %#v, %v", listed, err)
	}
	foreign, err := restarted.ListAmendments(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID)
	if err == nil || !errors.Is(err, kernelagent.ErrDeploymentNotFound) || len(foreign) != 0 {
		t.Fatalf("foreign amendments = %#v, %v", foreign, err)
	}
	approved, err := restarted.ResolveAmendment(context.Background(), kernelagent.ResolveAmendmentRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: restored.Revision, Approved: true, ActorType: "user", ActorID: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	activated, updated, activation, err := restarted.ActivateAmendment(context.Background(), scope, amendment.ID, approved.Revision, "user", "admin", "approved")
	if err != nil || activated.Status != kernelagent.AmendmentActivated || updated.ActiveVersion != "2" || activation.ID != activated.ActivationID {
		t.Fatalf("activate restored amendment = %#v %#v %#v, %v", activated, updated, activation, err)
	}
	if _, err := restarted.GetDefinition(context.Background(), "operator", "2"); err != nil {
		t.Fatalf("activated candidate was not persisted: %v", err)
	}
	advancedRunbook, err := reopened.GetRunbookActivation(context.Background(), Scope{Kind: scope.Kind, ID: scope.ID}, runbookActivation.ID)
	if err != nil || advancedRunbook.DefinitionVersion != "2" || advancedRunbook.Budget == nil || advancedRunbook.Budget.MaxTurns != 5 || advancedRunbook.ObjectiveID != objective.ID {
		t.Fatalf("amendment Runbook activation = %#v, %v", advancedRunbook, err)
	}
}

func sqliteAgentDefinition(version string) *kernelagent.AgentDefinition {
	return &kernelagent.AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Operate safely.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
	}
}

func sqliteAgentRunbook(version string, turns int64) *runbook.Definition {
	return &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "operator-runbook", Version: version, Name: "Operator runbook",
		Entrypoints: map[string]string{"operate": "done"},
		Triggers: map[string]runbook.Trigger{"hourly": {
			Kind: runbook.TriggerSchedule, Entrypoint: "operate", ObjectiveID: "agent:operator:operate",
			Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Budget: &runbook.BudgetAllocation{MaxAttempts: turns, MaxTurns: turns},
		}},
		Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
}
