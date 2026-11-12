package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// withScheduleArgument returns a copy of create_task arguments whose
// trigger.schedule sets key to value, or drops key when drop is true.
func withScheduleArgument(args map[string]interface{}, key string, value interface{}, drop bool) map[string]interface{} {
	result := maps.Clone(args)
	trigger := maps.Clone(result["trigger"].(map[string]interface{}))
	schedule := maps.Clone(trigger["schedule"].(map[string]interface{}))
	if drop {
		delete(schedule, key)
	} else {
		schedule[key] = value
	}
	trigger["schedule"] = schedule
	result["trigger"] = trigger
	return result
}

// routineActionStore is the store the routine actions need: they also read
// and write the chat.
type routineActionStore interface {
	models.KernelStore
	models.ConversationStore
}

func dispatchTaskForTest(t *testing.T, store models.KernelStore, run *models.AgentRun, bound *skill.BoundAction, args map[string]interface{}, callID string) map[string]interface{} {
	t.Helper()
	d, err := routine.NewRunbookActionDispatcher(store.(routineActionStore), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.DispatchAction(t.Context(), agentrun.ActionDispatchInput{Run: run, Bound: bound, Call: &models.ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: args})
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
	for _, kind := range []string{"sqlite"} {
		for _, limit := range []string{"bounded", "omitted", "null"} {
			t.Run(kind+"/"+limit, func(t *testing.T) {
				var store models.KernelStore = kerneltest.NewStore(t)
				if kind == "sqlite" {
					s, err := sqlite.New(filepath.Join(t.TempDir(), "tasks.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					store = s
				}
				run, catalog, bound, args := routinetest.ScheduledTask(t, store)
				occurrences, maximum, status := 2, int64(2), models.RunbookActivationRetired
				if limit != "bounded" {
					occurrences, maximum, status = 3, 0, models.RunbookActivationActive
					args = withScheduleArgument(args, "maximumOccurrences", nil, limit == "omitted")
				}
				if err := catalog.ValidateInput(t.Context(), bound, args); err != nil {
					t.Fatal(err)
				}
				validator, _ := routine.NewRunbookActionValidator(store.(routineActionStore))
				if _, err := validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err != nil {
					t.Fatal(err)
				}
				result := dispatchTaskForTest(t, store, run, bound, args, "create-task")
				a := result["activation"].(*models.RunbookActivation)
				if a.Task == nil || a.Task.ConversationID != run.Context[models.ConversationRunConversationIDContextKey] || a.NextRunAt == nil || a.DefinitionID != "" || a.Task.CreatedBy != "user" {
					t.Fatalf("activation=%#v", a)
				}
				if a.Trigger.Schedule == nil || a.Trigger.Schedule.MaximumOccurrences != maximum {
					t.Fatalf("persisted occurrence limit=%#v", a.Trigger.Schedule)
				}
				replay := dispatchTaskForTest(t, store, run, bound, args, "create-task")
				if replay["activation"].(*models.RunbookActivation).ID != a.ID || replay["replayed"] != true {
					t.Fatal("duplicate task creation")
				}
				reader := store.(models.ConversationStore)
				for occurrence := 0; occurrence < occurrences; occurrence++ {
					current, _ := store.GetRunbookActivation(t.Context(), run.Scope, a.ID)
					now := current.NextRunAt.Add(time.Second)
					scheduler := routine.NewRunbookScheduler(store, models.WithClock(func() time.Time { return now }))
					r, err := scheduler.ReconcileScope(t.Context(), run.Scope, 20)
					if err != nil || r.Scheduled != 1 {
						t.Fatalf("schedule=%#v,%v", r, err)
					}
					// A different scheduler instance sees the durable cursor, never a private timer.
					if r, err := routine.NewRunbookScheduler(store).ReconcileScope(t.Context(), run.Scope, 20); err != nil || r.Scheduled != 0 {
						t.Fatalf("restart replay=%#v,%v", r, err)
					}
					work, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: run.Scope, ObjectiveID: a.ObjectiveID, Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					var scheduled *models.AgentRun
					for _, w := range work {
						if w.Status == models.AgentRunStatusQueued {
							scheduled = w
							break
						}
					}
					// Every occurrence acts for the person who created the routine.
					if scheduled == nil || scheduled.Goal != a.Task.Goal || scheduled.Source != models.RunSourceSchedule || scheduled.Entrypoint != "" || scheduled.Plan["runbook"] != nil || scheduled.ActingUser != "user" {
						t.Fatalf("work=%#v", scheduled)
					}
					// Reporting projects the actual result into the original channel exactly once.
					scheduled.Status = models.AgentRunStatusCompleted
					scheduled.Output = map[string]interface{}{"summary": "Forecast result from the requested source"}
					scheduled.Revision++
					scheduled.UpdatedAt = now
					scheduled.CompletedAt = &now
					activity := runtimeactivity.NewRunActivityService(store, store)
					started, _, err := activity.TransitionRun(t.Context(), scheduled.Scope, scheduled.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: scheduled.Revision - 1, Status: models.AgentRunStatusRunning, Summary: "Execute scheduled task", Actor: models.ActivityActor{Type: "service", ID: "worker"}, Visibility: models.ActivityVisibilityScope})
					if err != nil {
						t.Fatal(err)
					}
					scheduled, _, err = activity.TransitionRun(t.Context(), started.Scope, started.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: started.Revision, Status: models.AgentRunStatusCompleted, Summary: "Finished task", Output: scheduled.Output, Actor: models.ActivityActor{Type: "service", ID: "worker"}, Visibility: models.ActivityVisibilityScope})
					if err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 2; i++ {
						if err := conversationrun.ProjectTerminalRunReporting(t.Context(), reader, scheduled); err != nil {
							t.Fatal(err)
						}
					}
				}
				terminal, _ := store.GetRunbookActivation(t.Context(), run.Scope, a.ID)
				if terminal.Status != status || terminal.OccurrencesProcessed != int64(occurrences) || (maximum == 0 && terminal.NextRunAt == nil) {
					t.Fatalf("terminal=%#v", terminal)
				}
				messages, err := reader.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: run.Scope, ConversationID: a.Task.ConversationID, Limit: 50})
				if err != nil {
					t.Fatal(err)
				}
				reports := 0
				for _, m := range messages {
					if m.Content == "Forecast result from the requested source" {
						reports++
					}
				}
				if reports != occurrences {
					t.Fatalf("reports=%d", reports)
				}
			})
		}
	}
}
func TestScheduledTaskRejectsForeignAuthorityInvalidTimingAndBackgroundRecursion(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, _, bound, args := routinetest.ScheduledTask(t, store)
	v, _ := routine.NewRunbookActionValidator(store)
	for _, change := range []string{"scope", "owner", "background", "conversation", "binding", "cron", "timezone", "foreign-input"} {
		t.Run(change, func(t *testing.T) {
			candidate := models.CloneAgentRun(run)
			b := *bound
			binding := *bound.Binding
			b.Binding = &binding
			a := maps.Clone(args)
			switch change {
			case "scope":
				candidate.Scope.ID = "foreign"
			case "owner":
				candidate.Owner.ID = "foreign"
			case "background":
				candidate.Kind = models.RunKindAgentWork
			case "conversation":
				candidate.Context[models.ConversationRunConversationIDContextKey] = "foreign"
			case "binding":
				binding.DeploymentID = "foreign"
			case "cron":
				a = withScheduleArgument(a, "cron", "0 * * * *", false)
			case "timezone":
				a = withScheduleArgument(a, "timezone", "Unknown/Zone", false)
			case "foreign-input":
				a["deploymentId"] = "foreign"
			}
			if _, err := v.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: candidate, Bound: &b, Arguments: a}); err == nil {
				t.Fatal("invalid task accepted")
			}
		})
	}
}

