package openseal

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type agentBuilderEngineHost struct{}

func (agentBuilderEngineHost) ValidateAgentModel(context.Context, skill.ScopeReference, string) error {
	return nil
}
func (agentBuilderEngineHost) AuthorizeAgentChange(context.Context, skill.ScopeReference, string, string) error {
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
	t.Cleanup(func() { engine.Close() })
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
	t.Cleanup(func() { engine.Close() })
	reference := skill.ScopeReference{Kind: "tenant", ID: "one"}
	home := seedSpace(t, engine, runtime.Scope{Kind: reference.Kind, ID: reference.ID}, "person")
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
	if err != nil || second.ID != first.ID || first.Origin == nil || first.Origin.Kind != runtime.ConversationReferenceAgentControl || first.SpaceID != home.ID {
		t.Fatalf("control conversation = %#v then %#v, %v", first, second, err)
	}
}

func TestAgentBuilderStartConversationOriginSpaceAndReplay(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	requester := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "ana"}
	space, err := engine.CreateSpace(ctx, runtime.CreateSpaceRequest{Scope: scope, Name: "Press", Look: runtime.DefaultSpaceLook, OwnerID: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	request := StartAgentBuilderRequest{
		Scope: scope, Requester: requester, Prompt: "An agent that drafts press releases",
		Parameters: AgentBuilderParameters{Name: " Press Desk ", Model: "openai/gpt-5"}, SpaceID: space.ID, IdempotencyKey: "key-1",
	}
	first, err := engine.StartAgentBuilderConversation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	conversation, message := first.Conversation, first.Message
	if first.Replayed || conversation.Owner != (runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: AgentBuilderDeploymentID}) ||
		conversation.Origin == nil || *conversation.Origin != (runtime.ConversationReference{Kind: runtime.ConversationReferenceAgentBuilder, ID: "key-1"}) ||
		conversation.SpaceID != space.ID || conversation.Title != "Press Desk" {
		t.Fatalf("conversation = %#v", conversation)
	}
	if message.Sender != requester || message.Content != "An agent that drafts press releases" || message.BuilderParameters == nil ||
		*message.BuilderParameters != (AgentBuilderParameters{Kind: runtime.AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5"}) {
		t.Fatalf("message = %#v", message)
	}
	again, err := engine.StartAgentBuilderConversation(ctx, request)
	if err != nil || !again.Replayed || again.Conversation.ID != conversation.ID || again.Message.ID != message.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	changed := request
	changed.Prompt = "Something else"
	if _, err := engine.StartAgentBuilderConversation(ctx, changed); !errors.Is(err, runtime.ErrMessageConflict) {
		t.Fatalf("a changed replay = %v", err)
	}

	personal, err := engine.StartAgentBuilderConversation(ctx, StartAgentBuilderRequest{
		Scope: scope, Requester: requester, Prompt: "A team for launches", Parameters: AgentBuilderParameters{Kind: runtime.AgentBuilderKindTeam}, IdempotencyKey: "key-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	// With no space named, the chat is in the requester's default space.
	home, err := engine.DefaultSpace(ctx, scope, requester.ID)
	if err != nil || personal.Conversation.SpaceID != home.ID || personal.Conversation.Title != "New team" || personal.Message.BuilderParameters.Kind != runtime.AgentBuilderKindTeam {
		t.Fatalf("personal = %#v, %v", personal.Conversation, err)
	}

	for name, bad := range map[string]StartAgentBuilderRequest{
		"sensitive prompt": {Scope: scope, Requester: requester, Prompt: "Use api key: sk-1234567890abcdefghijklmnop", IdempotencyKey: "key-3"},
		"sensitive name":   {Scope: scope, Requester: requester, Prompt: "Hi", Parameters: AgentBuilderParameters{Name: "Password: hunter2-secret"}, IdempotencyKey: "key-4"},
		"agent requester":  {Scope: scope, Requester: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "x"}, Prompt: "Hi", IdempotencyKey: "key-5"},
		"bad kind":         {Scope: scope, Requester: requester, Prompt: "Hi", Parameters: AgentBuilderParameters{Kind: "robot"}, IdempotencyKey: "key-6"},
		"no key":           {Scope: scope, Requester: requester, Prompt: "Hi"},
	} {
		if _, err := engine.StartAgentBuilderConversation(ctx, bad); !errors.Is(err, runtime.ErrInvalidConversation) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	conversations, err := engine.ListConversations(ctx, runtime.ConversationFilter{Scope: scope, OriginKind: runtime.ConversationReferenceAgentBuilder})
	if err != nil || len(conversations) != 2 {
		t.Fatalf("builder conversations = %d, %v", len(conversations), err)
	}
}

func TestAgentBuilderListDraftsKeepsRequesterActiveConversations(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	ana := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "ana"}
	bo := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "bo"}
	// Each requester's drafts start in their own default space.
	homes := map[string]string{ana.ID: seedSpace(t, engine, scope, ana.ID).ID, bo.ID: seedSpace(t, engine, scope, bo.ID).ID}
	start := func(requester runtime.ConversationParticipant, key, name string) *runtime.Conversation {
		t.Helper()
		result, err := engine.StartAgentBuilderConversation(ctx, StartAgentBuilderRequest{
			Scope: scope, Requester: requester, Prompt: "Build " + name, Parameters: AgentBuilderParameters{Name: name}, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Conversation.SpaceID != homes[requester.ID] {
			t.Fatalf("%s's draft is in space %s, want %s", requester.ID, result.Conversation.SpaceID, homes[requester.ID])
		}
		return result.Conversation
	}
	first := start(ana, "a1", "Scout")
	start(bo, "b1", "Other")
	archived := start(ana, "a2", "Gone")
	current, err := engine.GetConversation(ctx, scope, archived.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.UpdateConversation(ctx, runtime.UpdateConversationRequest{
		Scope: scope, ConversationID: archived.ID, ExpectedRevision: current.Revision, Status: ptr(runtime.ConversationStatusArchived),
	}); err != nil {
		t.Fatal(err)
	}
	// A plain chat with the builder is not a builder conversation.
	if _, _, err := engine.CreateConversation(ctx, runtime.CreateConversationRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: AgentBuilderDeploymentID}, Title: "Chat", CreatedBy: ana.ID, IdempotencyKey: "chat",
	}); err != nil {
		t.Fatal(err)
	}
	drafts, err := engine.ListAgentBuilderDrafts(ctx, AgentBuilderDraftFilter{Scope: scope, Requester: ana, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "ana"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].ConversationID != first.ID || drafts[0].Status != runtime.AgentBuilderDraftIdle || drafts[0].Title != "Scout" || drafts[0].Approval != nil {
		t.Fatalf("drafts = %#v", drafts)
	}
}

func ptr[T any](value T) *T { return &value }
