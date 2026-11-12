package runtime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// skillSetupTaskFixture also returns the test's own connection to the
// store's file, for the rows the tests tamper with.
func skillSetupTaskFixture(t *testing.T) (*runtime.SkillBindingActionDispatcher, *sqlite.Store, *sql.DB, *models.ConversationTaskResult) {
	t.Helper()
	store, db := newTestSQLiteStore(t)
	start := skillSetupTaskStart(t, store)
	result, err := runtime.NewConversationTaskService(store).Start(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skillActionCatalog(t, t.Context(), start.Scope, "agent")
	dispatcher, err := runtime.NewSkillBindingActionDispatcher(store, catalog, nil, skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reader", Readiness: skill.DiscoveryReadinessBindable}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, store, db, result
}

// skillSetupTaskStart returns the request that starts a conversation task
// from a chat reply Run's start_task call: the Run is running, leased by
// "worker", its step completed and applied, and the tool entry pending. It
// is a copy of the conversation task fixtures (settledTaskSourceFixture,
// taskStartRequest) on exported API, kept here because those fixtures live
// with the conversation run tests.
func skillSetupTaskStart(t *testing.T, store *sqlite.Store) runtime.StartConversationTaskRequest {
	t.Helper()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "worker-task"}
	if _, err := portfolio.NewSpaceService(store).CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.DefaultSpaceLook}); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	conversations := runtime.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtime.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "worker-task-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(ctx, runtime.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender-" + string(models.ConversationParticipantUser)},
		Intent: models.MessageIntentQuestion, Content: "Review the release in the background", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "task-user-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := runtime.NewConversationRunScheduler(store, store, runtime.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", run, err)
	}
	// Settle one completed step, as the loop does before the task starts.
	turns := activity.NewAgentTurnService(store, store)
	runs := activity.NewRunActivityService(store, store)
	if run.Status == models.AgentRunStatusPlanning {
		if run, _, err = runs.TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
			Summary: "Run started", LeaseOwner: "worker"}); err != nil {
			t.Fatalf("start run: %v", err)
		}
	}
	step, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: run.ID, SessionID: run.SessionID, Producer: models.StepProducerModel,
		WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("begin step: %v", err)
	}
	if step, err = turns.FinishTurn(ctx, scope, step.ID, activity.FinishAgentTurnRequest{ExpectedRevision: step.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: "worker"}); err != nil {
		t.Fatalf("finish step: %v", err)
	}
	if run, _, err = runs.TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning, Summary: "Step applied",
		ExpectedRevision: run.Revision, TurnID: step.ID, AppliedTurn: step.Sequence, LeaseOwner: "worker"}); err != nil {
		t.Fatalf("apply step: %v", err)
	}
	// The step's [assistant, start_task] entries follow the trigger message
	// in the Run's lane session.
	const callID = "call-start-task"
	goal, ack := "Review the release and report the outcome", "I’ve started the release review."
	now := time.Now().UTC()
	lease := models.AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	thread, _ := run.Context["threadRootMessageId"].(string)
	if _, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{Scope: scope, ConversationID: conversationID,
		ThreadRootID: thread, AgentID: run.AssignedAgentID, RunID: run.ID, Lease: lease, Now: now}); err != nil {
		t.Fatalf("open lane session: %v", err)
	}
	entryID := models.SessionEntryID(run.SessionID, models.ToolEntryKey(step.ID, callID))
	args, _ := json.Marshal(map[string]string{"goal": goal, "acknowledgment": ack})
	user := &models.SessionEntry{ID: models.SessionEntryID(run.SessionID, "message:"+triggerID), RunID: run.ID, Type: models.SessionEntryUser,
		SourceKey: "message:" + triggerID, User: &models.UserEntry{Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: "requester"},
			Content: []models.ContentPart{storetest.TextPart("Please do this in the background.")}, MessageID: triggerID}}
	assistant := &models.SessionEntry{ID: models.SessionEntryID(run.SessionID, models.AssistantEntryKey(step.ID)), RunID: run.ID, StepID: step.ID, Step: step.Sequence,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: callID, Name: string(models.ControlToolStartTask), Arguments: args}}}}
	tool := &models.SessionEntry{ID: entryID, RunID: run.ID, StepID: step.ID, Step: step.Sequence, Type: models.SessionEntryTool, State: models.ToolEntryPending,
		ToolCallID: callID, Tool: &models.ToolEntry{Name: string(models.ControlToolStartTask), Arguments: args, SideEffect: models.ToolSideEffectWrite}}
	for _, batch := range [][]*models.SessionEntry{{user}, {assistant, tool}} {
		if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{Scope: scope, SessionID: run.SessionID,
			RunID: run.ID, Lease: &lease, Entries: batch, Now: now}); err != nil {
			t.Fatalf("append start_task entries: %v", err)
		}
	}
	current, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || current == nil {
		t.Fatalf("get source: %#v %v", current, err)
	}
	return runtime.StartConversationTaskRequest{Scope: scope, SourceRunID: run.ID, ExpectedSourceRevision: current.Revision, WorkerID: "worker",
		TurnID: step.ID, ToolEntryID: entryID, AssignedAgentID: run.AssignedAgentID, TaskKey: models.StartTaskKey(entryID),
		Goal: goal, Acknowledgment: ack}
}

