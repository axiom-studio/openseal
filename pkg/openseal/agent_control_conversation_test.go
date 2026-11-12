package openseal

import (
	"context"
	"sync/atomic"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/runtimetest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// conversationWriteCountingStore counts conversation creation attempts.
type conversationWriteCountingStore struct {
	*sqlite.Store
	creates atomic.Int64
}

func (s *conversationWriteCountingStore) CreateConversation(ctx context.Context, conversation *models.Conversation, key string) (*models.Conversation, bool, error) {
	s.creates.Add(1)
	return s.Store.CreateConversation(ctx, conversation, key)
}

func controlConversationFixture(t *testing.T) (*Engine, *conversationWriteCountingStore, SkillScope, *AgentDefinition) {
	t.Helper()
	store := &conversationWriteCountingStore{Store: runtimetest.NewKernelStore(t)}
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	definition, err := engine.RegisterAgentDefinition(context.Background(), &AgentDefinition{
		ID: "control-fixture", Version: "1.0.0", DisplayName: "Control", Purpose: "Exercise control conversations",
		SystemPrompt: "Be precise.", Authority: AgentAuthorityPolicy{MaximumRisk: SkillRiskWrite, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's writes have no person behind them; the empty owner's
	// default space is where their control conversations go.
	scope := SkillScope{Kind: "tenant", ID: "7"}
	seedSpace(t, engine, models.Scope{Kind: scope.Kind, ID: scope.ID}, "")
	return engine, store, scope, definition
}

func controlConversation(t *testing.T, engine *Engine, scope SkillScope, deploymentID string) *models.Conversation {
	t.Helper()
	conversation, err := engine.store.(models.ConversationStore).FindConversationByIdempotencyKey(context.Background(),
		models.Scope{Kind: scope.Kind, ID: scope.ID}, "agent-control:"+deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func TestAgentDeploymentReadsAreSideEffectFree(t *testing.T) {
	engine, store, scope, definition := controlConversationFixture(t)
	ctx := context.Background()
	deployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "agent-1", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1, MaxQueuedRuns: 5},
	}, "test", "fixture", "create")
	if err != nil {
		t.Fatal(err)
	}
	if conversation := controlConversation(t, engine, scope, deployment.ID); conversation == nil || conversation.Origin == nil ||
		conversation.Origin.Kind != ConversationReferenceAgentControl {
		t.Fatalf("creating a deployment must create its control conversation: %+v", conversation)
	}
	before := store.creates.Load()
	for i := 0; i < 5; i++ {
		if _, err := engine.ListAgentDeployments(ctx, AgentDeploymentFilter{Scope: scope}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.GetAgentDeployment(ctx, scope, deployment.ID); err != nil {
			t.Fatal(err)
		}
	}
	if writes := store.creates.Load() - before; writes != 0 {
		t.Fatalf("deployment reads attempted %d conversation writes", writes)
	}
}

func TestAgentDeploymentWritesEnsureControlConversation(t *testing.T) {
	engine, store, scope, definition := controlConversationFixture(t)
	ctx := context.Background()
	// A deployment written below the Engine (as workforce apply does) has no
	// control conversation, and reading it must not create one.
	created, _, err := engine.agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "agent-2", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1, MaxQueuedRuns: 5},
	}, "test", "fixture", "create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ListAgentDeployments(ctx, AgentDeploymentFilter{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if controlConversation(t, engine, scope, created.ID) != nil || store.creates.Load() != 0 {
		t.Fatal("a read must not create the control conversation")
	}
	update := *created
	update.Environment = "staging"
	updated, _, err := engine.UpdateAgentDeployment(ctx, &update, created.Revision, "test", "fixture", "update")
	if err != nil {
		t.Fatal(err)
	}
	if controlConversation(t, engine, scope, updated.ID) == nil {
		t.Fatal("updating a deployment must ensure its control conversation")
	}
	// Repeated writes stay idempotent.
	update = *updated
	update.Environment = "production"
	if _, _, err := engine.UpdateAgentDeployment(ctx, &update, updated.Revision, "test", "fixture", "update again"); err != nil {
		t.Fatal(err)
	}
	conversations, err := engine.store.(models.ConversationStore).ListConversations(ctx, models.ConversationFilter{Scope: models.Scope{Kind: scope.Kind, ID: scope.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 {
		t.Fatalf("expected exactly one control conversation, got %d", len(conversations))
	}
}
