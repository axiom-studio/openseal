package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func skillSetupTaskFixture(t *testing.T) (*SkillBindingActionDispatcher, *MemoryStore, *ConversationTaskResult) {
	t.Helper()
	store := NewMemoryStore()
	source, turn, _, _ := settledWorkerTaskFixture(t, store)
	result, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn))
	if err != nil {
		t.Fatal(err)
	}
	catalog := skillActionCatalog(t, t.Context(), source.Scope, "agent")
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reader", Readiness: skill.DiscoveryReadinessBindable}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, store, result
}

func TestSkillSetupTaskOriginSupportsIndependentWorkAndExactForkLineage(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "fork"}[fork], func(t *testing.T) {
			dispatcher, store, task := skillSetupTaskFixture(t)
			run := task.WorkRun
			if fork {
				child := cloneAgentRun(run)
				child.ID = "setup-child"
				child.ParentRunID = run.ID
				child.Source = RunSourceFork
				if err := store.CreateAgentRun(t.Context(), child); err != nil {
					t.Fatal(err)
				}
				run = child
			}
			if !fork {
				// Independent roots require a persisted request_setup action. The
				// end-to-end wait tests cover proposal and worker admission; seed its
				// running state here to keep this test focused on origin resolution.
				now := time.Now().UTC()
				expires := now.Add(time.Minute)
				call := &ActionCall{
					ID: "task-setup-call", Scope: run.Scope, RunID: run.ID, DeploymentID: "agent",
					SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionRequestSetup,
					Status: ActionCallStatusRunning, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
					Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"},
					Attempt:   1, MaxAttempts: 1, LeaseOwner: "setup-action-worker", LeaseExpiresAt: &expires,
					AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
				}
				next := cloneAgentRun(run)
				next.Revision++
				next.Status, next.WakeCondition = AgentRunStatusWaitingForDependency, &WakeCondition{Type: "action", Reference: call.ID}
				next.UpdatedAt = now
				created, err := store.CreateActionProposal(t.Context(), ActionProposalRecord{
					Call: call, Run: next, ExpectedRunRevision: run.Revision,
					Event: &ActivityEvent{ID: "setup-origin-action", Scope: run.Scope, RunID: run.ID, EventType: "action.started", Summary: "Execute setup request", CreatedAt: now},
				})
				if err != nil {
					t.Fatal(err)
				}
				run = created.Run
			}
			// Copied presentation hints must never redirect the persisted task.
			run = cloneAgentRun(run)
			run.Context["conversationId"], run.Context["triggerMessageId"] = "forged-chat", "forged-message"
			before, _ := dispatcher.catalog.ListBindings(t.Context(), skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent")
			_, err := dispatcher.requestSkillSetup(t.Context(), ActionDispatchInput{Call: &ActionCall{ID: "task-setup-call", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"}}, run, "agent")
			if err != nil {
				t.Fatal(err)
			}
			request, err := store.GetSkillSetupRequest(t.Context(), run.Scope, "skill-setup:task-setup-call")
			if err != nil || request == nil || request.ConversationID != task.Task.ConversationID || request.TriggerMessageID != task.Task.SourceMessageID || request.RunID != run.ID || request.ActionCallID != "task-setup-call" || request.DeploymentID != run.AssignedAgentID {
				t.Fatalf("wrong causal setup: %#v %v", request, err)
			}
			listed, err := dispatcher.listSkillSetupRequests(t.Context(), run, "agent")
			if err != nil || len(listed["requests"].([]interface{})) != 1 {
				t.Fatalf("task could not list its origin setup: %#v %v", listed, err)
			}
			after, _ := dispatcher.catalog.ListBindings(t.Context(), skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("origin setup expanded work authority")
			}
		})
	}
}

func TestSkillSetupTaskOriginRejectsUnprovedContextAndChangedCanonicalSource(t *testing.T) {
	for _, scenario := range []string{"non-task", "foreign-scope", "foreign-agent", "source-changed", "actor-changed", "thread-changed", "turn-changed", "archived", "fork-owner", "missing-ledger"} {
		t.Run(scenario, func(t *testing.T) {
			dispatcher, store, task := skillSetupTaskFixture(t)
			run := cloneAgentRun(task.WorkRun)
			deploymentID := "agent"
			store.mu.Lock()
			switch scenario {
			case "non-task":
				canonical := store.agentRuns[portfolioKey(run.Scope, run.ID)]
				delete(canonical.Context, ConversationTaskContextKey)
				canonical.ConcurrencyKey = "ordinary-work"
				delete(store.conversationTaskWorkRuns, portfolioKey(run.Scope, run.ID))
			case "foreign-scope":
				run.Scope.ID = "other"
			case "foreign-agent":
				deploymentID = "other"
			case "source-changed":
				store.agentRuns[portfolioKey(run.Scope, task.Task.SourceRunID)].Context["triggerMessageId"] = "other"
			case "actor-changed":
				store.conversationTasks[portfolioKey(run.Scope, task.Task.ID)].AuthenticatedActor.ID = "other"
			case "thread-changed":
				store.conversationTasks[portfolioKey(run.Scope, task.Task.ID)].ThreadRootID = "other"
			case "turn-changed":
				store.turns[portfolioKey(run.Scope, task.Task.SourceRunID)][task.Task.SourceTurnID].RequestedTask.Goal = "other"
			case "archived":
				store.conversations[conversationStoreKey(run.Scope, task.Task.ConversationID)].Status = ConversationStatusArchived
			case "fork-owner":
				store.agentRuns[portfolioKey(run.Scope, run.ID)].Owner.ID = "other"
			case "missing-ledger":
				delete(store.conversationTasks, portfolioKey(run.Scope, task.Task.ID))
			}
			store.mu.Unlock()
			input := ActionDispatchInput{Call: &ActionCall{ID: "rejected", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect"}}
			if _, err := dispatcher.requestSkillSetup(t.Context(), input, run, deploymentID); err == nil {
				t.Fatal("unproved origin created setup")
			}
			if _, err := dispatcher.listSkillSetupRequests(t.Context(), run, deploymentID); err == nil {
				t.Fatal("unproved origin listed setup")
			}
			store.mu.RLock()
			count := len(store.skillSetupRequests)
			store.mu.RUnlock()
			if count != 0 {
				t.Fatal("rejected origin left durable setup")
			}
		})
	}
}
