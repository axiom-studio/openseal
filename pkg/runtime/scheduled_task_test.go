package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func scheduledTaskFixture(t *testing.T, store KernelStore, runbookDefinitions ...*skill.Definition) (*AgentRun, *skill.Catalog, *skill.BoundAction, map[string]interface{}) {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "task-test"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}
	reader := store.(ConversationStore)
	service := NewConversationService(reader)
	c, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Recurring research", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion, Content: "Check the public forecast every thirty minutes", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Handle request", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: c.ID, conversationRunContextTriggerID: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	registryStore, ok := store.(kernelagent.Store)
	if !ok {
		registryStore = kernelagent.NewMemoryStore()
	}
	registry := kernelagent.NewRegistryWithStore(registryStore)
	definition := sqliteAgentDefinition("1.0.0")
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "test"); err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalog()
	def := RunbookManagementSkill()
	if len(runbookDefinitions) == 1 {
		def = runbookDefinitions[0]
	}
	if err := catalog.Register(ctx, def); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "routines", Revision: 1, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: owner.ID, SkillID: def.ID, SkillVersion: def.Version, AllowedActions: []string{RunbookActionCreateTask, RunbookActionList, RunbookActionSetStatus, RunbookActionStart, RunbookActionReplaceSchedule}, MaximumRisk: skill.RiskLevelWrite}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, owner.ID, def.ID, def.Version, RunbookActionCreateTask)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Forecast update", "goal": "Use the browser to read the current public forecast and summarize it with its source.", "cron": "0 */30 * * * *", "timezone": "Asia/Kolkata", "maximumOccurrences": float64(2)}
	return run, catalog, bound, args
}
func dispatchTaskForTest(t *testing.T, store KernelStore, run *AgentRun, bound *skill.BoundAction, args map[string]interface{}, callID string) map[string]interface{} {
	t.Helper()
	d, err := NewRunbookActionDispatcher(store.(runbookActionStore), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: bound, Call: &ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	compiled := skill.NewCatalog()
	if err := compiled.Register(t.Context(), bound.Definition); err != nil {
		t.Fatal(err)
	}
	if err := compiled.ValidateOutput(t.Context(), bound, result); err != nil {
		t.Fatalf("result contract: %v", err)
	}
	return result
}
func TestScheduledTaskPersistsExecutesReportsAndRetiresAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store KernelStore = NewMemoryStore()
			if kind == "sqlite" {
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tasks.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				store = s
			}
			run, catalog, bound, args := scheduledTaskFixture(t, store)
			if err := catalog.ValidateInput(t.Context(), bound, args); err != nil {
				t.Fatal(err)
			}
			validator, _ := NewRunbookActionValidator(store.(runbookActionStore))
			if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err != nil {
				t.Fatal(err)
			}
			result := dispatchTaskForTest(t, store, run, bound, args, "create-task")
			a := result["activation"].(*RunbookActivation)
			if a.Task == nil || a.Task.ConversationID != run.Context[conversationRunContextConversationID] || a.NextRunAt == nil || a.DefinitionID != "" {
				t.Fatalf("activation=%#v", a)
			}
			replay := dispatchTaskForTest(t, store, run, bound, args, "create-task")
			if replay["activation"].(*RunbookActivation).ID != a.ID || replay["replayed"] != true {
				t.Fatal("duplicate task creation")
			}
			reader := store.(ConversationStore)
			for occurrence := 0; occurrence < 2; occurrence++ {
				current, _ := store.GetRunbookActivation(t.Context(), run.Scope, a.ID)
				now := current.NextRunAt.Add(time.Second)
				scheduler := NewRunbookScheduler(store)
				scheduler.now = func() time.Time { return now }
				r, err := scheduler.ReconcileScope(t.Context(), run.Scope, 20)
				if err != nil || r.Scheduled != 1 {
					t.Fatalf("schedule=%#v,%v", r, err)
				}
				// A different scheduler instance sees the durable cursor, never a private timer.
				if r, err := NewRunbookScheduler(store).ReconcileScope(t.Context(), run.Scope, 20); err != nil || r.Scheduled != 0 {
					t.Fatalf("restart replay=%#v,%v", r, err)
				}
				work, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, ObjectiveID: a.ObjectiveID, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				var scheduled *AgentRun
				for _, w := range work {
					if w.Status == AgentRunStatusQueued {
						scheduled = w
						break
					}
				}
				if scheduled == nil || scheduled.Goal != a.Task.Goal || scheduled.Source != RunSourceSchedule || scheduled.Entrypoint != "" || scheduled.Plan["runbook"] != nil {
					t.Fatalf("work=%#v", scheduled)
				}
				// Reporting projects the actual result into the original channel exactly once.
				scheduled.Status = AgentRunStatusCompleted
				scheduled.Output = map[string]interface{}{"summary": "Forecast result from the requested source"}
				scheduled.Revision++
				scheduled.UpdatedAt = now
				scheduled.CompletedAt = &now
				activity := NewRunActivityService(store, store)
				started, _, err := activity.TransitionRun(t.Context(), scheduled.Scope, scheduled.ID, RunTransitionRequest{ExpectedRevision: scheduled.Revision - 1, Status: AgentRunStatusRunning, Summary: "Execute scheduled task", Actor: ActivityActor{Type: "service", ID: "worker"}, Visibility: ActivityVisibilityScope})
				if err != nil {
					t.Fatal(err)
				}
				scheduled, _, err = activity.TransitionRun(t.Context(), started.Scope, started.ID, RunTransitionRequest{ExpectedRevision: started.Revision, Status: AgentRunStatusCompleted, Summary: "Finished task", Output: scheduled.Output, Actor: ActivityActor{Type: "service", ID: "worker"}, Visibility: ActivityVisibilityScope})
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					if err := projectTerminalRunReporting(t.Context(), reader, scheduled); err != nil {
						t.Fatal(err)
					}
				}
			}
			terminal, _ := store.GetRunbookActivation(t.Context(), run.Scope, a.ID)
			if terminal.Status != RunbookActivationRetired || terminal.OccurrencesProcessed != 2 {
				t.Fatalf("terminal=%#v", terminal)
			}
			messages, err := reader.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: a.Task.ConversationID, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			reports := 0
			for _, m := range messages {
				if m.Content == "Forecast result from the requested source" {
					reports++
				}
			}
			if reports != 2 {
				t.Fatalf("reports=%d", reports)
			}
			completion, ok := scheduledTaskConversationCompletion(run, conversationResultMap(result))
			if !ok || !strings.Contains(completion.Content, "active") {
				t.Fatalf("completion=%#v", completion)
			}
		})
	}
}
func TestScheduledTaskRejectsForeignAuthorityInvalidTimingAndBackgroundRecursion(t *testing.T) {
	store := NewMemoryStore()
	run, _, bound, args := scheduledTaskFixture(t, store)
	v, _ := NewRunbookActionValidator(store)
	for _, change := range []string{"scope", "owner", "background", "conversation", "binding", "cron", "timezone", "foreign-input"} {
		t.Run(change, func(t *testing.T) {
			candidate := cloneAgentRun(run)
			b := *bound
			binding := *bound.Binding
			b.Binding = &binding
			a := cloneMap(args)
			switch change {
			case "scope":
				candidate.Scope.ID = "foreign"
			case "owner":
				candidate.Owner.ID = "foreign"
			case "background":
				candidate.Kind = RunKindAgentWork
			case "conversation":
				candidate.Context[conversationRunContextConversationID] = "foreign"
			case "binding":
				binding.DeploymentID = "foreign"
			case "cron":
				a["cron"] = "0 * * * *"
			case "timezone":
				a["timezone"] = "Unknown/Zone"
			case "foreign-input":
				a["deploymentId"] = "foreign"
			}
			if _, err := v.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: candidate, Bound: &b, Arguments: a}); err == nil {
				t.Fatal("invalid task accepted")
			}
		})
	}
}
func TestScheduledTaskLifecycleRequiresExactRevisionAndReplaysItsOwnAction(t *testing.T) {
	store := NewMemoryStore()
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	a := dispatchTaskForTest(t, store, run, bound, args, "create")["activation"].(*RunbookActivation)
	statusBound, err := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionSetStatus)
	if err != nil {
		t.Fatal(err)
	}
	pause := map[string]interface{}{"activationId": a.ID, "expectedRevision": a.Revision, "status": "paused", "reason": "User paused"}
	result := dispatchTaskForTest(t, store, run, statusBound, pause, "pause")
	paused := result["activation"].(*RunbookActivation)
	if paused.Status != RunbookActivationPaused {
		t.Fatal("pause failed")
	}
	if replay := dispatchTaskForTest(t, store, run, statusBound, pause, "pause"); replay["replayed"] != true {
		t.Fatal("pause replay failed")
	}
	d, _ := NewRunbookActionDispatcher(store, nil)
	if _, err := d.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: statusBound, Call: &ActionCall{ID: "different", Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID}, Arguments: pause}); !errors.Is(err, ErrRunbookActivationRevision) {
		t.Fatalf("stale different action: %v", err)
	}
	resume := map[string]interface{}{"activationId": a.ID, "expectedRevision": paused.Revision, "status": "active", "reason": "User resumed"}
	active := dispatchTaskForTest(t, store, run, statusBound, resume, "resume")["activation"].(*RunbookActivation)
	if active.Status != RunbookActivationActive {
		t.Fatal("resume failed")
	}
	listBound, _ := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionList)
	listed := dispatchTaskForTest(t, store, run, listBound, map[string]interface{}{}, "list")
	if len(listed["activations"].([]*RunbookActivation)) != 1 {
		t.Fatal("list failed")
	}
	other := cloneAgentRun(run)
	other.Owner.ID = "foreign"
	if _, _, err := resolveOwnedTaskStatus(t.Context(), store, other, resume); err == nil {
		t.Fatal("foreign ownership accepted")
	}
	encoded, _ := json.Marshal(active)
	if !strings.Contains(string(encoded), `"task"`) {
		t.Fatal("task missing from durable projection")
	}
}

