package runtime

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func acceptedMethodFixture(scope models.Scope) (*kernelagent.AgentDeployment, *kernelagent.AgentDefinition) {
	return &kernelagent.AgentDeployment{ID: "agent", Scope: skill.ScopeReference(scope), DefinitionID: "reader-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		&kernelagent.AgentDefinition{ID: "reader-agent", Version: "1", Purpose: "Read messages carefully", SystemPrompt: "Read authorized messages", Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "reader-method", Version: "1", Name: "Read later", Entrypoints: map[string]string{"manual": "wait"},
			Steps: map[string]runbook.Step{
				"wait": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Hour, Next: "read"}},
				"read": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.0.0", Action: "read", ResultPath: "/result", Next: "done"}},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		}}
}

func TestAcceptedRunExecutionRetainsFutureRunbookActionBeforeFirstCallAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog, scope, binding := runSkillDependencyFixture(t, store)
		deployment, definition := acceptedMethodFixture(scope)
		pin, err := models.NewAcceptedRunExecution(scope, deployment, definition, "manual")
		if err != nil {
			t.Fatal(err)
		}
		request := runSkillDependencyRequest(scope)
		request.Entrypoint = "manual"
		request.Context = map[string]interface{}{models.AcceptedRunExecutionContextKey: pin}
		run, err := portfolio.NewService(store).CreateAgentRun(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		filter := models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "agent", BindingID: "account"}
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("future Runbook action was not retained: %v %v", busy, err)
		}
		service := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
		plan, err := service.Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(t.Context(), skillbinding.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt new executable"}); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("upgrade invalidated future exact Runbook step: %v", err)
		}
		if _, err := store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("cutover ignored future exact Runbook step: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(models.KernelStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
		}
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("restart lost accepted method dependency: %v %v", busy, err)
		}
		changed := models.CloneAgentRun(run)
		changed.Revision++
		delete(changed.Context, models.AcceptedRunExecutionContextKey)
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, runSkillDependencyActivity(changed), nil); !errors.Is(err, models.ErrAcceptedRunExecution) {
			t.Fatalf("accepted immutable method was removable: %v", err)
		}
		stored, _ := store.GetAgentRun(t.Context(), scope, run.ID)
		if stored.Revision != run.Revision || !reflect.DeepEqual(stored.Context, models.CloneAgentRun(run).Context) {
			t.Fatalf("rejected snapshot mutation changed Run: %#v", stored)
		}
		if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Revoke access"}); err != nil {
			t.Fatal(err)
		}
		refs, err := store.(models.SkillRuntimeUsageStore).ListReferencedSkillRuntimeVersions(t.Context(), scope)
		if err != nil || !reflect.DeepEqual(refs, []models.SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
			t.Fatalf("revocation discarded accepted continuation executable: %#v %v", refs, err)
		}
	})
}

