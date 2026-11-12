package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// skillSetupTaskFixture also returns the test's own connection to the
// store's file, for the rows the tests tamper with.
func skillSetupTaskFixture(t *testing.T) (*SkillBindingActionDispatcher, *sqlite.SQLiteStore, *sql.DB, *ConversationTaskResult) {
	t.Helper()
	store, db := newTestSQLiteStore(t)
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
	return dispatcher, store, db, result
}

func TestSkillSetupTaskOriginSupportsIndependentWorkAndExactForkLineage(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "fork"}[fork], func(t *testing.T) {
			dispatcher, store, _, task := skillSetupTaskFixture(t)
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
			_, err := dispatcher.requestSkillSetup(t.Context(), ActionDispatchInput{Call: &ActionCall{ID: "task-setup-call", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
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
			dispatcher, _, db, task := skillSetupTaskFixture(t)
			run := cloneAgentRun(task.WorkRun)
			deploymentID := "agent"
			switch scenario {
			case "non-task":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(canonical *AgentRun) {
					delete(canonical.Context, ConversationTaskContextKey)
					canonical.ConcurrencyKey = "ordinary-work"
				})
				execSQL(t, db, `UPDATE conversation_tasks SET work_run_id = '' WHERE scope_kind = ? AND scope_id = ? AND id = ?`, run.Scope.Kind, run.Scope.ID, task.Task.ID)
			case "foreign-scope":
				run.Scope.ID = "other"
			case "foreign-agent":
				deploymentID = "other"
			case "source-changed":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, task.Task.SourceRunID, func(source *AgentRun) {
					source.Context["triggerMessageId"] = "other"
				})
			case "actor-changed":
				rewriteRowPayload(t, db, "conversation_tasks", run.Scope, task.Task.ID, func(ledger *ConversationTask) {
					ledger.AuthenticatedActor.ID = "other"
				})
			case "thread-changed":
				rewriteRowPayload(t, db, "conversation_tasks", run.Scope, task.Task.ID, func(ledger *ConversationTask) {
					ledger.ThreadRootID = "other"
				})
			case "turn-changed":
				rewriteRowPayload(t, db, "agent_turns", run.Scope, task.Task.SourceTurnID, func(turn *AgentTurn) {
					turn.Status = AgentTurnStatusFailed
				})
				execSQL(t, db, `UPDATE agent_turns SET status = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, AgentTurnStatusFailed, run.Scope.Kind, run.Scope.ID, task.Task.SourceTurnID)
			case "archived":
				rewriteRowPayload(t, db, "conversations", run.Scope, task.Task.ConversationID, func(conversation *Conversation) {
					conversation.Status = ConversationStatusArchived
				})
				execSQL(t, db, `UPDATE conversations SET status = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, ConversationStatusArchived, run.Scope.Kind, run.Scope.ID, task.Task.ConversationID)
			case "fork-owner":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(canonical *AgentRun) {
					canonical.Owner.ID = "other"
				})
			case "missing-ledger":
				execSQL(t, db, `DELETE FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ? AND id = ?`, run.Scope.Kind, run.Scope.ID, task.Task.ID)
			}
			input := ActionDispatchInput{Call: &ActionCall{ID: "rejected", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect"}}
			if _, err := dispatcher.requestSkillSetup(t.Context(), input, run, deploymentID, ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}); err == nil {
				t.Fatal("unproved origin created setup")
			}
			if _, err := dispatcher.listSkillSetupRequests(t.Context(), run, deploymentID, ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}); err == nil {
				t.Fatal("unproved origin listed setup")
			}
			if countRows(t, db, "skill_setup_requests") != 0 {
				t.Fatal("rejected origin left durable setup")
			}
		})
	}
}

// skillSetupOriginAllowed reports whether a task work Run's in-chat setup
// origin resolves to the task's source conversation and message.
func skillSetupOriginAllowed(t *testing.T, store *sqlite.SQLiteStore, task *ConversationTaskResult) bool {
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
	_, store, _, task := skillSetupTaskFixture(t)
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
			_, store, db, task := skillSetupTaskFixture(t)
			scope := task.Task.Scope
			rewriteRowPayload(t, db, "agent_session_entries", scope, task.Task.SourceToolEntryID, func(entry *SessionEntry) {
				rewriteRowPayload(t, db, "conversation_tasks", scope, task.Task.ID, func(ledger *ConversationTask) {
					tamper(entry, ledger)
				})
			})
			if skillSetupOriginAllowed(t, store, task) {
				t.Fatal("tampered task start proved an origin")
			}
		})
	}
}

func TestSkillSetupOriginDeniesTaskWithoutEntry(t *testing.T) {
	for _, scenario := range []string{"no-entry-id", "entry-missing"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, db, task := skillSetupTaskFixture(t)
			scope := task.Task.Scope
			switch scenario {
			case "no-entry-id":
				rewriteRowPayload(t, db, "conversation_tasks", scope, task.Task.ID, func(ledger *ConversationTask) {
					ledger.SourceToolEntryID = ""
				})
			case "entry-missing":
				execSQL(t, db, `DELETE FROM agent_session_entries WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, task.Task.SourceToolEntryID)
			}
			if skillSetupOriginAllowed(t, store, task) {
				t.Fatal("task without its start_task entry proved an origin")
			}
		})
	}
}