func TestScheduledTaskCreationUsesNormalApprovalBeforePersistence(t *testing.T) {
	store := NewMemoryStore()
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	now := time.Now().Add(time.Second)
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%#v,%v", claimed, err)
	}
	validator, _ := NewRunbookActionValidator(store)
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "Review recurring work", EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour}, nil
	}), validator)
	coordinator.now = func() time.Time { return now }
	result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: RunbookActionCreateTask, Arguments: args, IdempotencyKey: "user-schedule", Summary: "Create recurring research", Actor: ActivityActor{Type: "agent", ID: run.AssignedAgentID}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval == nil || result.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatal("creation bypassed approval")
	}
	values, err := store.ListRunbookActivations(t.Context(), RunbookActivationFilter{Scope: run.Scope})
	if err != nil || len(values) != 0 {
		t.Fatal("schedule executed before approval")
	}
	binding := *bound.Binding
	binding.ID = "read-only"
	binding.MaximumRisk = skill.RiskLevelRead
	binding.Revision++
	if err := catalog.Bind(t.Context(), &binding); err == nil {
		if _, err := catalog.Resolve(t.Context(), binding.Scope, binding.DeploymentID, binding.SkillID, binding.SkillVersion, RunbookActionCreateTask); err == nil {
			t.Fatal("read-only binding allowed schedule creation")
		}
	}
}