func TestSkillSetupTaskOriginSupportsIndependentWorkAndExactForkLineage(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "fork"}[fork], func(t *testing.T) {
			dispatcher, store, _, task := skillSetupTaskFixture(t)
			run := task.WorkRun
			if fork {
				child := models.CloneAgentRun(run)
				child.ID = "setup-child"
				child.ParentRunID = run.ID
				child.Source = models.RunSourceFork
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
				claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, Kind: models.RunKindAgentWork, WorkerID: "setup-worker",
					AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4})
				if err != nil || claimed == nil || claimed.ID != run.ID {
					t.Fatalf("claim task work: %#v %v", claimed, err)
				}
				step, err := activity.NewAgentTurnService(store, store).BeginTurn(t.Context(), activity.BeginAgentTurnRequest{Scope: run.Scope, RunID: run.ID, SessionID: claimed.SessionID, Producer: models.StepProducerModel,
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
				call := &models.ActionCall{
					ID: "task-setup-call", Scope: run.Scope, RunID: run.ID, TurnID: step.ID, DeploymentID: "agent",
					IdempotencyKey: "loop:" + step.ID + ":call-setup",
					SkillID:        models.SkillManagementSkillID, SkillVersion: runtime.SkillManagementSkillVersion, Action: agentrun.SkillActionRequestSetup,
					Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
					Status: models.ActionCallStatusRunning, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
					Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"},
					Attempt:   1, MaxAttempts: 1, LeaseOwner: "setup-worker", LeaseExpiresAt: &expires,
					AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
				}
				call.InvocationDigest = models.ComputeActionInvocationDigest(call)
				if _, err := store.CreateActionProposal(t.Context(), models.ActionProposalRecord{
					Call: call, ExpectedRunRevision: claimed.Revision, Lease: &models.AgentRunLeaseGuard{WorkerID: "setup-worker", Now: now},
					Event: &models.ActivityEvent{ID: "setup-origin-action", Scope: run.Scope, RunID: run.ID, EventType: "action.started", Summary: "Execute setup request", CreatedAt: now},
				}); err != nil {
					t.Fatal(err)
				}
				run = claimed
			}
			// Copied presentation hints must never redirect the persisted task.
			run = models.CloneAgentRun(run)
			run.Context["conversationId"], run.Context["triggerMessageId"] = "forged-chat", "forged-message"
			before, _ := dispatcher.Catalog().ListBindings(t.Context(), skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent")
			_, err := dispatcher.RequestSkillSetup(t.Context(), agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "task-setup-call", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect the account"}}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
			if err != nil {
				t.Fatal(err)
			}
			request, err := store.GetSkillSetupRequest(t.Context(), run.Scope, "skill-setup:task-setup-call")
			if err != nil || request == nil || request.ConversationID != task.Task.ConversationID || request.TriggerMessageID != task.Task.SourceMessageID || request.RunID != run.ID || request.ActionCallID != "task-setup-call" || request.DeploymentID != run.AssignedAgentID {
				t.Fatalf("wrong causal setup: %#v %v", request, err)
			}
			listed, err := dispatcher.ListSkillSetupRequests(t.Context(), run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
			if err != nil || len(listed["requests"].([]interface{})) != 1 {
				t.Fatalf("task could not list its origin setup: %#v %v", listed, err)
			}
			after, _ := dispatcher.Catalog().ListBindings(t.Context(), skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent")
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
			run := models.CloneAgentRun(task.WorkRun)
			deploymentID := "agent"
			switch scenario {
			case "non-task":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(canonical *models.AgentRun) {
					delete(canonical.Context, models.ConversationTaskContextKey)
					canonical.ConcurrencyKey = "ordinary-work"
				})
				execSQL(t, db, `UPDATE conversation_tasks SET work_run_id = '' WHERE scope_kind = ? AND scope_id = ? AND id = ?`, run.Scope.Kind, run.Scope.ID, task.Task.ID)
			case "foreign-scope":
				run.Scope.ID = "other"
			case "foreign-agent":
				deploymentID = "other"
			case "source-changed":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, task.Task.SourceRunID, func(source *models.AgentRun) {
					source.Context["triggerMessageId"] = "other"
				})
			case "actor-changed":
				rewriteRowPayload(t, db, "conversation_tasks", run.Scope, task.Task.ID, func(ledger *models.ConversationTask) {
					ledger.AuthenticatedActor.ID = "other"
				})
			case "thread-changed":
				rewriteRowPayload(t, db, "conversation_tasks", run.Scope, task.Task.ID, func(ledger *models.ConversationTask) {
					ledger.ThreadRootID = "other"
				})
			case "turn-changed":
				rewriteRowPayload(t, db, "agent_turns", run.Scope, task.Task.SourceTurnID, func(turn *models.AgentTurn) {
					turn.Status = models.AgentTurnStatusFailed
				})
				execSQL(t, db, `UPDATE agent_turns SET status = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, models.AgentTurnStatusFailed, run.Scope.Kind, run.Scope.ID, task.Task.SourceTurnID)
			case "archived":
				rewriteRowPayload(t, db, "conversations", run.Scope, task.Task.ConversationID, func(conversation *models.Conversation) {
					conversation.Status = models.ConversationStatusArchived
				})
				execSQL(t, db, `UPDATE conversations SET status = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, models.ConversationStatusArchived, run.Scope.Kind, run.Scope.ID, task.Task.ConversationID)
			case "fork-owner":
				rewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(canonical *models.AgentRun) {
					canonical.Owner.ID = "other"
				})
			case "missing-ledger":
				execSQL(t, db, `DELETE FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ? AND id = ?`, run.Scope.Kind, run.Scope.ID, task.Task.ID)
			}
			input := agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "rejected", Scope: run.Scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect"}}
			if _, err := dispatcher.RequestSkillSetup(t.Context(), input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
				t.Fatal("unproved origin created setup")
			}
			if _, err := dispatcher.ListSkillSetupRequests(t.Context(), run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
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
func skillSetupOriginAllowed(t *testing.T, store *sqlite.Store, task *models.ConversationTaskResult) bool {
	t.Helper()
	run := task.WorkRun
	conversationID, messageID, err := runtime.SkillSetupConversationOrigin(t.Context(), store, run, run.AssignedAgentID, run.Owner)
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
	if task.Task.Mode != models.ConversationTaskModeIndependent || task.Task.SourceToolEntryID == "" || task.Task.TaskKey != models.StartTaskKey(task.Task.SourceToolEntryID) {
		t.Fatalf("fixture task is not admitted from a start_task entry: %#v", task.Task)
	}
	entry, err := store.GetSessionEntry(t.Context(), task.Task.Scope, task.Task.SourceToolEntryID)
	if err != nil || entry.State != models.ToolEntryCompleted || entry.StepID != task.Task.SourceTurnID {
		t.Fatalf("source entry: %#v %v", entry, err)
	}
	if !skillSetupOriginAllowed(t, store, task) {
		t.Fatal("task started by its step's start_task entry was denied")
	}
}

func TestSkillSetupOriginDeniesTamperedGoal(t *testing.T) {
	cases := map[string]func(entry *models.SessionEntry, task *models.ConversationTask){
		"entry-goal": func(entry *models.SessionEntry, task *models.ConversationTask) {
			entry.Tool.Arguments, _ = json.Marshal(map[string]string{"goal": "Something else", "acknowledgment": task.Acknowledgment})
		},
		"task-goal":           func(_ *models.SessionEntry, task *models.ConversationTask) { task.Goal = "Something else" },
		"task-acknowledgment": func(_ *models.SessionEntry, task *models.ConversationTask) { task.Acknowledgment = "Something else" },
		"task-key":            func(_ *models.SessionEntry, task *models.ConversationTask) { task.TaskKey = "task-other" },
		"entry-tool": func(entry *models.SessionEntry, _ *models.ConversationTask) {
			entry.Tool.Name = string(models.ControlToolFork)
		},
		"entry-open": func(entry *models.SessionEntry, _ *models.ConversationTask) { entry.State = models.ToolEntryPending },
		"entry-run":  func(entry *models.SessionEntry, _ *models.ConversationTask) { entry.RunID = "other-run" },
		"entry-step": func(entry *models.SessionEntry, _ *models.ConversationTask) { entry.Step++ },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			_, store, db, task := skillSetupTaskFixture(t)
			scope := task.Task.Scope
			rewriteRowPayload(t, db, "agent_session_entries", scope, task.Task.SourceToolEntryID, func(entry *models.SessionEntry) {
				rewriteRowPayload(t, db, "conversation_tasks", scope, task.Task.ID, func(ledger *models.ConversationTask) {
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
				rewriteRowPayload(t, db, "conversation_tasks", scope, task.Task.ID, func(ledger *models.ConversationTask) {
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