func TestScheduledTaskRejectsInvalidLimitsBeforePersistence(t *testing.T) {
	for _, kind := range []string{"sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := routinetest.NewFileStore(t, kind)
			run, catalog, bound, args := routinetest.ScheduledTask(t, store)
			now := time.Now().Add(time.Second)
			claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%#v,%v", claimed, err)
			}
			validator, _ := routine.NewRunbookActionValidator(store.(routineActionStore))
			policyCalls := 0
			coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
				policyCalls++
				return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
			}), []action.ActionProposalValidator{validator}, models.WithClock(func() time.Time { return now }))
			invalid := []struct {
				name  string
				value interface{}
			}{
				{"zero", 0}, {"negative", -1}, {"fraction", 1.5}, {"above-maximum", 1000001},
				{"string", "2"}, {"boolean", true}, {"array", []interface{}{}}, {"object", map[string]interface{}{}},
			}
			for _, value := range invalid {
				t.Run(value.name, func(t *testing.T) {
					candidate := withScheduleArgument(args, "maximumOccurrences", value.value, false)
					if err := catalog.ValidateInput(t.Context(), bound, candidate); err == nil {
						t.Fatal("invalid occurrence limit accepted by the tool contract")
					}
					if _, err := coordinator.Propose(t.Context(), action.ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: models.RunbookActionCreateTask, Arguments: candidate, IdempotencyKey: value.name, Summary: "Create recurring research"}); err == nil {
						t.Fatal("invalid occurrence limit persisted")
					}
				})
			}
			for _, required := range []string{"title", "goal", "trigger", "brief"} {
				candidate := maps.Clone(args)
				candidate[required] = nil
				if err := catalog.ValidateInput(t.Context(), bound, candidate); err == nil {
					t.Fatalf("required %s became nullable", required)
				}
			}
			for _, required := range []string{"cron", "timezone"} {
				if err := catalog.ValidateInput(t.Context(), bound, withScheduleArgument(args, required, nil, false)); err == nil {
					t.Fatalf("required %s became nullable", required)
				}
			}
			if policyCalls != 0 {
				t.Fatal("invalid limits reached action policy")
			}
			if values, err := store.ListRunbookActivations(t.Context(), models.RunbookActivationFilter{Scope: run.Scope}); err != nil || len(values) != 0 {
				t.Fatalf("invalid input created a routine: %#v, %v", values, err)
			}
			if values, err := store.ListObjectives(t.Context(), models.ObjectiveFilter{Scope: run.Scope}); err != nil || len(values) != 0 {
				t.Fatalf("invalid input created an objective: %#v, %v", values, err)
			}
		})
	}
}
func TestScheduledTaskLifecycleRequiresExactRevisionAndReplaysItsOwnAction(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, catalog, bound, args := routinetest.ScheduledTask(t, store)
	a := dispatchTaskForTest(t, store, run, bound, args, "create")["activation"].(*models.RunbookActivation)
	statusBound, err := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, models.RunbookActionSetStatus)
	if err != nil {
		t.Fatal(err)
	}
	pause := map[string]interface{}{"activationId": a.ID, "expectedRevision": a.Revision, "status": "paused", "reason": "User paused"}
	result := dispatchTaskForTest(t, store, run, statusBound, pause, "pause")
	paused := result["activation"].(*models.RunbookActivation)
	if paused.Status != models.RunbookActivationPaused {
		t.Fatal("pause failed")
	}
	if replay := dispatchTaskForTest(t, store, run, statusBound, pause, "pause"); replay["replayed"] != true {
		t.Fatal("pause replay failed")
	}
	d, _ := routine.NewRunbookActionDispatcher(store, nil)
	if _, err := d.DispatchAction(t.Context(), agentrun.ActionDispatchInput{Run: run, Bound: statusBound, Call: &models.ActionCall{ID: "different", Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID}, Arguments: pause}); !errors.Is(err, models.ErrRunbookActivationRevision) {
		t.Fatalf("stale different action: %v", err)
	}
	resume := map[string]interface{}{"activationId": a.ID, "expectedRevision": paused.Revision, "status": "active", "reason": "User resumed"}
	active := dispatchTaskForTest(t, store, run, statusBound, resume, "resume")["activation"].(*models.RunbookActivation)
	if active.Status != models.RunbookActivationActive {
		t.Fatal("resume failed")
	}
	listBound, _ := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, models.RunbookActionList)
	listed := dispatchTaskForTest(t, store, run, listBound, map[string]interface{}{}, "list")
	if len(listed["activations"].([]*models.RunbookActivation)) != 1 {
		t.Fatal("list failed")
	}
	other := models.CloneAgentRun(run)
	other.Owner.ID = "foreign"
	validator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: other, Bound: statusBound, Arguments: resume}); err == nil {
		t.Fatal("foreign ownership accepted")
	}
	// A Team chat with the Agent on its roster passes the roster check and
	// still does not own the Agent's routine.
	teamChat := models.CloneAgentRun(run)
	teamChat.Owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}
	validator.SetRoutineTeams(scheduledTaskTeams{team: &kernelteam.Deployment{
		ID: "team", Scope: capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: run.AssignedAgentID, AgentDeploymentID: run.AssignedAgentID}},
	}})
	if _, err := validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: teamChat, Bound: statusBound, Arguments: resume}); err == nil || !strings.Contains(err.Error(), "not owned by this chat") {
		t.Fatalf("foreign Team chat ownership accepted: %v", err)
	}
	encoded, _ := json.Marshal(active)
	if !strings.Contains(string(encoded), `"task"`) {
		t.Fatal("task missing from durable projection")
	}
}