func TestAcceptedRunExecutionResolverKeepsOriginalDefinitionAndChecksLiveGrants(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "accepted-method"}
	deployment, original := acceptedMethodFixture(scope)
	pin, err := models.NewAcceptedRunExecution(scope, deployment, original, "manual")
	if err != nil {
		t.Fatal(err)
	}
	latest := *original
	latest.Version = "2"
	latest.Runbook = &runbook.Definition{APIVersion: runbook.APIVersion, ID: "reader-method", Version: "2", Name: "New method", Entrypoints: map[string]string{"manual": "done"}, Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}}}
	deployment.ActiveVersion = "2"
	action := capability.ModelAction{Name: "reader.read", SkillID: "reader", Version: "1.0.0", Action: "read", BindingID: "account", BindingRevision: 1}
	catalog := &bindingCatalog{deployment: deployment, definitions: map[string]*kernelagent.AgentDefinition{"reader-agent@1": original, "reader-agent@2": &latest}, activation: &skill.ActivationSnapshot{SnapshotID: "live", Scope: skill.ScopeReference(scope), DeploymentID: "agent", Skills: []skill.ActivatedSkill{{SkillID: "reader", SkillVersion: "1.0.0", Actions: []capability.ModelAction{action}}}}}
	run := &models.AgentRun{ID: "accepted", Scope: scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Entrypoint: "manual", Context: map[string]interface{}{models.AcceptedRunExecutionContextKey: pin}}
	binding, err := runworker.ResolveLoopBinding(t.Context(), catalog, run, bindingTestConfig(t, kerneltest.NewStore(t), nil))
	if err != nil || binding.DefinitionVersion != "1" || len(binding.ModelActions) != 1 {
		t.Fatalf("accepted method followed current deployment behavior: %#v %v", binding, err)
	}
	// The producer and the binding's runbook reference both come from the
	// accepted definition: version 1 of the method, not the current 2.
	producer, ok := binding.Producer.(*routine.RunbookStepProducer)
	if !ok || !slices.Contains(binding.InputContextRefs, "runbook:"+original.Runbook.ID+"@"+original.Runbook.Version) || slices.Contains(binding.InputContextRefs, "runbook:"+latest.Runbook.ID+"@"+latest.Runbook.Version) {
		t.Fatalf("resolver did not recover exact accepted implementation: %#v %v", binding.Producer, binding.InputContextRefs)
	}
	out, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step", Sequence: 1}, Binding: binding, Path: &models.AgentSessionPath{}})
	if err != nil || out.Park == nil || out.Park.Status != models.AgentRunStatusSleeping || len(out.ToolCalls) != 0 {
		t.Fatalf("original waiting method was silently replaced by latest ending method: %#v %v", out, err)
	}
	// Revocation changes the live projection independently of the method pin.
	catalog.activation = &skill.ActivationSnapshot{SnapshotID: "revoked", Scope: skill.ScopeReference(scope), DeploymentID: "agent"}
	binding, err = runworker.ResolveLoopBinding(t.Context(), catalog, run, bindingTestConfig(t, kerneltest.NewStore(t), nil))
	if err != nil || len(binding.ModelActions) != 0 || binding.DefinitionVersion != "1" {
		t.Fatalf("accepted method snapshot restored revoked authority: %#v %v", binding, err)
	}
	for _, contextValues := range []map[string]interface{}{
		{},
		{"runbookDefinitionId": "reader-method", "runbookDefinitionVersion": "1"},
	} {
		legacy := models.CloneAgentRun(run)
		legacy.Context = contextValues
		if _, err := runworker.ResolveLoopBinding(t.Context(), catalog, legacy, bindingTestConfig(t, kerneltest.NewStore(t), nil)); !errors.Is(err, models.ErrAcceptedRunExecution) {
			t.Fatalf("unknown/stale legacy continuation guessed current method: %v", err)
		}
	}
}

func TestAcceptedRunExecutionGraphTraversalIncludesBranchesAndDeduplicatesExactRefs(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "graph"}
	deployment, definition := acceptedMethodFixture(scope)
	definition.Runbook.Entrypoints["manual"] = "choose"
	definition.Runbook.Steps["choose"] = runbook.Step{Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{Next: "read"}}, Default: "alternate"}}
	definition.Runbook.Steps["alternate"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.1.0", Action: "read", Next: "read"}}
	definition.Runbook.Steps["unused"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "unreachable", SkillVersion: "9", Action: "read", Next: "done"}}
	pin, err := models.NewAcceptedRunExecution(scope, deployment, definition, "manual")
	if err != nil || !reflect.DeepEqual(pin.SkillDependencies, []models.SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}, {SkillID: "reader", SkillVersion: "1.1.0"}}) {
		t.Fatalf("typed graph dependencies are incomplete or include arbitrary unreachable work: %#v %v", pin, err)
	}
	foreign := *pin
	foreign.Scope.ID = "another-tenant"
	if _, err := models.AcceptedRunExecutionForRun(&models.AgentRun{Scope: scope, AssignedAgentID: "agent", Entrypoint: "manual", Context: map[string]interface{}{models.AcceptedRunExecutionContextKey: &foreign}}); !errors.Is(err, models.ErrAcceptedRunExecution) {
		t.Fatalf("foreign accepted pin changed tenant routing: %v", err)
	}
}
