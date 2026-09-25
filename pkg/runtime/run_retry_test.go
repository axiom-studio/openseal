package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRetryConversationRunPreservesIdentityAndRejectsDuplicate(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store RunCommandStore = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			service, failed := failedReplyFixture(t, store)
			eligibility, err := service.ConversationRetryEligibility(t.Context(), failed.Scope, failed.ID)
			if err != nil || !eligibility.Available || eligibility.Revision != failed.Revision {
				t.Fatalf("failed reply eligibility = %#v, %v", eligibility, err)
			}
			request := AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision, Kind: AgentRunCommandRetry, Actor: ActivityActor{Type: "user", ID: "user"}}
			result, err := service.CommandAgentRun(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.ID != failed.ID || result.Run.Status != AgentRunStatusQueued || result.Run.CompletedAt != nil || result.Run.Error != "" || result.Run.Revision != failed.Revision+1 {
				t.Fatalf("invalid retry: %#v", result.Run)
			}
			if result.Run.Checkpoint["saved"] != "context" || result.Run.LastAppliedTurn != failed.LastAppliedTurn || result.Run.BudgetUsage != failed.BudgetUsage || result.Run.Attempt != failed.Attempt {
				t.Fatal("retry lost checkpoint, cursor or usage")
			}
			if result.Event.EventType != "run.retried" {
				t.Fatal("missing retry audit")
			}
			if _, err := service.CommandAgentRun(t.Context(), request); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("duplicate retry: %v", err)
			}
			eligibility, err = service.ConversationRetryEligibility(t.Context(), failed.Scope, failed.ID)
			if err != nil || eligibility.Available || eligibility.Reason != "not_failed" {
				t.Fatalf("queued reply eligibility = %#v, %v", eligibility, err)
			}
			request.Scope.ID = "other"
			if result, err := service.CommandAgentRun(t.Context(), request); err == nil || result != nil {
				t.Fatal("cross-tenant retry accepted")
			}
		})
	}
}

func failedReplyFixture(t *testing.T, store RunCommandStore) (*RunCommandService, *AgentRun) {
	t.Helper()
	ctx := t.Context()
	service := NewRunCommandService(store)
	scope := Scope{Kind: "tenant", ID: "one"}
	created, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Reply", Source: RunSourceChat, Checkpoint: map[string]interface{}{"saved": "context"}})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(ctx, scope, running.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusFailed, Error: "provider unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	return service, failed
}

type retryGuardStore struct {
	*MemoryStore
	actions  []*ActionCall
	children []*AgentRun
	pending  []*AgentTurn
}

func (s *retryGuardStore) ListActionCalls(context.Context, ActionFilter) ([]*ActionCall, error) {
	return s.actions, nil
}
func (s *retryGuardStore) ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error) {
	return s.children, nil
}
func (s *retryGuardStore) ListAgentTurns(context.Context, AgentTurnFilter) ([]*AgentTurn, error) {
	return s.pending, nil
}

func TestRetryConversationRunRejectsUnrecoveredWork(t *testing.T) {
	for _, kind := range []string{"actions", "children", "pending"} {
		t.Run(kind, func(t *testing.T) {
			store := &retryGuardStore{MemoryStore: NewMemoryStore()}
			service, failed := failedReplyFixture(t, store)
			switch kind {
			case "actions":
				store.actions = []*ActionCall{{}}
			case "children":
				store.children = []*AgentRun{{}}
			case "pending":
				store.pending = []*AgentTurn{{}}
			}
			_, err := service.RetryConversationRun(t.Context(), AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision})
			if !errors.Is(err, ErrInvalidRunCommand) {
				t.Fatalf("unsafe recovery accepted: %v", err)
			}
			current, _ := store.GetAgentRun(t.Context(), failed.Scope, failed.ID)
			if current.Status != AgentRunStatusFailed || current.Revision != failed.Revision {
				t.Fatal("rejected recovery mutated run")
			}
		})
	}
}

func TestRetryConversationReplyContinuesWorkerAndPublishesOnce(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "retry-worker"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Retry", IdempotencyKey: "retry-chat"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion, "Please answer once", "retry-question")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", DefinitionID: "agent-definition", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("provider unavailable")
			}
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Recovered answer"}, nil
		})}, nil
	})
	conversationRunner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, conversations), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := conversationRunner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	failed, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: scheduled.Run.ID, WorkerID: "first"}, binding.Runner)
	if err == nil || failed.Run.Status != AgentRunStatusFailed || failed.Run.LastAppliedTurn != 1 {
		t.Fatalf("failure not persisted: %#v %v", failed, err)
	}
	retry, err := NewRunCommandService(store).RetryConversationRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: failed.Run.ID, ExpectedRevision: failed.Run.Revision, Actor: ActivityActor{Type: "user", ID: "user"}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = conversationRunner.ResolveTurnRunner(ctx, retry.Run)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: retry.Run.ID, WorkerID: "second"}, binding.Runner)
	if err != nil || completed.Run.Status != AgentRunStatusCompleted || completed.Run.LastAppliedTurn != 2 || calls != 2 {
		t.Fatalf("retry did not continue: %#v calls=%d err=%v", completed, calls, err)
	}
	// A transport replay must resolve the same persisted answer, not call the
	// model again or append a second user/assistant message.
	_, err = coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: completed.Run.ID, WorkerID: "replayed"}, binding.Runner)
	if err == nil {
		t.Fatal("completed run admitted another worker turn")
	}
	messages, err := conversations.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[0].ID != trigger.ID || messages[1].Content != "Recovered answer" || calls != 2 {
		t.Fatalf("duplicate or missing reply: %#v calls=%d err=%v", messages, calls, err)
	}
}