func TestScheduledTaskCreationUsesNormalApprovalBeforePersistence(t *testing.T) {
	for _, limit := range []string{"bounded", "omitted", "null"} {
		t.Run(limit, func(t *testing.T) {
			store := kerneltest.NewStore(t)
			run, catalog, bound, args := routinetest.ScheduledTask(t, store)
			if limit != "bounded" {
				args = withScheduleArgument(args, "maximumOccurrences", nil, limit == "omitted")
			}
			now := time.Now().Add(time.Second)
			claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%#v,%v", claimed, err)
			}
			validator, _ := routine.NewRunbookActionValidator(store)
			coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
				return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, Reason: "Review recurring work", EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: time.Hour}, nil
			}), []action.ActionProposalValidator{validator}, models.WithClock(func() time.Time { return now }))
			result, err := coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: models.RunbookActionCreateTask, Arguments: args, IdempotencyKey: "user-schedule", Summary: "Create recurring research", Actor: models.ActivityActor{Type: "agent", ID: run.AssignedAgentID}}, "create-task"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Approval == nil || result.Call.Status != models.ActionCallStatusWaitingApproval {
				t.Fatal("creation bypassed approval")
			}
			values, err := store.ListRunbookActivations(t.Context(), models.RunbookActivationFilter{Scope: run.Scope})
			if err != nil || len(values) != 0 {
				t.Fatal("schedule executed before approval")
			}
			binding := *bound.Binding
			binding.ID = "read-only"
			binding.MaximumRisk = skill.RiskLevelRead
			binding.Revision++
			if err := catalog.Bind(t.Context(), &binding); err == nil {
				if _, err := catalog.Resolve(t.Context(), binding.Scope, binding.DeploymentID, binding.SkillID, binding.SkillVersion, models.RunbookActionCreateTask); err == nil {
					t.Fatal("read-only binding allowed schedule creation")
				}
			}
		})
	}
}

