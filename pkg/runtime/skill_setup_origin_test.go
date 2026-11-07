package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func skillSetupTaskFixture(t *testing.T) (*SkillBindingActionDispatcher, *MemoryStore, *ConversationTaskResult) {
	t.Helper()
	store := NewMemoryStore()
	source, turn, _, _ := settledWorkerTaskFixture(t, store)
	result, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(t, store, source, turn))
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
				// Independent roots require a persisted request_setup loop call
				// running under the Run's own lease. The end-to-end wait tests cover
				// proposal and inline execution; seed its running state here to keep
				// this test focused on origin resolution.
				now := time.Now().UTC()
				claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindAgentWork, WorkerID: "setup-worker",
					AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4})
				if err != nil || claimed == nil || claimed.ID != run.ID {
					t.Fatalf("claim task work: %#v %v", claimed, err)
				}
				step, err := NewAgentTurnService(store, store).BeginTurn(t.Context(), BeginAgentTurnRequest{Scope: run.Scope, RunID: run.ID, SessionID: claimed.SessionID, Producer: StepProducerModel,
					WorkerID: "setup-worker", LeaseDuration: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				claimed, err = store.GetAgentRun(t.Context(), run.Scope, run.ID)
				if err != nil || claimed == nil {
					t.Fatalf("reload task work: %#v %v", claimed, err)
				}
				now = time.Now().UTC()
				expires := now.Add(time.Minute)
				call := &ActionCall{
					ID: "task-setup-call", Scope: run.Scope, RunID: run.ID, TurnID: step.ID, DeploymentID: "agent",
					IdempotencyKey: loopIdempotencyKey(step.ID, "call-setup", nil),
					SkillID:        SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionRequestSetup,
					Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
					Status: ActionCallStatusRunning, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
					Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"},
					Attempt:   1, MaxAttempts: 1, LeaseOwner: "setup-worker", LeaseExpiresAt: &expires,
					AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
				}
				call.InvocationDigest = ComputeActionInvocationDigest(call)
				if _, err := store.CreateActionProposal(t.Context(), ActionProposalRecord{
					Call: call, ExpectedRunRevision: claimed.Revision, Lease: &AgentRunLeaseGuard{WorkerID: "setup-worker", Now: now},
					Event: &ActivityEvent{ID: "setup-origin-action", Scope: run.Scope, RunID: run.ID, EventType: "action.started", Summary: "Execute setup request", CreatedAt: now},
				}); err != nil {
					t.Fatal(err)
				}
				run = claimed
			}
			// Copied presentation hints must never redirect the persisted task.
			run = cloneAgentRun(run)
			run.Context["conversationId"], run.Context["triggerMessageId"] = "forged-chat", "forged-message"
			before, _ := dispatcher.catalog.ListBindings(t.Context(), skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent")
			_, err := dispatcher.requestSkillSetup(t.Context(), ActionDispatchInput{Call: &ActionCall{ID: "task-setup-call", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
			if err != nil {
				t.Fatal(err)
			}
			request, err := store.GetSkillSetupRequest(t.Context(), run.Scope, "skill-setup:task-setup-call")
			if err != nil || request == nil || request.ConversationID != task.Task.ConversationID || request.TriggerMessageID != task.Task.SourceMessageID || request.RunID != run.ID || request.ActionCallID != "task-setup-call" || request.DeploymentID != run.AssignedAgentID {
				t.Fatalf("wrong causal setup: %#v %v", request, err)
			}
			listed, err := dispatcher.listSkillSetupRequests(t.Context(), run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
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
				store.turns[portfolioKey(run.Scope, task.Task.SourceRunID)][task.Task.SourceTurnID].Status = AgentTurnStatusFailed
			case "archived":
				store.conversations[conversationStoreKey(run.Scope, task.Task.ConversationID)].Status = ConversationStatusArchived
			case "fork-owner":
				store.agentRuns[portfolioKey(run.Scope, run.ID)].Owner.ID = "other"
			case "missing-ledger":
				delete(store.conversationTasks, portfolioKey(run.Scope, task.Task.ID))
			}
			store.mu.Unlock()
			input := ActionDispatchInput{Call: &ActionCall{ID: "rejected", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect"}}
			if _, err := dispatcher.requestSkillSetup(t.Context(), input, run, deploymentID, ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}); err == nil {
				t.Fatal("unproved origin created setup")
			}
			if _, err := dispatcher.listSkillSetupRequests(t.Context(), run, deploymentID, ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}); err == nil {
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

// skillSetupOriginAllowed reports whether a task work Run's in-chat setup
// origin resolves to the task's source conversation and message.
func skillSetupOriginAllowed(t *testing.T, store *MemoryStore, task *ConversationTaskResult) bool {
	t.Helper()
	run := task.WorkRun
	conversationID, messageID, err := skillSetupConversationOrigin(t.Context(), store, run, run.AssignedAgentID, run.Owner)
	if err != nil {
		return false
	}
	if conversationID != task.Task.ConversationID || messageID != task.Task.SourceMessageID {
		t.Fatalf("origin = %q/%q, want %q/%q", conversationID, messageID, task.Task.ConversationID, task.Task.SourceMessageID)
	}
	return true
}

func TestSkillSetupOriginAcceptsTaskFromStartTaskEntry(t *testing.T) {
	_, store, task := skillSetupTaskFixture(t)
	if task.Task.Mode != ConversationTaskModeIndependent || task.Task.SourceToolEntryID == "" || task.Task.TaskKey != startTaskKey(task.Task.SourceToolEntryID) {
		t.Fatalf("fixture task is not admitted from a start_task entry: %#v", task.Task)
	}
	entry, err := store.GetSessionEntry(t.Context(), task.Task.Scope, task.Task.SourceToolEntryID)
	if err != nil || entry.State != ToolEntryCompleted || entry.StepID != task.Task.SourceTurnID {
		t.Fatalf("source entry: %#v %v", entry, err)
	}
	if !skillSetupOriginAllowed(t, store, task) {
		t.Fatal("task started by its step's start_task entry was denied")
	}
}

func TestSkillSetupOriginDeniesTamperedGoal(t *testing.T) {
	cases := map[string]func(entry *SessionEntry, task *ConversationTask){
		"entry-goal": func(entry *SessionEntry, task *ConversationTask) {
			entry.Tool.Arguments, _ = json.Marshal(map[string]string{"goal": "Something else", "acknowledgment": task.Acknowledgment})
		},
		"task-goal":           func(_ *SessionEntry, task *ConversationTask) { task.Goal = "Something else" },
		"task-acknowledgment": func(_ *SessionEntry, task *ConversationTask) { task.Acknowledgment = "Something else" },
		"task-key":            func(_ *SessionEntry, task *ConversationTask) { task.TaskKey = "task-other" },
		"entry-tool":          func(entry *SessionEntry, _ *ConversationTask) { entry.Tool.Name = string(ControlToolFork) },
		"entry-open":          func(entry *SessionEntry, _ *ConversationTask) { entry.State = ToolEntryPending },
		"entry-run":           func(entry *SessionEntry, _ *ConversationTask) { entry.RunID = "other-run" },
		"entry-step":          func(entry *SessionEntry, _ *ConversationTask) { entry.Step++ },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			_, store, task := skillSetupTaskFixture(t)
			store.mu.Lock()
			entry := store.memorySessionEntryLocked(task.Task.Scope, task.Task.SourceToolEntryID)
			ledger := store.conversationTasks[portfolioKey(task.Task.Scope, task.Task.ID)]
			if entry == nil || ledger == nil {
				store.mu.Unlock()
				t.Fatal("fixture has no source entry or ledger task")
			}
			tamper(entry, ledger)
			store.mu.Unlock()
			if skillSetupOriginAllowed(t, store, task) {
				t.Fatal("tampered task start proved an origin")
			}
		})
	}
}

func TestSkillSetupOriginDeniesTaskWithoutEntry(t *testing.T) {
	for _, scenario := range []string{"no-entry-id", "entry-missing"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, task := skillSetupTaskFixture(t)
			store.mu.Lock()
			switch scenario {
			case "no-entry-id":
				store.conversationTasks[portfolioKey(task.Task.Scope, task.Task.ID)].SourceToolEntryID = ""
			case "entry-missing":
				delete(store.agentSessionEntryIndex, portfolioKey(task.Task.Scope, task.Task.SourceToolEntryID))
			}
			store.mu.Unlock()
			if skillSetupOriginAllowed(t, store, task) {
				t.Fatal("task without its start_task entry proved an origin")
			}
		})
	}
}
