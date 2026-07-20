package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type resolverCatalog struct {
	deployment *kernelagent.AgentDeployment
	definition *kernelagent.AgentDefinition
	activation *skill.ActivationSnapshot
	gotHost    skill.HostCapabilityState
}

func (c *resolverCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.deployment, nil
}

func (c *resolverCatalog) GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error) {
	return c.definition, nil
}

func (c *resolverCatalog) ActivateSkills(_ context.Context, _ skill.ScopeReference, _ string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	c.gotHost = host
	return c.activation, nil
}

func (c *resolverCatalog) GetOutreachThread(context.Context, Scope, string) (*OutreachThread, error) {
	return nil, ErrOutreachThreadNotFound
}

func (c *resolverCatalog) ReconcileOutreachAction(context.Context, Scope, string, ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	return nil, errors.New("not used")
}

func TestCatalogTurnResolverSelectsDeterministicKernelRunnersAndFailsClosedWithoutHost(t *testing.T) {
	scope := Scope{Kind: "local", ID: "research"}
	action := capability.ModelAction{
		Name: "forum.reply", BindingID: "forum-account", BindingRevision: 3,
		SkillID: "forum", Version: "1", Action: "reply", SideEffect: capability.SideEffectExternal,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "researcher", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "researcher", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "researcher", Version: "1", Purpose: "Research communities", SystemPrompt: "Be truthful."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-1", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", Skills: []skill.ActivatedSkill{{
			BindingID: "forum-account", BindingRevision: 3, SkillID: "forum", SkillVersion: "1", Name: "Forum", Actions: []capability.ModelAction{action},
		}}},
	}
	base := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Context: map[string]interface{}{}}

	outreach := cloneAgentRun(base)
	outreach.Context = map[string]interface{}{}
	outreach.Context[OutreachInvocationContextKey] = map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, outreach, CatalogTurnResolverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binding.Runner.(*OutreachTurnRunner); !ok || len(binding.ModelActions) != 1 || binding.ModelActions[0].BindingID != "forum-account" ||
		len(binding.InputContextRefs) != 2 || binding.InputContextRefs[0] != "skill-snapshot:snapshot-1" || binding.InputContextRefs[1] != "run:"+OutreachInvocationContextKey {
		t.Fatalf("outreach binding = %#v", binding)
	}

	invocation := cloneAgentRun(base)
	invocation.Context = map[string]interface{}{}
	invocation.Context["capabilityInvocation"] = map[string]interface{}{"skillId": "forum", "skillVersion": "1", "action": "reply"}
	binding, err = ResolveCatalogTurnRunner(t.Context(), catalog, invocation, CatalogTurnResolverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binding.Runner.(*CapabilityInvocationTurnRunner); !ok || len(binding.ModelActions) != 1 {
		t.Fatalf("capability binding = %#v", binding)
	}

	_, err = ResolveCatalogTurnRunner(t.Context(), catalog, base, CatalogTurnResolverConfig{})
	if !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("unhosted prompt work error = %v", err)
	}
}

func TestCatalogTurnResolverUsesDeploymentSpecificSkillHost(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "sre", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "sre", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "sre", Version: "1", Purpose: "Operate safely", SystemPrompt: "Use bounded tools."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-host", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "sre"},
	}
	wanted := skill.HostCapabilityState{OperatingSystem: "linux", Architecture: "arm64", Revision: "atlas-host/v1"}
	var resolvedScope skill.ScopeReference
	var resolvedDeployment string
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}, AssignedAgentID: "sre", Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{}}}
	_, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{SkillHosts: SkillHostCapabilityResolverFunc(func(_ context.Context, gotScope skill.ScopeReference, deploymentID string) (*skill.HostCapabilityState, error) {
		resolvedScope, resolvedDeployment = gotScope, deploymentID
		return &wanted, nil
	})})
	if err == nil || !strings.Contains(err.Error(), "requires authorized actions") {
		t.Fatalf("expected deterministic runner validation after host resolution, got %v", err)
	}
	if resolvedScope != (skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}) || resolvedDeployment != "sre" || !reflect.DeepEqual(catalog.gotHost, wanted) {
		t.Fatalf("scope=%#v deployment=%q host=%#v", resolvedScope, resolvedDeployment, catalog.gotHost)
	}
}