func TestScheduledTaskEachOccurrenceResolvesCurrentToolAuthority(t *testing.T) {
	store := NewMemoryStore()
	source, _, bound, args := scheduledTaskFixture(t, store)
	a := dispatchTaskForTest(t, store, source, bound, args, "create")["activation"].(*RunbookActivation)
	scheduler := NewRunbookScheduler(store)
	scheduler.now = func() time.Time { return a.NextRunAt.Add(time.Second) }
	if _, err := scheduler.ReconcileScope(t.Context(), source.Scope, 10); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, ObjectiveID: a.ObjectiveID, Limit: 10})
	run := runs[0]
	c := &resolverCatalog{deployment: &kernelagent.AgentDeployment{ID: run.AssignedAgentID, DefinitionID: "researcher", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}, definition: &kernelagent.AgentDefinition{ID: "researcher", Version: "1", Purpose: "Research", SystemPrompt: "Use current tools"}, activation: &skill.ActivationSnapshot{SnapshotID: "live", Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, DeploymentID: run.AssignedAgentID, Skills: []skill.ActivatedSkill{{SkillID: "browser", SkillVersion: "1", Actions: []capability.ModelAction{{Name: "browser.read", SkillID: "browser", Version: "1", Action: "read"}}}}}}
	host := &recordingTurnHost{response: &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "scheduled-turn", NextRunStatus: AgentRunStatusCompleted, ModelProvider: "test", Model: "test", OutputSummary: "Read result"}}
	b, err := ResolveCatalogTurnRunner(t.Context(), c, run, CatalogTurnResolverConfig{Host: host})
	if err != nil || len(b.ModelActions) != 1 {
		t.Fatalf("live authority=%#v,%v", b, err)
	}
	if _, ok := b.Runner.(*HostedTurnRunner); !ok {
		t.Fatalf("task did not use normal hosted runner: %T", b.Runner)
	}
	c.activation.Skills = nil // Revoked permission is not resurrected by persisted schedule.
	b, err = ResolveCatalogTurnRunner(t.Context(), c, run, CatalogTurnResolverConfig{Host: host})
	if err != nil || len(b.ModelActions) != 0 {
		t.Fatalf("revoked authority=%#v,%v", b, err)
	}
	c.deployment.RolloutStatus = kernelagent.RolloutPaused
	if _, err := ResolveCatalogTurnRunner(t.Context(), c, run, CatalogTurnResolverConfig{Host: host}); err == nil {
		t.Fatal("paused Agent executed task")
	}
}

