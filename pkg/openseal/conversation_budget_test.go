package openseal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type conversationBudgetHostFunc func(context.Context, HostedTurnRequest) (*HostedTurnResponse, error)

func (f conversationBudgetHostFunc) ExecuteHostedTurn(ctx context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return f(ctx, request)
}

func TestPublicAgentConversationAdmitsCanonicalHostedBudgetAndPreservesDirectWork(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "bounded-agent-conversation"}
	budget := BudgetPolicy{MaxAttempts: 4, MaxTurns: 4, MaxInputTokens: 100000, MaxOutputTokens: 4096}
	var mu sync.Mutex
	requests := map[string]HostedTurnRequest{}
	host := conversationBudgetHostFunc(func(ctx context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
		run, err := store.GetAgentRun(ctx, scope, request.RunID)
		if err != nil {
			return nil, err
		}
		reserved, exists := run.BudgetReservations[request.TurnID]
		estimated, err := runtime.EstimateHostedTurnMaximumInputTokens(request)
		if err != nil {
			return nil, err
		}
		if request.Budget == nil || !exists || request.Budget.TurnReservation != reserved.Usage || reserved.Usage.OutputTokens != budget.MaxOutputTokens || reserved.Usage.InputTokens < estimated {
			return nil, errors.New("hosted model dispatch did not have its canonical durable token reservation")
		}
		mu.Lock()
		requests[request.RunID] = request
		mu.Unlock()
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "test", NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Reserved answer", RunOutput: map[string]interface{}{"summary": "Reserved answer"}, Usage: TurnUsage{InputTokens: 100, OutputTokens: 20}}, nil
	})
	resolver := conversationBudgetResolver(t, host)
	engine := conversationBudgetEngine(t, store, scope, &budget, resolver)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conversation, _, err := engine.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Bounded chat", IdempotencyKey: "bounded-chat"})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}, Intent: MessageIntentUpdate, Content: "The prior color is amber.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "prior-context"})
	if err != nil || prior.Run != nil {
		t.Fatalf("prior context scheduled unexpected work: %v, %v", prior, err)
	}
	question, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: prior.Conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion, Content: "Reply with the prior color.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, IdempotencyKey: "current-request"})
	if err != nil || question.Run == nil {
		t.Fatalf("canonical question scheduling: %v, %v", question, err)
	}
	direct, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindAgentWork, Owner: conversation.Owner, AssignedAgentID: "agent", Goal: "Direct work marker", Budget: &budget, Source: RunSourceObjective, IdempotencyKey: "direct-work"})
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	conversationBudgetWait(t, ctx, engine, scope, question.Run.ID, AgentRunStatusCompleted)
	conversationBudgetWait(t, ctx, engine, scope, direct.ID, AgentRunStatusCompleted)
	mu.Lock()
	foreground, foregroundExists := requests[question.Run.ID]
	work, workExists := requests[direct.ID]
	mu.Unlock()
	if !foregroundExists || !strings.Contains(foreground.Goal, `"currentMessage"`) || !strings.Contains(foreground.Goal, "The prior color is amber.") || !strings.Contains(foreground.Goal, "Reply with the prior color.") || !workExists || work.Goal != direct.Goal {
		t.Fatalf("foreground canonical history or direct work changed: foreground=%+v direct=%+v", foreground, work)
	}
	messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 3 || messages[2].Content != "Reserved answer" || messages[2].Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}) || messages[2].ResolvesMessageID != question.Message.ID {
		t.Fatalf("durable resolving answer: %+v, %v", messages, err)
	}
}

func TestPublicAgentConversationPausesBeforeUnfundedCanonicalInput(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "unfunded-agent-conversation"}
	budget := BudgetPolicy{MaxAttempts: 4, MaxTurns: 4, MaxInputTokens: 1, MaxOutputTokens: 4096}
	var mu sync.Mutex
	calls := 0
	host := conversationBudgetHostFunc(func(context.Context, HostedTurnRequest) (*HostedTurnResponse, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, errors.New("unfunded model must not be dispatched")
	})
	engine := conversationBudgetEngine(t, store, scope, &budget, conversationBudgetResolver(t, host))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conversation, _, err := engine.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Unfunded chat", IdempotencyKey: "unfunded-chat"})
	if err != nil {
		t.Fatal(err)
	}
	question, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion, Content: "This canonical model input cannot fit one token.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, IdempotencyKey: "unfunded-request"})
	if err != nil || question.Run == nil {
		t.Fatalf("canonical question scheduling: %v, %v", question, err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	conversationBudgetWait(t, ctx, engine, scope, question.Run.ID, AgentRunStatusPaused)
	mu.Lock()
	observedCalls := calls
	mu.Unlock()
	if observedCalls != 0 {
		t.Fatalf("unfunded input invoked the model %d times", observedCalls)
	}
	messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("admission failure published a resolving answer: %+v, %v", messages, err)
	}
}

func conversationBudgetResolver(t *testing.T, host TurnHost) TurnRunnerResolver {
	t.Helper()
	return TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
		if err != nil {
			return nil, err
		}
		return &TurnRunnerBinding{Runner: runner, DeploymentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", BudgetReservation: BudgetUsage{Turns: 1}}, nil
	})
}

func conversationBudgetEngine(t *testing.T, store *runtime.MemoryStore, scope Scope, budget *BudgetPolicy, resolver TurnRunnerResolver) *Engine {
	t.Helper()
	engine, err := New(WithStore(store), WithConversationCoordinator(
		ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return nil, fmt.Errorf("Agent channel cannot resolve a Team roster")
		}),
		ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
			return ParticipationProposal{}, fmt.Errorf("Agent channel cannot arbitrate Team participation")
		}), DefaultConversationCoordinatorConfig()),
		WithDynamicConversationRuns(ConversationRunConfig{Scheduler: ConversationRunSchedulerConfig{Budget: budget}, Runner: ConversationRunTurnRunnerConfig{AgentTurns: resolver}, Workers: DynamicAgentRunWorkerConfig{Kind: RunKindConversation, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, Reconciler: ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond}}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })),
		WithAgentRunWorkers(AgentRunWorkerConfig{Scope: scope, Kind: RunKindAgentWork, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, resolver))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func conversationBudgetWait(t *testing.T, ctx context.Context, engine *Engine, scope Scope, id string, expected AgentRunStatus) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := engine.GetAgentRun(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == expected {
			return
		}
		if run.Status == AgentRunStatusCompleted || run.Status == AgentRunStatusFailed || run.Status == AgentRunStatusCanceled || run.Status == AgentRunStatusPaused {
			t.Fatalf("run %s status=%s expected=%s error=%s", id, run.Status, expected, run.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run %s did not reach %s: %v", id, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}
