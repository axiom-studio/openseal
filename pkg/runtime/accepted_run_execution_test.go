package runtime

import (
	"errors"
	"reflect"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func acceptedMethodFixture(scope Scope) (*kernelagent.AgentDeployment, *kernelagent.AgentDefinition) {
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
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		catalog, scope, binding := runSkillDependencyFixture(t, store)
		deployment, definition := acceptedMethodFixture(scope)
		pin, err := NewAcceptedRunExecution(scope, deployment, definition, "manual")
		if err != nil {
			t.Fatal(err)
		}
		request := runSkillDependencyRequest(scope)
		request.Entrypoint = "manual"
		request.Context = map[string]interface{}{AcceptedRunExecutionContextKey: pin}
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		filter := SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "agent", BindingID: "account"}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("future Runbook action was not retained: %v %v", busy, err)
		}
		service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt new executable"}); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("upgrade invalidated future exact Runbook step: %v", err)
		}
		if _, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("cutover ignored future exact Runbook step: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
		}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("restart lost accepted method dependency: %v %v", busy, err)
		}
		changed := cloneAgentRun(run)
		changed.Revision++
		delete(changed.Context, AcceptedRunExecutionContextKey)
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, runSkillDependencyActivity(changed), nil); !errors.Is(err, ErrAcceptedRunExecution) {
			t.Fatalf("accepted immutable method was removable: %v", err)
		}
		stored, _ := store.GetAgentRun(t.Context(), scope, run.ID)
		if stored.Revision != run.Revision || !reflect.DeepEqual(stored.Context, cloneAgentRun(run).Context) {
			t.Fatalf("rejected snapshot mutation changed Run: %#v", stored)
		}
		if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Revoke access"}); err != nil {
			t.Fatal(err)
		}
		refs, err := store.(SkillRuntimeUsageStore).ListReferencedSkillRuntimeVersions(t.Context(), scope)
		if err != nil || !reflect.DeepEqual(refs, []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
			t.Fatalf("revocation discarded accepted continuation executable: %#v %v", refs, err)
		}
	})
}

func TestAcceptedRunExecutionResolverKeepsOriginalDefinitionAndChecksLiveGrants(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "accepted-method"}
	deployment, original := acceptedMethodFixture(scope)
	pin, err := NewAcceptedRunExecution(scope, deployment, original, "manual")
	if err != nil {
		t.Fatal(err)
	}
	latest := *original
	latest.Version = "2"
	latest.Runbook = &runbook.Definition{APIVersion: runbook.APIVersion, ID: "reader-method", Version: "2", Name: "New method", Entrypoints: map[string]string{"manual": "done"}, Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}}}
	deployment.ActiveVersion = "2"
	action := capability.ModelAction{Name: "reader.read", SkillID: "reader", Version: "1.0.0", Action: "read", BindingID: "account", BindingRevision: 1}
	catalog := &resolverCatalog{deployment: deployment, definitions: map[string]*kernelagent.AgentDefinition{"reader-agent@1": original, "reader-agent@2": &latest}, activation: &skill.ActivationSnapshot{SnapshotID: "live", Scope: skill.ScopeReference(scope), DeploymentID: "agent", Skills: []skill.ActivatedSkill{{SkillID: "reader", SkillVersion: "1.0.0", Actions: []capability.ModelAction{action}}}}}
	run := &AgentRun{ID: "accepted", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Entrypoint: "manual", Context: map[string]interface{}{AcceptedRunExecutionContextKey: pin}}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{})
	if err != nil || binding.DefinitionVersion != "1" || len(binding.ModelActions) != 1 {
		t.Fatalf("accepted method followed current deployment behavior: %#v %v", binding, err)
	}
	runner, ok := binding.Runner.(*RunbookTurnRunner)
	if !ok || runner.definition != original.Runbook {
		t.Fatalf("resolver did not recover exact accepted implementation: %#v", binding.Runner)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn", Sequence: 1}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusSleeping || len(outcome.ProposedActions) != 0 {
		t.Fatalf("original waiting method was silently replaced by latest ending method: %#v %v", outcome, err)
	}
	// Revocation changes the live projection independently of the method pin.
	catalog.activation = &skill.ActivationSnapshot{SnapshotID: "revoked", Scope: skill.ScopeReference(scope), DeploymentID: "agent"}
	binding, err = ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{})
	if err != nil || len(binding.ModelActions) != 0 || binding.DefinitionVersion != "1" {
		t.Fatalf("accepted method snapshot restored revoked authority: %#v %v", binding, err)
	}
	for _, contextValues := range []map[string]interface{}{
		{},
		{"runbookDefinitionId": "reader-method", "runbookDefinitionVersion": "1"},
	} {
		legacy := cloneAgentRun(run)
		legacy.Context = contextValues
		if _, err := ResolveCatalogTurnRunner(t.Context(), catalog, legacy, CatalogTurnResolverConfig{}); !errors.Is(err, ErrAcceptedRunExecution) {
			t.Fatalf("unknown/stale legacy continuation guessed current method: %v", err)
		}
	}
}

func TestAcceptedRunExecutionGraphTraversalIncludesBranchesAndDeduplicatesExactRefs(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "graph"}
	deployment, definition := acceptedMethodFixture(scope)
	definition.Runbook.Entrypoints["manual"] = "choose"
	definition.Runbook.Steps["choose"] = runbook.Step{Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{Next: "read"}}, Default: "alternate"}}
	definition.Runbook.Steps["alternate"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.1.0", Action: "read", Next: "read"}}
	definition.Runbook.Steps["unused"] = runbook.Step{Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "unreachable", SkillVersion: "9", Action: "read", Next: "done"}}
	pin, err := NewAcceptedRunExecution(scope, deployment, definition, "manual")
	if err != nil || !reflect.DeepEqual(pin.SkillDependencies, []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}, {SkillID: "reader", SkillVersion: "1.1.0"}}) {
		t.Fatalf("typed graph dependencies are incomplete or include arbitrary unreachable work: %#v %v", pin, err)
	}
	foreign := *pin
	foreign.Scope.ID = "another-tenant"
	if _, err := AcceptedRunExecutionForRun(&AgentRun{Scope: scope, AssignedAgentID: "agent", Entrypoint: "manual", Context: map[string]interface{}{AcceptedRunExecutionContextKey: &foreign}}); !errors.Is(err, ErrAcceptedRunExecution) {
		t.Fatalf("foreign accepted pin changed tenant routing: %v", err)
	}
}
