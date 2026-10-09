package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type skillSetupTaskTestStore interface {
	KernelStore
	ConversationStore
	ConversationTaskStore
	AgentRunAdmissionStore
}

// SQLite is closed and reopened, rather than merely constructing a new service,
// so the callback must recover both task authority and its waiting run from disk.
func forSkillSetupTaskStores(t *testing.T, test func(*testing.T, skillSetupTaskTestStore, func() skillSetupTaskTestStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		store := NewMemoryStore()
		test(t, store, func() skillSetupTaskTestStore { return store })
	})
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "setup-task.db")
		var current *SQLiteStore
		open := func() skillSetupTaskTestStore {
			if current != nil {
				if err := current.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			current, err = NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}
		store := open()
		t.Cleanup(func() { _ = current.Close() })
		test(t, store, open)
	})
}

type skillSetupTaskWaitFixture struct {
	store   skillSetupTaskTestStore
	task    *ConversationTaskResult
	catalog *skill.Catalog
	request *SkillSetupRequest
	call    *ActionCall
}

func admitSkillSetupWaitTask(t *testing.T, store skillSetupTaskTestStore) *ConversationTaskResult {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "task-sql"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "task-agent"}, Title: "Task setup", IdempotencyKey: "task-setup-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion, "Read the community in the background", "task-setup-source")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule setup task source: %#v %v", scheduled, err)
	}
	source, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || source == nil || source.ID != scheduled.Run.ID {
		t.Fatalf("claim setup task source: %#v %v", source, err)
	}
	settled, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: source.ID, WorkerID: "worker"}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: &TurnTaskProposal{
			TaskKey: "skill-setup", Goal: "Read the community and report the outcome", Acknowledgment: "I have started reading the community.", Budget: &BudgetPolicy{MaxTurns: 5},
		}}, nil
	}))
	if err != nil || settled == nil || settled.Turn == nil || settled.Turn.RequestedTask == nil {
		t.Fatalf("settle setup task source turn: %#v %v", settled, err)
	}
	task, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(settled.Run, settled.Turn))
	if err != nil {
		t.Fatal(err)
	}
	task.SourceRun, err = store.GetAgentRun(t.Context(), scope, source.ID)
	if err != nil || task.SourceRun == nil {
		t.Fatalf("load completed setup task source: %v", err)
	}
	return task
}

func newSkillSetupTaskFixture(t *testing.T, store skillSetupTaskTestStore) *skillSetupTaskWaitFixture {
	t.Helper()
	return newSkillSetupTaskFixtureForKind(t, store, "configure")
}

