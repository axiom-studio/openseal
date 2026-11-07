package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type agentBuilderEngineHost struct{}

func (agentBuilderEngineHost) ValidateAgentModel(context.Context, skill.ScopeReference, string) error {
	return nil
}
func (agentBuilderEngineHost) PrepareAgentModel(context.Context, AgentModelRequest) error { return nil }
func (agentBuilderEngineHost) AfterAgentCreated(context.Context, AgentCreated)            {}

func TestAgentBuilderEngineWiresValidatorAndDispatchers(t *testing.T) {
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	scope := Scope{Kind: "tenant", ID: "one"}
	engine, err := New(
		WithActionWorkers(ActionWorkerConfig{Scope: scope}, nil, fallback),
		WithAgentManagementActions(),
		WithAgentBuilderHost(agentBuilderEngineHost{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if engine.agentBuilderHost == nil {
		t.Fatal("the builder host was not kept")
	}
	validators := 0
	for _, validator := range engine.actionValidators {
		if _, ok := validator.(*runtime.AgentBuilderActionValidator); ok {
			validators++
		}
	}
	if validators != 1 {
		t.Fatalf("builder validators = %d", validators)
	}
	if _, ok := engine.actionPoolSpecs[0].dispatcher.(*runtime.AgentBuilderActionDispatcher); !ok {
		t.Fatalf("pool dispatcher = %T", engine.actionPoolSpecs[0].dispatcher)
	}
	if governed := engine.GovernedTools(); governed == nil {
		t.Fatal("the inline executor was not built over the builder dispatcher")
	}
	var _ runtime.AgentProvisioner = engine
	if _, err := New(WithAgentBuilderHost(nil)); err == nil {
		t.Fatal("a nil builder host was accepted")
	}
}

func TestAgentBuilderEngineEnsuresControlConversation(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	reference := skill.ScopeReference{Kind: "tenant", ID: "one"}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "helper-definition", Version: "1.0.0", DisplayName: "Helper", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: skill.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "helper", Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: "active", Environment: "default", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "person", "test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.EnsureAgentControlConversation(ctx, deployment)
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.EnsureAgentControlConversation(ctx, deployment)
	if err != nil || second.ID != first.ID || first.Origin == nil || first.Origin.Kind != runtime.ConversationReferenceAgentControl {
		t.Fatalf("control conversation = %#v then %#v, %v", first, second, err)
	}
}