// TestRoutineBriefIsDecidedByTheRequester: a routine's brief names the person
// who asked for it as its only approver, whatever role the host policy named,
// so a member accepts their own routine in any space they belong to.
func TestRoutineBriefIsDecidedByTheRequester(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, catalog, bound, args := routinetest.ScheduledTask(t, store)
	now := time.Now().Add(time.Second)
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: run.AssignedAgentID, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%#v,%v", claimed, err)
	}
	validator, _ := routine.NewRunbookActionValidator(store)
	policy := action.NewDefaultActionPolicy()
	policy.Approvers = []models.ApprovalPrincipal{{Type: "role", ID: "operator"}}
	coordinator := action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator}, models.WithClock(func() time.Time { return now }))
	result, err := coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: bound.Definition.ID, SkillVersion: bound.Definition.Version, Action: models.RunbookActionCreateTask, Arguments: args, IdempotencyKey: "requester-routine", Summary: "Create recurring research", Actor: models.ActivityActor{Type: "agent", ID: run.AssignedAgentID}}, "create-task"))
	if err != nil {
		t.Fatal(err)
	}
	want := []models.ApprovalPrincipal{{Type: "user", ID: "user"}}
	if result.Approval == nil || !reflect.DeepEqual(result.Approval.EligibleApprovers, want) {
		t.Fatalf("approvers = %#v, want %#v", result.Approval, want)
	}
}