func newSkillSetupTaskFixtureForKind(t *testing.T, store skillSetupTaskTestStore, kind string, accountVersion ...string) *skillSetupTaskWaitFixture {
	t.Helper()
	const sourceIdentity = "https://skills.example::reddit.reader"
	task := admitSkillSetupWaitTask(t, store)
	if task.SourceRun.Status != AgentRunStatusCompleted || task.WorkRun.ParentRunID != "" {
		t.Fatal("fixture must be independent work whose source already completed")
	}
	claim := conversationTaskAdmissionClaim(time.Now().UTC().Add(time.Second), 0)
	claim.WorkerID = "setup-worker"
	run, err := store.ClaimNextAgentRun(t.Context(), claim)
	if err != nil || run == nil || run.ID != task.WorkRun.ID {
		t.Fatalf("claim setup task: %#v %v", run, err)
	}
	catalog := skillActionCatalog(t, t.Context(), task.Task.Scope, task.Task.TargetAgentID)
	targetVersion := "1.0.0"
	if len(accountVersion) > 0 {
		durable, ok := store.(skill.CatalogStore)
		if !ok {
			t.Fatal("binding upgrade fixture requires durable Skill catalog storage")
		}
		catalog = skill.NewCatalogWithStore(durable)
		if err := catalog.Register(t.Context(), SkillManagementSkill()); err != nil {
			t.Fatal(err)
		}
		for _, version := range []string{"1.0.0", "2.0.0"} {
			definition := redditSkillDefinition()
			definition.Version = version
			definition.Source = &skill.SourceProvenance{Identity: sourceIdentity, Format: "openseal.dev/v1alpha1"}
			if err := catalog.Register(t.Context(), definition); err != nil {
				t.Fatal(err)
			}
		}
		if err := catalog.Bind(t.Context(), &skill.Binding{
			ID: "reddit-account", Revision: 1, Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID},
			DeploymentID: run.AssignedAgentID, SkillID: "reddit.reader", SkillVersion: accountVersion[0], SourceIdentity: sourceIdentity,
			MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://saved-reddit-account"}},
		}); err != nil {
			t.Fatal(err)
		}
		targetVersion = "2.0.0"
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{
		ID: "setup-actions", Revision: 1, Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID},
		DeploymentID: run.AssignedAgentID, SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion,
		AllowedActions: []string{SkillActionRequestSetup, SkillActionListSetupRequests}, MaximumRisk: skill.RiskLevelRead,
	}); err != nil {
		t.Fatal(err)
	}
	validator, err := NewSkillBindingActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]interface{}{"kind": kind, "skillId": "reddit.reader", "skillVersion": targetVersion, "sourceIdentity": sourceIdentity, "reason": "Read the community for this task", "requiredActions": []interface{}{"read"}, "enablePrompt": true}
	if len(accountVersion) > 0 {
		arguments["bindingId"] = "reddit-account"
	}
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(t.Context(), ProposeActionRequest{
		Scope: run.Scope, RunID: run.ID, WorkerID: "setup-worker", DeploymentID: run.AssignedAgentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionRequestSetup,
		Arguments: arguments,
		Summary:   "Ask for Reddit setup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval != nil || proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("setup request proposal: %#v", proposal)
	}
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		if request.Scope.ID != run.Scope.ID || request.DeploymentID != run.AssignedAgentID {
			t.Fatal("setup authority was not derived from the task run")
		}
		candidate := skill.DiscoveryCandidate{
			ID: "reddit.reader", Version: targetVersion, Name: "Reddit", SourceIdentity: sourceIdentity,
			Readiness: skill.DiscoveryReadinessBindable, PromptAvailable: true,
			Actions: []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}},
		}
		if kind == "install" {
			candidate.Readiness = skill.DiscoveryReadinessNeedsInstallation
			candidate.Compatibility = []skill.DiscoveryCompatibility{
				{Requirement: "installation", Compatible: false, Reference: "listing:reddit-reader", Evidence: "Authorized source requires installation"},
				{Requirement: "source_digest", Compatible: true, Reference: "sha256:reddit-reader-1", Evidence: "Authorized source digest verified"},
			}
		}
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{candidate}}, nil
	})
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(t.Context(), run.Scope, "setup-action-worker", time.Minute)
	if err != nil || executed == nil || executed.Call.Status != ActionCallStatusSucceeded {
		if executed != nil && executed.Call != nil {
			t.Fatalf("canonical setup execution: status=%s action error=%s worker error=%v", executed.Call.Status, executed.Call.Error, err)
		}
		t.Fatalf("canonical setup execution: %#v %v", executed, err)
	}
	request, err := store.GetSkillSetupRequest(t.Context(), run.Scope, "skill-setup:"+proposal.Call.ID)
	if err != nil || request == nil || request.Status != "pending" || request.RunID != task.WorkRun.ID || request.ActionCallID != proposal.Call.ID || request.TriggerMessageID != task.Task.SourceMessageID || request.ConversationID != task.Task.ConversationID {
		t.Fatalf("task setup provenance: %#v %v", request, err)
	}
	return &skillSetupTaskWaitFixture{store: store, task: task, catalog: catalog, request: request, call: executed.Call}
}

func (f *skillSetupTaskWaitFixture) wait(t *testing.T) *AgentRun {
	t.Helper()
	result, err := NewTurnCoordinator(f.store, f.store, f.store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: f.request.Scope, RunID: f.request.RunID, WorkerID: "setup-worker",
	}, TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		checkpoint := cloneMap(input.Run.Checkpoint)
		if checkpoint == nil {
			checkpoint = map[string]interface{}{}
		}
		checkpoint["phase"] = "resume-existing-analysis"
		return &TurnOutcome{
			NextRunStatus:          AgentRunStatusWaitingForEvent,
			WakeCondition:          &WakeCondition{Type: ConversationTaskSkillSetupWakeType, Reference: f.request.ID},
			RunOutput:              map[string]interface{}{"summary": "Complete the Reddit setup form to continue this task."},
			ContinuationCheckpoint: checkpoint,
		}, nil
	}))
	if err != nil || result == nil || result.Run.Status != AgentRunStatusWaitingForEvent || result.Run.WakeCondition == nil || result.Run.WakeCondition.Reference != f.request.ID {
		t.Fatalf("wait for exact setup: %#v %v", result, err)
	}
	// Compare durable snapshots; the persisted checkpoint normalizes JSON
	// numbers and timestamps that a freshly returned transition can retain.
	saved, err := f.store.GetAgentRun(t.Context(), f.request.Scope, f.request.RunID)
	if err != nil || saved == nil {
		t.Fatalf("load saved setup wait: %v", err)
	}
	return saved
}

func (f *skillSetupTaskWaitFixture) resolve(t *testing.T, status string) {
	t.Helper()
	request := cloneSkillSetupRequest(f.request)
	request.Status, request.ResolvedBy = status, f.task.Task.AuthenticatedActor.ID
	request.Revision++
	request.UpdatedAt = time.Now().UTC()
	if status == "resolved" {
		request.ResolvedBindingID, request.ResolvedBindingRevision = "reddit-account", 3
	}
	if err := f.store.SaveSkillSetupRequest(t.Context(), request, f.request.Revision); err != nil {
		t.Fatal(err)
	}
	f.request = request
}

func (f *skillSetupTaskWaitFixture) reconcile(t *testing.T) (*AgentRunCommandResult, bool, error) {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(f.store, f.store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler.ReconcileSkillSetupTask(t.Context(), f.request.Scope, f.request.ID)
}

func skillSetupCompletionEvents(t *testing.T, store skillSetupTaskTestStore, run *AgentRun) int {
	t.Helper()
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: run.Scope, RunID: run.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.EventType == "run.skill_setup_completed" {
			count++
		}
	}
	return count
}

