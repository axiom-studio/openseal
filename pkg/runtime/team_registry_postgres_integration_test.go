//go:build integration

package runtime

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

func TestPostgresTeamRegistryIsConcurrentRestartSafeAndScoped(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_team_registry_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}

	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	primaryAgents := kernelagent.NewRegistryWithStore(primary)
	agentDefinition, err := primaryAgents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		SkillRequirements: []kernelagent.SkillRequirement{{SkillID: "web"}},
		Authority:         kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"web"}, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := primaryAgents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "operator", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	primaryTeams := kernelteam.NewRegistryWithStore(primary, primaryAgents)
	for _, version := range []string{"1", "2"} {
		if _, err := primaryTeams.RegisterDefinition(ctx, sqliteTeamDefinition(version)); err != nil {
			t.Fatal(err)
		}
	}
	replicaTeams := kernelteam.NewRegistryWithStore(replica, kernelagent.NewRegistryWithStore(replica))
	registries := []*kernelteam.Registry{primaryTeams, replicaTeams}
	deploymentInput := &kernelteam.Deployment{
		ID: "research-team", Scope: scope, DefinitionID: "research-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}
	var created atomic.Int32
	var wait sync.WaitGroup
	for index := range registries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, _, err := registries[index].CreateDeployment(ctx, deploymentInput, "user", "operator", "concurrent"); err == nil {
				created.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if created.Load() != 1 {
		t.Fatalf("successful concurrent creates = %d, want 1", created.Load())
	}
	createdDeployment, err := primaryTeams.GetDeployment(ctx, scope, deploymentInput.ID)
	if err != nil {
		t.Fatal(err)
	}

	var activated atomic.Int32
	for index := range registries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, _, err := registries[index].ActivateDefinition(ctx, scope, deploymentInput.ID, "2", createdDeployment.Revision, "user", "operator", "concurrent activation"); err == nil {
				activated.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if activated.Load() != 1 {
		t.Fatalf("successful concurrent activations = %d, want 1", activated.Load())
	}
	candidate := sqliteTeamDefinition("3")
	candidate.Purpose = "Produce independently reviewed evidence-backed findings"
	amendment, err := primaryTeams.ProposeAmendment(ctx, kernelteam.ProposeAmendmentRequest{
		Scope: scope, DeploymentID: deploymentInput.ID, Candidate: candidate, ProposerType: "user", ProposerID: "operator", Rationale: "Independent review",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := primaryTeams.ResolveAmendment(ctx, kernelteam.ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: true, ActorType: "user", ActorID: "operator", Reason: "reviewed",
	})
	if err != nil {
		t.Fatal(err)
	}
	var amendmentActivated atomic.Int32
	for index := range registries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, _, _, err := registries[index].ActivateAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "operator", "concurrent amendment activation"); err == nil {
				amendmentActivated.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if amendmentActivated.Load() != 1 {
		t.Fatalf("successful concurrent amendment activations = %d, want 1", amendmentActivated.Load())
	}

	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedTeams := kernelteam.NewRegistryWithStore(restarted, kernelagent.NewRegistryWithStore(restarted))
	restored, err := restartedTeams.GetDeployment(ctx, scope, deploymentInput.ID)
	if err != nil || restored.ActiveVersion != "3" || restored.Revision != 3 || restored.Roster[0].AgentDeploymentID != agentDeployment.ID {
		t.Fatalf("restored Team = %#v, err = %v", restored, err)
	}
	activations, err := restartedTeams.ListActivations(ctx, scope, deploymentInput.ID)
	if err != nil || len(activations) != 3 {
		t.Fatalf("restored activations = %#v, err = %v", activations, err)
	}
	restoredAmendment, err := restartedTeams.GetAmendment(ctx, scope, amendment.ID)
	if err != nil || restoredAmendment.Status != kernelteam.AmendmentActivated {
		t.Fatalf("restored amendment = %#v, err = %v", restoredAmendment, err)
	}
	restoredAmendments, err := restartedTeams.ListAmendments(ctx, scope, deploymentInput.ID)
	if err != nil || len(restoredAmendments) != 1 || restoredAmendments[0].ID != amendment.ID {
		t.Fatalf("restored amendment list = %#v, err = %v", restoredAmendments, err)
	}
	if _, err := restartedTeams.GetDeployment(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deploymentInput.ID); err == nil {
		t.Fatal("cross-scope Team deployment should not be visible")
	}
}
