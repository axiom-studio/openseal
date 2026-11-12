package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRetryConversationRunPreservesIdentityAndRejectsDuplicate(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store RunCommandStore = newTestKernelStore(t)
			if backend == "sqlite" {
				db, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
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
			request := agentrun.AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision, Kind: agentrun.AgentRunCommandRetry, Actor: ActivityActor{Type: "user", ID: "user"}}
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
			if result.Run.FailureExplanation != nil {
				t.Fatal("explicit retry retained the failure-only boundary")
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

func failedReplyFixture(t *testing.T, store RunCommandStore) (*agentrun.RunCommandService, *AgentRun) {
	return failedReplyFixtureWithFailure(t, store, &RunFailureExplanation{Kind: "execution", Code: "provider_unavailable", Message: "The model provider was unavailable."})
}

func failedReplyFixtureWithExplanation(t *testing.T, store RunCommandStore, explained bool) (*agentrun.RunCommandService, *AgentRun) {
	return failedReplyFixtureWithFailure(t, store, &RunFailureExplanation{Kind: "execution", Code: "tool_arguments_invalid", Message: "invalid input", Explained: explained})
}

func failedReplyFixtureWithFailure(t *testing.T, store RunCommandStore, failure *RunFailureExplanation) (*agentrun.RunCommandService, *AgentRun) {
	t.Helper()
	ctx := t.Context()
	service := agentrun.NewRunCommandService(store)
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
	failed, _, err := activity.TransitionRun(ctx, scope, running.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusFailed, Error: "provider unavailable", FailureExplanation: failure})
	if err != nil {
		t.Fatal(err)
	}
	return service, failed
}

func TestRetryConversationRunRejectsAnAlreadyExplainedProposalFailure(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store RunCommandStore = newTestKernelStore(t)
			if backend == "sqlite" {
				db, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "explained-retry.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			service, failed := failedReplyFixtureWithExplanation(t, store, true)
			actions, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: failed.Scope, RunID: failed.ID})
			if err != nil || len(actions) != 0 {
				t.Fatalf("proposal failure unexpectedly dispatched actions: %#v %v", actions, err)
			}
			const reason = "This attempt already has a reply. Send a new message to try again."
			eligibility, err := service.ConversationRetryEligibility(t.Context(), failed.Scope, failed.ID)
			if err != nil || eligibility.Available || eligibility.Reason != reason {
				t.Fatalf("answered failure retry eligibility = %#v, %v", eligibility, err)
			}
			_, err = service.RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision})
			if !errors.Is(err, ErrInvalidRunCommand) {
				t.Fatalf("answered proposal failure admitted same-run retry: %v", err)
			}
			current, err := store.GetAgentRun(t.Context(), failed.Scope, failed.ID)
			if err != nil || current.Status != AgentRunStatusFailed || current.Revision != failed.Revision {
				t.Fatalf("rejected retry changed the completed attempt: %#v, %v", current, err)
			}
		})
	}
}

func TestRetryClearsFailureExplanation(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store RunCommandStore = newTestKernelStore(t)
			if backend == "sqlite" {
				db, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "retry-failure.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			service, failed := failedReplyFixtureWithFailure(t, store, conversation.BeginRunFailureExplanation(nil, conversation.RunFailureKindCredits, "gateway_credits_exhausted", "No credits.", "", ""))
			if failed.FailureExplanation == nil || failed.CreditError == nil {
				t.Fatalf("fixture lacks its typed failure: %+v %+v", failed.FailureExplanation, failed.CreditError)
			}
			result, err := service.RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision, Actor: ActivityActor{Type: "user", ID: "user"}})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetAgentRun(t.Context(), failed.Scope, failed.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range []*AgentRun{result.Run, stored} {
				if run.FailureExplanation != nil || run.CreditError != nil || run.Status != AgentRunStatusQueued || conversation.RunExplainingFailure(run) {
					t.Fatalf("retry kept the failure: %+v %+v %s", run.FailureExplanation, run.CreditError, run.Status)
				}
			}
		})
	}
}

type retryGuardStore struct {
	*sqlite.Store
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
			store := &retryGuardStore{Store: newTestKernelStore(t)}
			service, failed := failedReplyFixture(t, store)
			switch kind {
			case "actions":
				store.actions = []*ActionCall{{}}
			case "children":
				store.children = []*AgentRun{{}}
			case "pending":
				store.pending = []*AgentTurn{{}}
			}
			_, err := service.RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision})
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