func TestScheduledTaskEachOccurrenceResolvesCurrentToolAuthority(t *testing.T) {
	store := kerneltest.NewStore(t)
	source, _, bound, args := routinetest.ScheduledTask(t, store)
	a := dispatchTaskForTest(t, store, source, bound, args, "create")["activation"].(*models.RunbookActivation)
	scheduler := routine.NewRunbookScheduler(store, models.WithClock(func() time.Time { return a.NextRunAt.Add(time.Second) }))
	if _, err := scheduler.ReconcileScope(t.Context(), source.Scope, 10); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: source.Scope, ObjectiveID: a.ObjectiveID, Limit: 10})
	run := runs[0]
	c := &bindingCatalog{deployment: &kernelagent.AgentDeployment{ID: run.AssignedAgentID, DefinitionID: "researcher", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}, definition: &kernelagent.AgentDefinition{ID: "researcher", Version: "1", Purpose: "Research", SystemPrompt: "Use current tools"}, activation: &skill.ActivationSnapshot{SnapshotID: "live", Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, DeploymentID: run.AssignedAgentID, Skills: []skill.ActivatedSkill{{SkillID: "browser", SkillVersion: "1", Actions: []capability.ModelAction{{Name: "browser.read", SkillID: "browser", Version: "1", Action: "read"}}}}}}
	config := bindingTestConfig(t, store, &modelsteptest.RecordingStepHost{})
	b, err := runworker.ResolveLoopBinding(t.Context(), c, run, config)
	if err != nil || len(b.ModelActions) != 1 {
		t.Fatalf("live authority=%#v,%v", b, err)
	}
	if _, ok := b.Producer.(*modelstep.ModelStepProducer); !ok {
		t.Fatalf("task did not use the model step producer: %T", b.Producer)
	}
	c.activation.Skills = nil // Revoked permission is not resurrected by persisted schedule.
	b, err = runworker.ResolveLoopBinding(t.Context(), c, run, config)
	if err != nil || len(b.ModelActions) != 0 {
		t.Fatalf("revoked authority=%#v,%v", b, err)
	}
	c.deployment.RolloutStatus = kernelagent.RolloutPaused
	if _, err := runworker.ResolveLoopBinding(t.Context(), c, run, config); err == nil {
		t.Fatal("paused Agent executed task")
	}
}

func TestScheduledTaskSurvivesAgentDefinitionSynchronization(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, _, bound, args := routinetest.ScheduledTask(t, store)
	first := dispatchTaskForTest(t, store, run, bound, args, "task-one")["activation"].(*models.RunbookActivation)
	second := dispatchTaskForTest(t, store, run, bound, args, "task-two")["activation"].(*models.RunbookActivation)
	// Both tasks share the portable schedule trigger name. An unrelated Agent
	// amendment must neither treat them as duplicate embedded triggers nor retire them.
	updates, err := models.ReconcileAgentRunbookActivations([]*models.RunbookActivation{first, second}, sqliteAgentDefinition("2.0.0"), &kernelagent.AgentDeployment{ID: run.AssignedAgentID}, time.Now())
	if err != nil || len(updates) != 0 {
		t.Fatalf("amendment changed chat-authored tasks: %#v,%v", updates, err)
	}
}

// scheduledTaskTeams serves one Team deployment to the routine roster check.
type scheduledTaskTeams struct{ team *kernelteam.Deployment }

func (c scheduledTaskTeams) GetDeployment(_ context.Context, _ skill.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if c.team == nil || c.team.ID != id {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return c.team, nil
}