func TestSkillSetupTaskWaitResumesSameWorkAfterRestart(t *testing.T) {
	for _, status := range []string{"resolved", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, restart func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixture(t, store)
				waiting := f.wait(t)
				f.store = restart()
				pending, handled, err := f.reconcile(t)
				if err != nil || !handled || pending == nil || pending.Run.Status != AgentRunStatusWaitingForEvent || pending.Run.Revision != waiting.Revision || len(pending.Run.PendingInterventions) != 0 || skillSetupCompletionEvents(t, f.store, waiting) != 0 {
					t.Fatalf("pending setup woke task: %#v %v %v", pending, handled, err)
				}
				f.resolve(t, status)
				// A crash after persisting the user's form must still allow one wake.
				f.store = restart()
				resumed, handled, err := f.reconcile(t)
				if err != nil || !handled || resumed == nil || resumed.Run.ID != waiting.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil || resumed.Event == nil || resumed.Event.EventType != "run.skill_setup_completed" {
					t.Fatalf("resume same work: %#v %v %v", resumed, handled, err)
				}
				if !reflect.DeepEqual(resumed.Run.Checkpoint, waiting.Checkpoint) || !reflect.DeepEqual(resumed.Run.Budget, waiting.Budget) || !reflect.DeepEqual(resumed.Run.BudgetUsage, waiting.BudgetUsage) || resumed.Run.LastAppliedTurn != waiting.LastAppliedTurn || !reflect.DeepEqual(resumed.Run.Context, waiting.Context) {
					t.Fatal("setup resolution changed task progress, budget, or identity")
				}
				if len(resumed.Run.PendingInterventions) != 1 || resumed.Run.PendingInterventions[0].SkillSetupResolution == nil {
					t.Fatalf("missing typed setup resolution: %#v", resumed.Run.PendingInterventions)
				}
				receipt := resumed.Run.PendingInterventions[0].SkillSetupResolution
				if receipt.Scope != f.request.Scope || receipt.RunID != waiting.ID || receipt.RequestID != f.request.ID || receipt.Status != status || receipt.ActionCallID != f.call.ID || receipt.Revision != f.request.Revision || receipt.ResolvedBindingID != f.request.ResolvedBindingID || receipt.ResolvedBindingRevision != f.request.ResolvedBindingRevision {
					t.Fatalf("resolution receipt lost exact provenance: %#v", receipt)
				}
				f.store = restart()
				again, handled, err := f.reconcile(t)
				if err != nil || !handled || again == nil || again.Run.Revision != resumed.Run.Revision || len(again.Run.PendingInterventions) != 1 || skillSetupCompletionEvents(t, f.store, again.Run) != 1 {
					t.Fatalf("callback duplicated wake: %#v %v %v", again, handled, err)
				}
				finished, err := NewTurnCoordinator(f.store, f.store, f.store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: waiting.Scope, RunID: waiting.ID, WorkerID: "resume-worker"}, TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
					if len(input.Run.PendingInterventions) != 1 || input.Run.PendingInterventions[0].SkillSetupResolution == nil || input.Run.PendingInterventions[0].SkillSetupResolution.Status != status || input.Run.Checkpoint["phase"] != "resume-existing-analysis" {
						t.Fatal("resumed model turn did not receive persisted progress and resolution")
					}
					return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Task concluded with the user's setup decision."}}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				f.store = restart()
				afterCompletion, handled, err := f.reconcile(t)
				if err != nil || !handled || afterCompletion == nil || afterCompletion.Run.Status != AgentRunStatusCompleted || afterCompletion.Run.Revision != finished.Run.Revision || skillSetupCompletionEvents(t, f.store, finished.Run) != 1 {
					t.Fatalf("late callback reopened completed task: %#v %v %v", afterCompletion, handled, err)
				}
				source, err := f.store.GetAgentRun(t.Context(), f.request.Scope, f.task.SourceRun.ID)
				if err != nil || !reflect.DeepEqual(source, f.task.SourceRun) {
					t.Fatalf("setup changed completed source: %#v %v", source, err)
				}
				calls, err := f.store.ListActionCalls(t.Context(), ActionFilter{Scope: waiting.Scope, RunID: waiting.ID})
				if err != nil || len(calls) != 1 || calls[0].ID != f.call.ID || calls[0].Revision != f.call.Revision || calls[0].Status != ActionCallStatusSucceeded || calls[0].Output["setupRequest"].(map[string]interface{})["status"] != "pending" {
					t.Fatalf("resolution repeated or rewrote the completed action: %#v %v", calls, err)
				}
				tasks, err := f.store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: waiting.Scope, Owner: waiting.Owner, ConversationID: f.request.ConversationID, Limit: 100})
				if err != nil || len(tasks) != 1 || tasks[0].Task.ID != f.task.Task.ID || tasks[0].WorkRun.ID != waiting.ID {
					t.Fatalf("resolution recreated task: %#v %v", tasks, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskResolutionPreservesOperatorPauseAndCancel(t *testing.T) {
	for _, command := range []AgentRunCommandKind{AgentRunCommandPause, AgentRunCommandCancel} {
		t.Run(string(command), func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, restart func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixture(t, store)
				waiting := f.wait(t)
				changed, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{
					Scope: waiting.Scope, RunID: waiting.ID, ExpectedRevision: waiting.Revision, Kind: command,
					Actor: ActivityActor{Type: "user", ID: f.task.Task.AuthenticatedActor.ID},
				})
				if err != nil {
					t.Fatal(err)
				}
				changed.Run, err = store.GetAgentRun(t.Context(), waiting.Scope, waiting.ID)
				if err != nil {
					t.Fatal(err)
				}
				if command == AgentRunCommandCancel {
					// Canceling closes the Run's open setup form; it can no longer
					// be completed for this Run.
					closed, err := store.GetSkillSetupRequest(t.Context(), f.request.Scope, f.request.ID)
					if err != nil || closed == nil || closed.Status != "dismissed" || closed.ResolvedBy != TerminalRunInteractionCloser {
						t.Fatalf("cancel left the setup form open: %#v %v", closed, err)
					}
					f.request = closed
				} else {
					f.resolve(t, "resolved")
				}
				f.store = restart()
				result, handled, err := f.reconcile(t)
				if err != nil || !handled || result == nil || !reflect.DeepEqual(result.Run, changed.Run) || skillSetupCompletionEvents(t, f.store, waiting) != 0 {
					t.Fatalf("setup overrode operator %s: %#v %v %v", command, result, handled, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskReconciliationRecoversResolutionBeforeWait(t *testing.T) {
	forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, restart func() skillSetupTaskTestStore) {
		f := newSkillSetupTaskFixture(t, store)
		// The form can finish before the model commits its wait. The callback
		// claims the task request, but must not create a foreground conversation.
		f.resolve(t, "resolved")
		beforeWait, handled, err := f.reconcile(t)
		if err != nil || !handled || beforeWait == nil || len(beforeWait.Run.PendingInterventions) != 0 || skillSetupCompletionEvents(t, store, beforeWait.Run) != 0 {
			t.Fatalf("resolution before wait: %#v %v %v", beforeWait, handled, err)
		}
		waiting := f.wait(t)
		f.store = restart()
		scheduler, err := NewConversationRunScheduler(f.store, f.store, ConversationRunSchedulerConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scheduler.ReconcileScope(t.Context(), waiting.Scope); err != nil {
			t.Fatal(err)
		}
		resumed, err := f.store.GetAgentRun(t.Context(), waiting.Scope, waiting.ID)
		if err != nil || resumed == nil || resumed.Status != AgentRunStatusQueued || len(resumed.PendingInterventions) != 1 || skillSetupCompletionEvents(t, f.store, resumed) != 1 {
			t.Fatalf("scope reconciliation lost resolved setup: %#v %v", resumed, err)
		}
		if _, err := scheduler.ReconcileScope(t.Context(), waiting.Scope); err != nil {
			t.Fatal(err)
		}
		if skillSetupCompletionEvents(t, f.store, resumed) != 1 {
			t.Fatal("scope reconciliation repeated the setup wake")
		}
	})
}

func (f *skillSetupTaskWaitFixture) upgrade(t *testing.T) {
	t.Helper()
	installed := redditSkillDefinition()
	installed.Version, installed.Name = "2.0.0", "Reddit Reader 2"
	installed.Source = &skill.SourceProvenance{Identity: f.request.SourceIdentity, Format: "openseal.dev/v1alpha1"}
	if err := f.catalog.Register(t.Context(), installed); err != nil {
		t.Fatal(err)
	}
	selected, err := f.catalog.GetDefinitionVariant(t.Context(), f.request.SkillID, installed.Version, f.request.SourceIdentity)
	if err != nil || selected == nil || selected.Prompt == nil || selected.Actions["read"].Name != "read" {
		t.Fatalf("reviewed upgrade target is not installed with the requested features: %#v %v", selected, err)
	}
	// Match the host's explicit prepareSkillSetupUpgrade mutation. The original
	// action receipt stays immutable; only the canonical request advances by CAS.
	upgraded := cloneSkillSetupRequest(f.request)
	upgraded.SkillVersion, upgraded.SkillName = selected.Version, selected.Name
	upgraded.Kind = "configure"
	upgraded.InstallationReference, upgraded.SourceDigest = "", ""
	upgraded.Revision++
	upgraded.UpdatedAt = time.Now().UTC()
	if err := f.store.SaveSkillSetupRequest(t.Context(), upgraded, f.request.Revision); err != nil {
		t.Fatal(err)
	}
	f.request = upgraded
}

func TestSkillSetupTaskAuthorizedUpgradeResumesSameWorkAfterRestart(t *testing.T) {
	for _, kind := range []string{"configure", "install"} {
		t.Run(kind, func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, restart func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixtureForKind(t, store, kind)
				original := cloneSkillSetupRequest(f.request)
				if original.SourceIdentity == "" || len(original.RequiredActions) != 1 || !original.EnablePrompt || kind == "install" && (original.InstallationReference == "" || original.SourceDigest == "") {
					t.Fatal("upgrade fixture lacks canonical source or installation proof")
				}
				waiting := f.wait(t)
				f.upgrade(t)
				f.store = restart()
				pending, handled, err := f.reconcile(t)
				if err != nil || !handled || pending == nil || pending.Run.Revision != waiting.Revision || pending.Run.Status != AgentRunStatusWaitingForEvent || skillSetupCompletionEvents(t, f.store, waiting) != 0 {
					t.Fatalf("pending upgrade did not preserve task wait: %#v %v %v", pending, handled, err)
				}
				f.resolve(t, "resolved")
				f.store = restart()
				resumed, handled, err := f.reconcile(t)
				if err != nil || !handled || resumed == nil || resumed.Run.ID != waiting.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil || len(resumed.Run.PendingInterventions) != 1 {
					t.Fatalf("upgraded setup did not resume original task: %#v %v %v", resumed, handled, err)
				}
				receipt := resumed.Run.PendingInterventions[0].SkillSetupResolution
				if receipt == nil || receipt.RequestID != original.ID || receipt.ActionCallID != original.ActionCallID || receipt.Revision != original.Revision+2 || receipt.Status != "resolved" || receipt.ResolvedBindingID != f.request.ResolvedBindingID || receipt.ResolvedBindingRevision != f.request.ResolvedBindingRevision {
					t.Fatalf("upgraded resolution receipt: %#v", receipt)
				}
				if !reflect.DeepEqual(resumed.Run.Checkpoint, waiting.Checkpoint) || resumed.Run.BudgetUsage != waiting.BudgetUsage || !reflect.DeepEqual(resumed.Run.Budget, waiting.Budget) {
					t.Fatal("upgrade discarded existing task progress or budget")
				}
				saved, err := f.store.GetSkillSetupRequest(t.Context(), original.Scope, original.ID)
				if err != nil || saved == nil || saved.SkillVersion != "2.0.0" || saved.SkillName != "Reddit Reader 2" || saved.Kind != "configure" || saved.SourceIdentity != original.SourceIdentity || saved.InstallationReference != "" || saved.SourceDigest != "" || !reflect.DeepEqual(saved.RequiredActions, original.RequiredActions) || saved.EnablePrompt != original.EnablePrompt || saved.Reason != original.Reason || !saved.CreatedAt.Equal(original.CreatedAt) {
					t.Fatalf("upgraded canonical request lost its authority: %#v %v", saved, err)
				}
				action, err := f.store.GetActionCall(t.Context(), original.Scope, original.ActionCallID)
				if err != nil || action == nil || action.Revision != f.call.Revision || !reflect.DeepEqual(action.Output, f.call.Output) {
					t.Fatalf("upgrade rewrote the original successful action receipt: %#v %v", action, err)
				}
				f.store = restart()
				again, handled, err := f.reconcile(t)
				if err != nil || !handled || again == nil || again.Run.Revision != resumed.Run.Revision || skillSetupCompletionEvents(t, f.store, waiting) != 1 {
					t.Fatalf("upgraded callback duplicated task wake: %#v %v %v", again, handled, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskBindingUpgradeRebaseResumesSameWorkAfterRestart(t *testing.T) {
	forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, restart func() skillSetupTaskTestStore) {
		f := newSkillSetupTaskFixtureForKind(t, store, "configure", "1.0.0")
		original := cloneSkillSetupRequest(f.request)
		if original.Phase != SkillSetupPhaseBindingUpgrade || original.SkillVersion != "2.0.0" || original.BindingVersion != "1.0.0" || original.BindingRevision != 1 || original.BindingID != "reddit-account" {
			t.Fatalf("setup did not retain the old account and newer requested target: %#v", original)
		}
		waiting := f.wait(t)
		upgradeStore, ok := store.(SkillReferenceUpgradeStore)
		if !ok {
			t.Fatal("binding upgrade fixture requires canonical reference upgrade storage")
		}
		service := NewSkillReferenceUpgradeService(upgradeStore, f.catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: original.Scope, DeploymentID: original.DeploymentID, BindingID: original.BindingID,
			ToVersion: original.SkillVersion, ToSourceIdentity: original.SourceIdentity,
		})
		if err != nil {
			t.Fatal(err)
		}
		actor := ActivityActor{Type: "user", ID: f.task.Task.AuthenticatedActor.ID}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: actor, Reason: "Upgrade the saved account to the reviewed installed version",
			Approval: &SkillReferenceUpgradeApproval{Principal: actor, Reason: "Approve the reviewed account upgrade"},
		}); err != nil {
			t.Fatal(err)
		}
		skillScope := skill.ScopeReference{Kind: original.Scope.Kind, ID: original.Scope.ID}
		upgraded, err := f.catalog.GetBinding(t.Context(), skillScope, original.DeploymentID, original.BindingID)
		if err != nil || upgraded == nil || len(upgraded.Lifecycle) == 0 || upgraded.Lifecycle[len(upgraded.Lifecycle)-1].SkillUpgrade == nil {
			t.Fatalf("canonical upgrade did not persist lifecycle proof: %#v %v", upgraded, err)
		}
		rebased, err := RebaseSkillSetupRequestAfterBindingUpgrade(original, original.Revision, upgraded)
		if err != nil {
			t.Fatal(err)
		}
		if rebased.SkillVersion != original.SkillVersion || rebased.Phase != SkillSetupPhaseConfiguration || rebased.BindingVersion != original.SkillVersion || rebased.BindingRevision <= original.BindingRevision || rebased.Revision != original.Revision+1 || rebased.Status != "pending" {
			t.Fatalf("binding rebase changed the target or prematurely resolved setup: %#v", rebased)
		}
		if err := store.SaveSkillSetupRequest(t.Context(), rebased, original.Revision); err != nil {
			t.Fatal(err)
		}
		f.request = rebased
		f.store = restart()
		pending, handled, err := f.reconcile(t)
		if err != nil || !handled || pending == nil || pending.Run.Revision != waiting.Revision || pending.Run.Status != AgentRunStatusWaitingForEvent || skillSetupCompletionEvents(t, f.store, waiting) != 0 {
			t.Fatalf("binding rebase woke work before configuration: %#v %v %v", pending, handled, err)
		}
		// The upgrade itself is not a saved configuration. Record the subsequent
		// governed configuration revision before resolving the same request.
		f.catalog = skill.NewCatalogWithStore(f.store.(skill.CatalogStore))
		upgraded, err = f.catalog.GetBinding(t.Context(), skillScope, original.DeploymentID, original.BindingID)
		if err != nil || upgraded == nil {
			t.Fatalf("reload upgraded account: %#v %v", upgraded, err)
		}
		upgraded.AllowedActions, upgraded.EnablePrompt = []string{"read"}, true
		upgraded.Config = map[string]interface{}{"subreddits": []interface{}{"science"}}
		configured, err := f.catalog.UpsertBinding(t.Context(), skill.UpsertBindingRequest{
			Binding: upgraded, ExpectedRevision: rebased.BindingRevision,
			Actor: skill.BindingActor{Type: actor.Type, ID: actor.ID}, Reason: "Save reviewed task account configuration",
		})
		if err != nil || configured == nil || configured.Revision <= rebased.BindingRevision {
			t.Fatalf("save configuration after canonical upgrade: %#v %v", configured, err)
		}
		f.resolve(t, "resolved")
		if f.request.ResolvedBindingID != configured.ID || f.request.ResolvedBindingRevision != configured.Revision {
			t.Fatal("resolution did not refer to the separately saved configuration")
		}
		f.store = restart()
		resumed, handled, err := f.reconcile(t)
		if err != nil || !handled || resumed == nil || resumed.Run.ID != waiting.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil || len(resumed.Run.PendingInterventions) != 1 {
			t.Fatalf("rebased setup did not resume original task: %#v %v %v", resumed, handled, err)
		}
		receipt := resumed.Run.PendingInterventions[0].SkillSetupResolution
		if receipt == nil || receipt.RequestID != original.ID || receipt.ActionCallID != original.ActionCallID || receipt.Revision != original.Revision+2 || receipt.ResolvedBindingID != configured.ID || receipt.ResolvedBindingRevision != configured.Revision {
			t.Fatalf("rebased setup resolution lost durable identity: %#v", receipt)
		}
		if !reflect.DeepEqual(resumed.Run.Checkpoint, waiting.Checkpoint) || resumed.Run.BudgetUsage != waiting.BudgetUsage || !reflect.DeepEqual(resumed.Run.Budget, waiting.Budget) {
			t.Fatal("rebased setup discarded task progress or budget")
		}
		action, err := f.store.GetActionCall(t.Context(), original.Scope, original.ActionCallID)
		if err != nil || action == nil || action.Revision != f.call.Revision || !reflect.DeepEqual(action.Output, f.call.Output) {
			t.Fatalf("binding upgrade changed the original action receipt: %#v %v", action, err)
		}
		f.store = restart()
		again, handled, err := f.reconcile(t)
		if err != nil || !handled || again == nil || again.Run.Revision != resumed.Run.Revision || skillSetupCompletionEvents(t, f.store, waiting) != 1 {
			t.Fatalf("rebased setup callback duplicated work: %#v %v %v", again, handled, err)
		}
	})
}

func TestSkillSetupTaskOrdinaryConfigurationCannotForgeBindingRebase(t *testing.T) {
	for _, scenario := range []string{"binding-revision", "phase"} {
		t.Run(scenario, func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, _ func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixtureForKind(t, store, "configure", "2.0.0")
				if f.request.Phase != SkillSetupPhaseConfiguration || f.request.BindingVersion != f.request.SkillVersion {
					t.Fatal("fixture must start with an ordinary same-version account configuration")
				}
				waiting := f.wait(t)
				f.resolve(t, "resolved")
				proof := &skillSetupTaskProofOverrideStore{skillSetupTaskTestStore: store, request: func(request *SkillSetupRequest) {
					if scenario == "binding-revision" {
						request.BindingRevision++
					} else {
						request.Phase, request.BindingVersion = SkillSetupPhaseBindingUpgrade, "1.0.0"
						request.Status = "dismissed"
						request.ResolvedBindingID, request.ResolvedBindingRevision = "", 0
					}
				}}
				scheduler, err := NewConversationRunScheduler(proof, proof, ConversationRunSchedulerConfig{})
				if err != nil {
					t.Fatal(err)
				}
				result, handled, err := scheduler.ReconcileSkillSetupTask(t.Context(), waiting.Scope, f.request.ID)
				if !handled || !errors.Is(err, ErrInvalidSkillSetup) || result != nil && result.Run != nil && result.Run.Status == AgentRunStatusQueued {
					t.Fatalf("ordinary setup accepted forged %s rebase: %#v %v %v", scenario, result, handled, err)
				}
				unchanged, err := store.GetAgentRun(t.Context(), waiting.Scope, waiting.ID)
				if err != nil || !reflect.DeepEqual(unchanged, waiting) || skillSetupCompletionEvents(t, store, waiting) != 0 {
					t.Fatalf("forged %s rebase changed canonical work: %v", scenario, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskUpgradeRejectsUnversionedOrRepurposedTarget(t *testing.T) {
	for _, scenario := range []string{"same-version", "single-cas-resolution", "stale-revision", "purpose", "requested-actions", "source"} {
		t.Run(scenario, func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, _ func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixture(t, store)
				original := cloneSkillSetupRequest(f.request)
				waiting := f.wait(t)
				f.upgrade(t)
				f.resolve(t, "resolved")
				proof := &skillSetupTaskProofOverrideStore{skillSetupTaskTestStore: store, request: func(request *SkillSetupRequest) {
					switch scenario {
					case "same-version":
						request.SkillVersion = original.SkillVersion
					case "single-cas-resolution":
						request.Revision = original.Revision + 1
					case "stale-revision":
						request.Revision = original.Revision
					case "purpose":
						request.Reason = "Publish content for an unrelated task"
					case "requested-actions":
						request.RequiredActions = []string{"read", "publish"}
					case "source":
						request.SourceIdentity = "https://unrelated.example::reddit.reader"
					}
				}}
				scheduler, err := NewConversationRunScheduler(proof, proof, ConversationRunSchedulerConfig{})
				if err != nil {
					t.Fatal(err)
				}
				result, handled, err := scheduler.ReconcileSkillSetupTask(t.Context(), waiting.Scope, original.ID)
				if !handled || !errors.Is(err, ErrInvalidSkillSetup) || result != nil && result.Run != nil && result.Run.Status == AgentRunStatusQueued {
					t.Fatalf("forged %s upgrade accepted: %#v %v %v", scenario, result, handled, err)
				}
				unchanged, err := store.GetAgentRun(t.Context(), waiting.Scope, waiting.ID)
				if err != nil || !reflect.DeepEqual(unchanged, waiting) || skillSetupCompletionEvents(t, store, waiting) != 0 {
					t.Fatalf("forged %s upgrade mutated task: %v", scenario, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskRequestRequiresPersistedActionAndListRejectsCopiedContext(t *testing.T) {
	forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, _ func() skillSetupTaskTestStore) {
		f := newSkillSetupTaskFixture(t, store)
		run, err := store.GetAgentRun(t.Context(), f.request.Scope, f.request.RunID)
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := NewSkillBindingActionDispatcher(store, f.catalog, nil)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := dispatcher.listSkillSetupRequests(t.Context(), run, run.AssignedAgentID)
		if err != nil || listed == nil {
			t.Fatalf("proven task cannot list its setup: %#v %v", listed, err)
		}
		requests := listed["requests"].([]interface{})
		if len(requests) != 1 || requests[0].(map[string]interface{})["id"] != f.request.ID {
			t.Fatalf("task setup list: %#v", listed)
		}
		forged := cloneAgentRun(run)
		forged.ID, forged.RootRunID, forged.ConcurrencyKey = "copied-work", "copied-work", "copied-task"
		if err := store.CreateAgentRun(t.Context(), forged); err != nil {
			t.Fatal(err)
		}
		if _, err := dispatcher.listSkillSetupRequests(t.Context(), forged, forged.AssignedAgentID); err == nil {
			t.Fatal("copied conversation context granted task setup access")
		}
		if _, err := dispatcher.requestSkillSetup(t.Context(), ActionDispatchInput{
			Call: &ActionCall{ID: "model-invented-action", Scope: run.Scope, RunID: run.ID}, Arguments: cloneMap(f.call.Arguments),
		}, run, run.AssignedAgentID); err == nil {
			t.Fatal("unpersisted action was allowed to create a task setup request")
		}
		persisted, err := store.ListSkillSetupRequests(t.Context(), run.Scope, run.AssignedAgentID, f.request.ConversationID)
		if err != nil || len(persisted) != 1 || persisted[0].ID != f.request.ID {
			t.Fatalf("forged action changed setup requests: %#v %v", persisted, err)
		}
	})
}

// Override only reads to exercise persisted-proof validation without relying on
// a backend's internal representation or rewriting immutable channel history.
type skillSetupTaskProofOverrideStore struct {
	skillSetupTaskTestStore
	request func(*SkillSetupRequest)
	run     func(*AgentRun)
	message func(*ChannelMessage)
	action  func(*ActionCall)
}

func (s *skillSetupTaskProofOverrideStore) GetSkillSetupRequest(ctx context.Context, scope Scope, id string) (*SkillSetupRequest, error) {
	request, err := s.skillSetupTaskTestStore.GetSkillSetupRequest(ctx, scope, id)
	if request != nil && s.request != nil {
		s.request(request)
	}
	return request, err
}

func (s *skillSetupTaskProofOverrideStore) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	run, err := s.skillSetupTaskTestStore.GetAgentRun(ctx, scope, id)
	if run != nil && s.run != nil {
		s.run(run)
	}
	return run, err
}

func (s *skillSetupTaskProofOverrideStore) GetChannelMessage(ctx context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
	message, err := s.skillSetupTaskTestStore.GetChannelMessage(ctx, scope, conversationID, id)
	if message != nil && s.message != nil {
		s.message(message)
	}
	return message, err
}

func (s *skillSetupTaskProofOverrideStore) GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error) {
	action, err := s.skillSetupTaskTestStore.GetActionCall(ctx, scope, id)
	if action != nil && s.action != nil {
		s.action(action)
	}
	return action, err
}

func TestSkillSetupTaskResolutionRejectsForgedProvenance(t *testing.T) {
	for _, scenario := range []string{"run", "task", "source", "thread", "actor", "tenant", "action", "failed-action", "action-output", "wait-reference"} {
		t.Run(scenario, func(t *testing.T) {
			forSkillSetupTaskStores(t, func(t *testing.T, store skillSetupTaskTestStore, _ func() skillSetupTaskTestStore) {
				f := newSkillSetupTaskFixture(t, store)
				waiting := f.wait(t)
				f.resolve(t, "resolved")
				proof := &skillSetupTaskProofOverrideStore{skillSetupTaskTestStore: store}
				scope := waiting.Scope
				switch scenario {
				case "run":
					proof.request = func(request *SkillSetupRequest) { request.RunID = f.task.SourceRun.ID }
				case "task":
					proof.run = func(run *AgentRun) {
						if run.ID == waiting.ID {
							run.Context[ConversationTaskContextKey] = "other-task"
						}
					}
				case "source":
					proof.request = func(request *SkillSetupRequest) { request.TriggerMessageID = "other-source" }
				case "thread":
					proof.message = func(message *ChannelMessage) { message.ThreadRootID = "other-thread" }
				case "actor":
					proof.message = func(message *ChannelMessage) { message.Sender.ID = "other-user" }
				case "tenant":
					scope.ID = "other-tenant"
				case "action":
					proof.request = func(request *SkillSetupRequest) { request.ActionCallID = "other-action" }
				case "failed-action":
					proof.action = func(action *ActionCall) { action.Status = ActionCallStatusFailed }
				case "action-output":
					proof.action = func(action *ActionCall) {
						action.Output["setupRequest"].(map[string]interface{})["id"] = "other-request"
					}
				case "wait-reference":
					proof.run = func(run *AgentRun) {
						if run.ID == waiting.ID {
							run.WakeCondition.Reference = "other-request"
						}
					}
				}
				scheduler, err := NewConversationRunScheduler(proof, proof, ConversationRunSchedulerConfig{})
				if err != nil {
					t.Fatal(err)
				}
				result, _, _ := scheduler.ReconcileSkillSetupTask(t.Context(), scope, f.request.ID)
				if result != nil && result.Run != nil && result.Run.Status == AgentRunStatusQueued {
					t.Fatalf("forged %s resumed work: %#v", scenario, result)
				}
				unchanged, err := store.GetAgentRun(t.Context(), waiting.Scope, waiting.ID)
				if err != nil || !reflect.DeepEqual(unchanged, waiting) || skillSetupCompletionEvents(t, store, waiting) != 0 {
					t.Fatalf("forged %s mutated canonical work: %v", scenario, err)
				}
			})
		})
	}
}

func TestSkillSetupTaskWaitRequiresExactSuccessfulRequest(t *testing.T) {
	for _, scenario := range []string{"unknown-reference", "failed-action"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSkillSetupTaskFixture(t, NewMemoryStore())
			proof := &skillSetupTaskProofOverrideStore{skillSetupTaskTestStore: f.store}
			reference := f.request.ID
			if scenario == "unknown-reference" {
				reference = "skill-setup:unknown"
			} else {
				proof.action = func(action *ActionCall) { action.Status = ActionCallStatusFailed }
			}
			result, err := NewTurnCoordinator(proof, proof, proof).Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.request.Scope, RunID: f.request.RunID, WorkerID: "setup-worker"}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: ConversationTaskSkillSetupWakeType, Reference: reference}}, nil
			}))
			if err == nil || result != nil && result.Run != nil && result.Run.Status == AgentRunStatusWaitingForEvent {
				t.Fatalf("unproven setup wait accepted: %#v %v", result, err)
			}
		})
	}
}
