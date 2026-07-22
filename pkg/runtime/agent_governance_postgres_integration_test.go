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
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

func TestPostgresAgentGovernanceIsConcurrentRestartSafeAndScoped(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_agent_governance_" + uuid.NewString()[:8]
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

	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	primaryRegistry := kernelagent.NewRegistryWithStore(primary)
	base, err := primaryRegistry.RegisterDefinition(ctx, postgresGovernedAgentDefinition("1", "Verify attributable evidence before acting."))
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := primaryRegistry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "operator", Scope: scope, DefinitionID: base.ID, ActiveVersion: base.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "initial activation")
	if err != nil {
		t.Fatal(err)
	}
	candidate := postgresGovernedAgentDefinition("2", "Verify attributable evidence, state uncertainty, and ask before widening impact.")
	amendment, err := primaryRegistry.ProposeAmendment(ctx, kernelagent.ProposeAmendmentRequest{
		Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "user", ProposerID: "admin", Rationale: "Make uncertainty explicit", ExpectedDeploymentRevision: deployment.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	evaluated, err := primaryRegistry.SubmitAmendmentEvaluation(ctx, kernelagent.SubmitAmendmentEvaluationRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision,
		Evaluations: []kernelagent.AmendmentEvaluation{{CriterionID: "safety", Passed: true, Summary: "Authority remains unchanged"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := primaryRegistry.ResolveAmendment(ctx, kernelagent.ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true, ActorType: "user", ActorID: "admin", Reason: "Safety evaluation passed",
	})
	if err != nil {
		t.Fatal(err)
	}

	registries := []*kernelagent.Registry{primaryRegistry, kernelagent.NewRegistryWithStore(replica)}
	var activated atomic.Int32
	var wait sync.WaitGroup
	for index := range registries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, _, _, activateErr := registries[index].ActivateAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "admin", "concurrent activation"); activateErr == nil {
				activated.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if activated.Load() != 1 {
		t.Fatalf("successful concurrent Agent amendment activations = %d, want 1", activated.Load())
	}

	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedRegistry := kernelagent.NewRegistryWithStore(restarted)
	restored, err := restartedRegistry.GetDeployment(ctx, scope, deployment.ID)
	if err != nil || restored.ActiveVersion != "2" || restored.Revision != 2 {
		t.Fatalf("restored Agent deployment = %#v, err = %v", restored, err)
	}
	history, err := restartedRegistry.ListAmendments(ctx, scope, deployment.ID)
	if err != nil || len(history) != 1 || history[0].ID != amendment.ID || history[0].Status != kernelagent.AmendmentActivated {
		t.Fatalf("restored Agent governance history = %#v, err = %v", history, err)
	}
	if _, err := restartedRegistry.ListAmendments(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID); err == nil {
		t.Fatal("cross-scope Agent governance history should not be visible")
	}
}

func postgresGovernedAgentDefinition(version, systemPrompt string) *kernelagent.AgentDefinition {
	return &kernelagent.AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: systemPrompt, Personality: "Calm and precise.",
		Authority:   kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Evaluations: []workforce.EvaluationCriterion{{ID: "safety", Description: "Authority remains bounded", Required: true}},
		Amendments:  workforce.AmendmentPolicy{AllowedFields: []string{"systemPrompt", "personality"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}},
	}
}