func TestScheduledTaskSurvivesAgentDefinitionSynchronization(t *testing.T) {
	store := NewMemoryStore()
	run, _, bound, args := scheduledTaskFixture(t, store)
	first := dispatchTaskForTest(t, store, run, bound, args, "task-one")["activation"].(*RunbookActivation)
	second := dispatchTaskForTest(t, store, run, bound, args, "task-two")["activation"].(*RunbookActivation)
	// Both tasks share the portable schedule trigger name. An unrelated Agent
	// amendment must neither treat them as duplicate embedded triggers nor retire them.
	updates, err := reconcileAgentRunbookActivations([]*RunbookActivation{first, second}, sqliteAgentDefinition("2.0.0"), &kernelagent.AgentDeployment{ID: run.AssignedAgentID}, time.Now())
	if err != nil || len(updates) != 0 {
		t.Fatalf("amendment changed chat-authored tasks: %#v,%v", updates, err)
	}
}

func TestScheduledTaskSurvivesWorkforceProjectionReplacement(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, _, bound, args := scheduledTaskFixture(t, store)
	a := dispatchTaskForTest(t, store, run, bound, args, "task")["activation"].(*RunbookActivation)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	change := &authoring.ChangeSet{Scope: capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, Placement: authoring.ChangeSetPlacement{AgentDeploymentIDs: map[string]string{"definition": run.AssignedAgentID}}}
	if err := applySQLiteWorkforceRunbookActivations(t.Context(), tx, change, &workforceApplication{}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRunbookActivation(t.Context(), run.Scope, a.ID)
	if err != nil || current == nil || current.Status != RunbookActivationActive {
		t.Fatalf("workforce replacement removed task: %#v,%v", current, err)
	}
}
