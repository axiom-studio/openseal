package runtime

// Copies of the run worker tests' fixtures for runtime's tests that drive
// the worker pool, loop binding and conversation tasks through the exported
// API: the resolver catalog and loop binding config of
// catalog_turn_resolver_test.go, the timer step producer of
// agent_run_worker_loop_test.go and the continuation Run fixture of
// conversation_task_continuation_lifecycle_test.go. Change them together.

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type bindingCatalog struct {
	deployment            *kernelagent.AgentDeployment
	deployments           []*kernelagent.AgentDeployment
	definition            *kernelagent.AgentDefinition
	definitions           map[string]*kernelagent.AgentDefinition
	teamDeployment        *kernelteam.Deployment
	teamDefinition        *kernelteam.Definition
	activation            *skill.ActivationSnapshot
	activations           map[string]*skill.ActivationSnapshot
	activationDeployments []string
	gotHost               skill.HostCapabilityState
	actors                []skill.RunActor
}

func (c *bindingCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return c.teamDeployment, nil
}

func (c *bindingCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return c.teamDefinition, nil
}

func (c *bindingCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.deployment, nil
}

func (c *bindingCatalog) ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	if c.deployments != nil {
		return c.deployments, nil
	}
	if c.deployment == nil {
		return nil, nil
	}
	return []*kernelagent.AgentDeployment{c.deployment}, nil
}

func (c *bindingCatalog) GetAgentDefinition(_ context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	if c.definitions != nil {
		return c.definitions[id+"@"+version], nil
	}
	return c.definition, nil
}

func (c *bindingCatalog) ActivateSkills(ctx context.Context, _ skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	c.gotHost = host
	actor, _ := skill.RunActorFromContext(ctx)
	c.actors = append(c.actors, actor)
	c.activationDeployments = append(c.activationDeployments, deploymentID)
	if c.activations != nil {
		return c.activations[deploymentID], nil
	}
	return c.activation, nil
}

func (c *bindingCatalog) GetActionCall(context.Context, models.Scope, string) (*models.ActionCall, error) {
	return nil, models.ErrActionNotFound
}

// bindingTestConfig is the minimal LoopBindingConfig of a test: the
// store's sessions, steps and calls and a tool dispatcher over them.
func bindingTestConfig(t testing.TB, store models.KernelStore, host agentrun.StepHost) runworker.LoopBindingConfig {
	t.Helper()
	tools, err := runloop.NewStepToolDispatcher(runloop.StepToolDispatcherConfig{Sessions: store, Turns: store, Actions: store})
	if err != nil {
		t.Fatal(err)
	}
	return runworker.LoopBindingConfig{Host: host, Store: store, Tools: tools}
}

// timerStepProducer sleeps once on a timer, then finishes the Run.
type timerStepProducer struct {
	calls  atomic.Int32
	steps  int // producer calls before the final (each parks on a timer)
	wakeIn time.Duration
}

func (*timerStepProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (p *timerStepProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	call := int(p.calls.Add(1))
	if call <= p.steps {
		due := time.Now().Add(p.wakeIn)
		return agentrun.StepOutput{Park: &agentrun.LoopPark{
			Status: models.AgentRunStatusSleeping, Summary: "Waiting for the next step",
			Wake: &models.WakeCondition{Type: "timer", WakeAt: &due, Reference: "next-step"},
		}}, nil
	}
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Answer: "Work complete", Output: map[string]interface{}{"winner": "agent", "sequence": float64(in.Step.Sequence)}}}, nil
}

func continuationRunFixture(t *testing.T, store *sqlite.Store, id string) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	service := conversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: id}
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := conversationruntest.PostMessage(t, service, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, "Review the release", id+"-message")
	result, _, err := conversationruntest.NewScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule conversation: %#v %v", result, err)
	}
	return result.Run, conversation, message
}

// backdateContinuationRun makes the Run look created at createdAt.
// No store method changes a Run's creation time, so it rewrites the row on
// db, the test's own connection to the store's file.
func backdateContinuationRun(t *testing.T, store *sqlite.Store, db *sql.DB, run *models.AgentRun, createdAt time.Time) *models.AgentRun {
	t.Helper()
	storetest.ExecSQL(t, db, `UPDATE agent_runs SET created_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, createdAt, run.Scope.Kind, run.Scope.ID, run.ID)
	storetest.RewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(stored *models.AgentRun) { stored.CreatedAt = createdAt })
	return getStoredRun(t, store, run.Scope, run.ID)
}
