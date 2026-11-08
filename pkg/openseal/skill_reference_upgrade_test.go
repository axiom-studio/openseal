package openseal_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	upgradeTestSkillID = "example.reader"
	upgradeTestAction  = "read_feed"
)

func TestSkillReferenceUpgradeMovesLiveReferencesAtomicallyAndPreservesHistory(t *testing.T) {
	engine, err := openseal.New()
	if err != nil {
		t.Fatal(err)
	}
	assertSkillReferenceUpgradeMovesLiveReferences(t, engine)

	store, err := openseal.NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	durable, err := openseal.New(openseal.WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	assertSkillReferenceUpgradeMovesLiveReferences(t, durable)
}

func assertSkillReferenceUpgradeMovesLiveReferences(t *testing.T, engine *openseal.Engine) {
	t.Helper()
	ctx := context.Background()
	var err error
	from, to := upgradeTestDefinition(t, "1.0.3"), upgradeTestDefinition(t, "1.0.4")
	if err = engine.RegisterSkill(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, to); err != nil {
		t.Fatal(err)
	}
	scope := openseal.Scope{Kind: "tenant", ID: "one"}
	skillScope := openseal.SkillScope{Kind: scope.Kind, ID: scope.ID}
	binding, err := engine.UpsertOwnerSkillBinding(ctx, upgradeTestAgent(t, engine, skillScope, "researcher"), openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: skillScope, DeploymentID: "researcher", SkillID: upgradeTestSkillID,
			SkillVersion: from.Version, AllowedActions: []string{upgradeTestAction}, EnablePrompt: true,
			MaximumRisk: openseal.SkillRiskRead,
		},
		Actor: openseal.SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Enable source monitoring",
	})
	if err != nil {
		t.Fatal(err)
	}
	objective, err := engine.CreateObjective(ctx, openseal.CreateObjectiveRequest{
		Scope: scope, Owner: openseal.ObjectiveOwner{Type: openseal.OwnerTypeAgent, ID: "researcher"},
		Title: "Monitor releases", Goal: "Observe release guidance", Status: openseal.ObjectiveStatusActive, Priority: 1,
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.CreateRunbookActivation(ctx, openseal.CreateRunbookActivationRequest{
		ID: "release-monitor", Scope: scope, Owner: objective.Owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "source-monitor", DefinitionVersion: "1", TriggerID: "releases",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 * * * * *", Timezone: "UTC"}, Entrypoint: "monitor"},
		Input:   map[string]interface{}{"url": "https://example.com/feed.xml", "maxItems": 3},
		Budget:  &openseal.BudgetPolicy{MaxAttempts: 2, MaxTurns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	historical, err := engine.CreateAgentRun(ctx, openseal.CreateAgentRunRequest{
		Scope: scope, Kind: openseal.RunKindAgentWork, ObjectiveID: objective.ID, Owner: objective.Owner,
		AssignedAgentID: "researcher", Goal: "Historical source run", Source: openseal.RunSourceSchedule,
		Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{
			"skillId": upgradeTestSkillID, "skillVersion": from.Version, "action": upgradeTestAction,
		}},
		Actor: openseal.ActivityActor{Type: "system", ID: "scheduler"}, Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
		Scope: scope, DeploymentID: "researcher", BindingID: binding.ID, ToVersion: to.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ApprovalRequired || len(plan.Objectives) != 0 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	request := openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
		Reason: "Adopt the reviewed source runtime",
	}
	if _, err = engine.ApplySkillReferenceUpgrade(ctx, request); !errors.Is(err, openseal.ErrSkillReferenceUpgradeBusy) {
		t.Fatalf("active queued invocation must retain its exact Skill version: err=%v", err)
	}
	blockedBinding, err := engine.GetSkillBinding(ctx, skillScope, "researcher", binding.ID)
	if err != nil || blockedBinding.SkillVersion != from.Version || blockedBinding.Revision != binding.Revision {
		t.Fatalf("busy upgrade changed binding: %#v err=%v", blockedBinding, err)
	}
	canceled, err := engine.CommandAgentRun(ctx, openseal.AgentRunCommandRequest{
		Scope: scope, RunID: historical.ID, ExpectedRevision: historical.Revision, Kind: openseal.AgentRunCommandCancel,
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Summary: "Retire the queued invocation before upgrading",
		Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil || canceled.Run.Status != openseal.AgentRunStatusCanceled {
		t.Fatalf("retiring queued invocation: %#v err=%v", canceled, err)
	}
	receipt, err := engine.ApplySkillReferenceUpgrade(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BindingRevision != binding.Revision+1 || len(receipt.ActivityIDs) != 0 {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	updatedBinding, err := engine.GetSkillBinding(ctx, skillScope, "researcher", binding.ID)
	if err != nil || updatedBinding.SkillVersion != to.Version || updatedBinding.Revision != receipt.BindingRevision ||
		len(updatedBinding.Lifecycle) != 2 || updatedBinding.Lifecycle[1].Reason != "Adopt the reviewed source runtime" {
		t.Fatalf("binding=%#v err=%v", updatedBinding, err)
	}
	restoredHistory, err := engine.GetAgentRun(ctx, scope, historical.ID)
	if err != nil || restoredHistory.Status != openseal.AgentRunStatusCanceled ||
		restoredHistory.Context["capabilityInvocation"].(map[string]interface{})["skillVersion"] != from.Version {
		t.Fatalf("historical run changed: %#v err=%v", restoredHistory, err)
	}
}

func TestSkillReferenceUpgradeRejectsStaleImpactAndRequiresApprovalForContractChange(t *testing.T) {
	ctx := context.Background()
	engine, err := openseal.New()
	if err != nil {
		t.Fatal(err)
	}
	from, to := upgradeTestDefinition(t, "2.0.0"), upgradeTestDefinition(t, "3.0.0")
	action := to.Actions[upgradeTestAction]
	action.Risk = openseal.SkillRiskWrite
	to.Actions[upgradeTestAction] = action
	if err = engine.RegisterSkill(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, to); err != nil {
		t.Fatal(err)
	}
	scope := openseal.Scope{Kind: "tenant", ID: "approval"}
	skillScope := openseal.SkillScope{Kind: scope.Kind, ID: scope.ID}
	binding, err := engine.UpsertOwnerSkillBinding(ctx, upgradeTestAgent(t, engine, skillScope, "researcher"), openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: skillScope, DeploymentID: "researcher", SkillID: upgradeTestSkillID,
			SkillVersion: from.Version, AllowedActions: []string{upgradeTestAction}, MaximumRisk: openseal.SkillRiskWrite,
		},
		Actor: openseal.SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Enable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
		Scope: scope, DeploymentID: "researcher", BindingID: binding.ID, ToVersion: to.Version,
	})
	if err != nil || !plan.ApprovalRequired {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	request := openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
		Reason: "Upgrade source",
	}
	if _, err = engine.ApplySkillReferenceUpgrade(ctx, request); !errors.Is(err, openseal.ErrSkillReferenceUpgradeApproval) {
		t.Fatalf("missing approval error=%v", err)
	}
	request.Approval = &openseal.SkillReferenceUpgradeApproval{
		Principal: openseal.ActivityActor{Type: "user", ID: "security-reviewer"}, Reason: "Reviewed the wider action risk",
	}
	if _, err = engine.ApplySkillReferenceUpgrade(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func upgradeTestDefinition(t *testing.T, version string) *openseal.SkillDefinition {
	t.Helper()
	return &skill.Definition{
		ID: upgradeTestSkillID, Version: version, Name: "Feed reader", Description: "Read a feed.",
		Transport: skill.TransportReference{Kind: "tool", Endpoint: upgradeTestSkillID},
		Actions: map[string]skill.Action{upgradeTestAction: {
			Name: upgradeTestAction, Description: "Read a feed.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"url"},
				"properties": map[string]interface{}{"url": map[string]interface{}{"type": "string"}},
			},
			OutputSchema: map[string]interface{}{"type": "object"},
			SideEffect:   skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		}},
		Prompt:       &capability.PromptModule{Instructions: "Read feeds.", UserInvocable: true, AllowedTools: []string{upgradeTestAction}},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
}

// upgradeTestAgent creates Agent deployment id so its bindings have an owner.
func upgradeTestAgent(t *testing.T, engine *openseal.Engine, scope openseal.SkillScope, id string) openseal.ObjectiveOwner {
	t.Helper()
	ctx := context.Background()
	if owner, err := engine.ResolveUmbrella(ctx, scope, id); err == nil {
		return owner
	}
	if _, err := engine.GetAgentDefinition(ctx, "upgrade-"+id, "1"); err != nil {
		if _, err := engine.RegisterAgentDefinition(ctx, &openseal.AgentDefinition{
			ID: "upgrade-" + id, Version: "1", DisplayName: "Test", Purpose: "Test", SystemPrompt: "Test.",
			Authority: openseal.AgentAuthorityPolicy{MaximumRisk: openseal.SkillRiskWrite, MaxConcurrentRuns: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &openseal.AgentDeployment{
		ID: id, Scope: scope, DefinitionID: "upgrade-" + id, ActiveVersion: "1", RolloutStatus: openseal.AgentRolloutActive,
		Environment: "local", Capacity: openseal.AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	return openseal.ObjectiveOwner{Type: openseal.OwnerTypeAgent, ID: id}
}
