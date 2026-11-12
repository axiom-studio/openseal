package actiontest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// NewPool is an action worker pool for scope whose credentials always
// resolve to a test token.
func NewPool(t *testing.T, store models.KernelStore, catalog *skill.Catalog, scope models.Scope, interval time.Duration, concurrency int, dispatcher agentrun.ActionDispatcher) *action.ActionWorkerPool {
	t.Helper()
	pool, err := action.NewActionWorkerPool(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "test-secret"}, nil
	}), dispatcher, nil, action.ActionWorkerConfig{Scope: scope, Concurrency: concurrency, PollInterval: interval, LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// CreatePoolWork claims a Run of release-agent with a running model step and
// proposes one nested deploy call for the pool to execute.
func CreatePoolWork(t *testing.T, store models.KernelStore, catalog *skill.Catalog, scope models.Scope) *models.ActionProposalResult {
	t.Helper()
	now := time.Now().Add(-2 * time.Second)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	// Action workers execute the nested calls of a running step; loop calls
	// run inline in their Run's loop.
	stepExpires := now.Add(time.Hour)
	step, err := store.CreateAgentTurn(t.Context(), &models.AgentTurn{
		ID: "step-" + run.ID, Scope: scope, RunID: claimed.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel,
		LeaseOwner: "agent-worker", LeaseExpiresAt: &stepExpires, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
	}), nil, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	proposal, err := coordinator.Propose(t.Context(), action.ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, TurnID: step.ID, WorkerID: "agent-worker", DeploymentID: "release-agent", Origin: models.ActionOriginVM, OriginRef: "command-1",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "deploy-" + run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}
