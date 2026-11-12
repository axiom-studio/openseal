package openseal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// Bounded conversation Runs reserve every model step before it runs (D12):
// the reservation is on the Run, keyed by the step id, when the step reaches
// the host, and a step whose reservation does not fit pauses the Run without a
// model call.

func TestPublicAgentConversationReservesEveryStepAndPreservesDirectWork(t *testing.T) {
	t.Parallel()
	store := kerneltest.NewStore(t)
	scope := Scope{Kind: "tenant", ID: "bounded-agent-conversation"}
	budget := BudgetPolicy{MaxAttempts: 4, MaxTurns: 4, MaxInputTokens: 200000, MaxOutputTokens: 4096}
	var engine *Engine
	reservations := map[string]BudgetReservation{}
	host := &loopStepHost{decide: func(ctx context.Context, req ModelStepRequest) (loopReply, error) {
		run, err := store.GetAgentRun(ctx, scope, req.RunID)
		if err != nil {
			return loopReply{}, err
		}
		reserved, exists := run.BudgetReservations[req.StepID]
		if !exists || reserved.Usage.Turns != 1 || reserved.Usage.InputTokens <= 0 || reserved.Usage.OutputTokens <= 0 ||
			req.MaxOutputTokens <= 0 || req.MaxOutputTokens > budget.MaxOutputTokens {
			return loopReply{}, fmt.Errorf("model step %s reached the host without its durable reservation: %+v (max output %d)", req.StepID, reserved, req.MaxOutputTokens)
		}
		reservations[req.RunID] = reserved
		return loopReply{Text: "Reserved answer"}, nil
	}}
	engine = conversationBudgetEngine(t, store, scope, &budget, loopResolver(&engine, host))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "agent")
	conversation, _, err := engine.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Bounded chat", IdempotencyKey: "bounded-chat"})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}, Intent: MessageIntentUpdate, Content: "The prior color is amber.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "prior-context"})
	if err != nil || prior.Run != nil {
		t.Fatalf("prior context scheduled unexpected work: %v, %v", prior, err)
	}
	question, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: prior.Conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion, Content: "Reply with the prior color.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, IdempotencyKey: "current-request"})
	if err != nil || question.Run == nil || question.Run.Budget == nil {
		t.Fatalf("bounded question scheduling: %+v, %v", question, err)
	}
	direct, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindAgentWork, Owner: conversation.Owner, AssignedAgentID: "agent", Goal: "Direct work marker", Budget: &budget, Source: RunSourceObjective, IdempotencyKey: "direct-work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	chat := waitForRunStatus(t, ctx, engine, scope, question.Run.ID, AgentRunStatusCompleted)
	work := waitForRunStatus(t, ctx, engine, scope, direct.ID, AgentRunStatusCompleted)

	foreground := host.requestsFor(chat.ID)
	background := host.requestsFor(work.ID)
	if len(foreground) != 1 || len(background) != 1 {
		t.Fatalf("model steps: chat %d, direct work %d", len(foreground), len(background))
	}
	if text := loopRequestText(foreground[0]); !strings.Contains(text, "The prior color is amber.") || !strings.Contains(text, "Reply with the prior color.") {
		t.Fatalf("chat step did not see the canonical history and the current message: %s", text)
	}
	if text := loopRequestText(background[0]); !strings.Contains(text, "Direct work marker") || strings.Contains(text, "Reply with the prior color.") {
		t.Fatalf("direct work step changed: %s", text)
	}
	// Each step's reservation is settled into the Run's usage when it is applied.
	for _, run := range []*AgentRun{chat, work} {
		if len(run.BudgetReservations) != 0 || run.BudgetUsage.Turns != 1 || run.BudgetUsage.InputTokens != 100 || run.BudgetUsage.OutputTokens != 20 {
			t.Fatalf("run %s budget after its step: usage=%+v reservations=%+v (held %+v)", run.ID, run.BudgetUsage, run.BudgetReservations, reservations[run.ID])
		}
	}
	messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 3 || messages[2].Content != "Reserved answer" || messages[2].Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}) || messages[2].ResolvesMessageID != question.Message.ID {
		t.Fatalf("durable resolving answer: %+v, %v", messages, err)
	}
}

func TestPublicAgentConversationPausesBeforeAnUnfundedStep(t *testing.T) {
	t.Parallel()
	store := kerneltest.NewStore(t)
	scope := Scope{Kind: "tenant", ID: "unfunded-agent-conversation"}
	budget := BudgetPolicy{MaxAttempts: 4, MaxTurns: 4, MaxInputTokens: 1, MaxOutputTokens: 4096}
	var engine *Engine
	host := &loopStepHost{decide: func(context.Context, ModelStepRequest) (loopReply, error) {
		return loopReply{}, errors.New("an unfunded step must not reach the model")
	}}
	engine = conversationBudgetEngine(t, store, scope, &budget, loopResolver(&engine, host))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "agent")
	conversation, _, err := engine.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Unfunded chat", IdempotencyKey: "unfunded-chat"})
	if err != nil {
		t.Fatal(err)
	}
	question, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion, Content: "This model step cannot fit one input token.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, IdempotencyKey: "unfunded-request"})
	if err != nil || question.Run == nil {
		t.Fatalf("question scheduling: %v, %v", question, err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	paused := waitForRunStatus(t, ctx, engine, scope, question.Run.ID, AgentRunStatusPaused)
	if calls := len(host.requestsFor(paused.ID)); calls != 0 {
		t.Fatalf("unfunded step invoked the model %d times", calls)
	}
	if paused.BudgetAdmission == nil || paused.BudgetAdmission.Dimension != "input_tokens" || len(paused.BudgetReservations) != 0 || paused.BudgetUsage.Turns != 0 {
		t.Fatalf("paused run budget: admission=%+v usage=%+v reservations=%+v", paused.BudgetAdmission, paused.BudgetUsage, paused.BudgetReservations)
	}
	steps, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: paused.ID})
	if err != nil || len(steps) != 1 || steps[0].Status != AgentTurnStatusCanceled || steps[0].FailureCode != runloop.StepFailureBudgetExhausted {
		t.Fatalf("refused step rows = %+v, %v", steps, err)
	}
	messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("budget refusal published a resolving answer: %+v, %v", messages, err)
	}
}

func conversationBudgetEngine(t *testing.T, store *sqlite.Store, scope Scope, budget *BudgetPolicy, resolver TurnRunnerResolver) *Engine {
	t.Helper()
	engine, err := New(WithStore(store), WithConversationCoordinator(
		ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return nil, fmt.Errorf("Agent channel cannot resolve a Team roster")
		}),
		ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
			return ConversationRouteDecision{}, fmt.Errorf("Agent channel cannot route Team messages")
		}), DefaultConversationCoordinatorConfig()),
		WithDynamicConversationRuns(ConversationRunConfig{Scheduler: ConversationRunSchedulerConfig{Budget: budget}, Runner: ConversationRunTurnRunnerConfig{AgentTurns: resolver}, Workers: DynamicAgentRunWorkerConfig{Kind: RunKindConversation, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, Reconciler: ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond}}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })),
		WithAgentRunWorkers(AgentRunWorkerConfig{Scope: scope, Kind: RunKindAgentWork, AssignedAgentID: "agent", Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second}, resolver))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}
