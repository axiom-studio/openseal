package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

func TestLatestCanonicalActionFailureRequiresMatchingProtectedReceipt(t *testing.T) {
	failure := skillerror.NewActionError("source_rate_limited", "", nil)
	newCheckpoint := func() map[string]interface{} {
		return checkpointTerminalAction(nil, &ActionCall{ID: "current", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}, nil)
	}
	if latestCanonicalActionFailure(newCheckpoint()) == nil {
		t.Fatal("canonical receipt was not recognized")
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"no history": func(c map[string]interface{}) { delete(c, actionHistoryCheckpointKey) },
		"different latest receipt": func(c map[string]interface{}) {
			c["lastAction"].(map[string]interface{})["actionCallId"] = "older"
		},
		"mismatched text": func(c map[string]interface{}) {
			c["lastAction"].(map[string]interface{})["error"] = "RAW_SECRET HTTP429"
		},
		"unbounded details": func(c map[string]interface{}) {
			c["lastAction"].(map[string]interface{})["errorDetails"].(map[string]interface{})["url"] = "SECRET"
		},
		"retry authority forged": func(c map[string]interface{}) {
			c["lastAction"].(map[string]interface{})["errorDetails"].(map[string]interface{})["retryable"] = "true"
		},
		"successful history": func(c map[string]interface{}) {
			c[actionHistoryCheckpointKey].([]interface{})[0].(map[string]interface{})["status"] = ActionCallStatusSucceeded
		},
	} {
		t.Run(name, func(t *testing.T) {
			checkpoint := newCheckpoint()
			mutate(checkpoint)
			if latestCanonicalActionFailure(checkpoint) != nil {
				t.Fatal("unproven or stale evidence was attributed as the current failure")
			}
		})
	}
}

func TestSourceRateLimitPreservesPauseThenReportsOnceWithoutModelOnResume(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(taskFailureReportingStore)
		accepted, _ := admittedFailureReportingTask(t, store, true)
		scope := accepted.WorkRun.Scope
		catalog, _ := feedbackReadCatalog(t)
		if err := catalog.Bind(t.Context(), &skill.Binding{ID: "reader-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
			SkillID: "reader", SkillVersion: "1.0.0", AllowedActions: []string{"list"}, MaximumRisk: skill.RiskLevelRead,
			Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "reader-secret"}}, Revision: 1}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		claimed := feedbackClaim(t, store, scope, now)
		coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		}))
		proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: scope, RunID: claimed.ID, WorkerID: "agent-worker", DeploymentID: "agent", SkillID: "reader", SkillVersion: "1.0.0", Action: "list", Arguments: map[string]interface{}{"fields": "id"}, IdempotencyKey: "pause-429", Summary: "Read source"})
		if err != nil {
			t.Fatal(err)
		}
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "SECRET"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			current, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: current.ID, ExpectedRevision: current.Revision, Kind: AgentRunCommandPause, Actor: ActivityActor{Type: "user", ID: "requester"}}); err != nil {
				t.Fatal(err)
			}
			return nil, skillerror.NewActionError("source_rate_limited", "SECRET", nil)
		}))
		result, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || result == nil || result.Call.Status != ActionCallStatusFailed || result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued || result.Run.PausedWakeCondition != nil {
			t.Fatalf("rate limit overrode operator pause: %#v %v", result, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = fixture.store.(taskFailureReportingStore)
		}
		if ready, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "premature-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || ready != nil {
			t.Fatalf("paused task ran autonomously: %#v %v", ready, err)
		}
		paused, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: paused.ID, ExpectedRevision: paused.Revision, Kind: AgentRunCommandResume, Actor: ActivityActor{Type: "user", ID: "requester"}}); err != nil {
			t.Fatal(err)
		}
		modelCalls := 0
		pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
			modelCalls++
			t.Fatal("rate limit resolved a model after resume")
			return nil, nil
		}), nil, AgentRunWorkerConfig{Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		resumed := feedbackClaim(t, store, scope, time.Now())
		pool.executeClaim(t.Context(), "agent-worker", resumed)
		terminal, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
		if err != nil || terminal.Status != AgentRunStatusFailed || modelCalls != 0 || dispatches != 1 || latestCanonicalActionFailure(terminal.Checkpoint) == nil {
			t.Fatalf("resume did not settle without a model: %#v %v", terminal, err)
		}
		outbox := terminalReportingContractWorker(t, store, time.Now().Add(time.Second))
		if _, err := outbox.ProcessBatch(t.Context()); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := projectTerminalRunReporting(t.Context(), store, terminal); err != nil {
				t.Fatal(err)
			}
		}
		messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: accepted.Task.ConversationID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		answers := 0
		for _, message := range messages {
			if message.Intent == MessageIntentAnswer {
				answers++
				if answers == 2 && message.Content != "I couldn't finish this request because one or more sources rate-limited requests (HTTP 429). I stopped this attempt. Try again later." {
					t.Fatalf("resume failure lost rate limit: %q", message.Content)
				}
			}
		}
		if answers != 2 {
			t.Fatalf("expected source acknowledgment and one failure, got %d", answers)
		}
		call, err := store.GetActionCall(t.Context(), scope, proposal.Call.ID)
		if err != nil || call.Attempt != 1 || call.Status != ActionCallStatusFailed {
			t.Fatal("resume changed or retried action")
		}
	})
}
