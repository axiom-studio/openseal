package openseal_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/source"
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
	from, to := clonedSourceDefinition(t, "1.0.3"), clonedSourceDefinition(t, "1.0.4")
	if err = engine.RegisterSkill(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, to); err != nil {
		t.Fatal(err)
	}
	scope := openseal.Scope{Kind: "tenant", ID: "one"}
	skillScope := openseal.SkillScope{Kind: scope.Kind, ID: scope.ID}
	binding, err := engine.UpsertSkillBinding(ctx, openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: skillScope, DeploymentID: "researcher", SkillID: source.SkillID,
			SkillVersion: from.Version, AllowedActions: []string{source.ObserveFeed}, EnablePrompt: true,
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
		Cadence: &openseal.ObjectiveCadence{
			Type: openseal.ObjectiveCadenceInterval, IntervalSeconds: 60, AssignedAgentID: "researcher",
			RunBudget: &openseal.BudgetPolicy{MaxAttempts: 2, MaxTurns: 2},
			RunTemplate: &openseal.ObjectiveRunTemplate{
				Context: map[string]interface{}{"initiativeId": "release-research", "sourceMonitorId": "releases"},
				Policy:  map[string]interface{}{"sourcePolicyRef": "public-feed@1"},
				Capability: &openseal.ObjectiveCapabilityInvocation{
					SkillID: source.SkillID, SkillVersion: from.Version, Action: source.ObserveFeed,
					Inputs: map[string]interface{}{"url": "https://example.com/feed.xml", "maxItems": 3},
				},
			},
		},
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	initiative, _, err := engine.CreateInitiative(ctx, openseal.CreateInitiativeRequest{
		Initiative: &openseal.Initiative{
			ID: "release-research", Scope: scope, Owner: objective.Owner, Title: "Release research",
			Purpose: "Track releases", Status: openseal.InitiativeStatusActive, ObjectiveRefs: []string{objective.ID},
			SourceMonitors: []openseal.InitiativeSourceMonitorReference{{
				ID: "releases", ObjectiveID: objective.ID, AssignedAgentID: "researcher",
				SkillID: source.SkillID, SkillVersion: from.Version, Action: source.ObserveFeed,
				SourcePolicyRef: "public-feed@1", Deduplication: openseal.SourceMonitorDeduplicateStableSourceAndContent,
			}},
		},
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	historical, err := engine.CreateAgentRun(ctx, openseal.CreateAgentRunRequest{
		Scope: scope, Kind: openseal.RunKindAgentWork, ObjectiveID: objective.ID, Owner: objective.Owner,
		AssignedAgentID: "researcher", Goal: "Historical source run", Source: openseal.RunSourceSchedule,
		Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{
			"skillId": source.SkillID, "skillVersion": from.Version, "action": source.ObserveFeed,
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
	if plan.ApprovalRequired || len(plan.Objectives) != 1 || len(plan.Initiatives) != 1 ||
		len(plan.Objectives[0].References) != 1 || len(plan.Initiatives[0].MonitorIDs) != 1 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	receipt, err := engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
		Reason: "Adopt the reviewed source runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BindingRevision != binding.Revision+1 || len(receipt.ActivityIDs) != 2 {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	objectiveActivity, err := engine.ListActivityFeed(ctx, openseal.ActivityFeedRequest{
		Scope: scope, ObjectiveID: objective.ID, EventTypes: []string{"objective.skill_reference_upgraded"},
	})
	if err != nil || len(objectiveActivity.Items) != 1 {
		t.Fatalf("objective upgrade activity=%#v err=%v", objectiveActivity, err)
	}
	initiativeActivity, err := engine.ListActivityFeed(ctx, openseal.ActivityFeedRequest{
		Scope: scope, InitiativeID: initiative.ID, EventTypes: []string{"initiative.skill_reference_upgraded"},
	})
	if err != nil || len(initiativeActivity.Items) != 1 {
		t.Fatalf("initiative upgrade activity=%#v err=%v", initiativeActivity, err)
	}
	updatedBinding, err := engine.GetSkillBinding(ctx, skillScope, "researcher", binding.ID)
	if err != nil || updatedBinding.SkillVersion != to.Version || updatedBinding.Revision != receipt.BindingRevision ||
		len(updatedBinding.Lifecycle) != 2 || updatedBinding.Lifecycle[1].Reason != "Adopt the reviewed source runtime" {
		t.Fatalf("binding=%#v err=%v", updatedBinding, err)
	}
	updatedObjective, err := engine.GetObjective(ctx, scope, objective.ID)
	if err != nil || updatedObjective.Cadence.RunTemplate.Capability.SkillVersion != to.Version {
		t.Fatalf("objective=%#v err=%v", updatedObjective, err)
	}
	updatedInitiative, err := engine.GetInitiative(ctx, scope, initiative.ID)
	if err != nil || updatedInitiative.SourceMonitors[0].SkillVersion != to.Version {
		t.Fatalf("initiative=%#v err=%v", updatedInitiative, err)
	}
	restoredHistory, err := engine.GetAgentRun(ctx, scope, historical.ID)
	if err != nil || restoredHistory.Context["capabilityInvocation"].(map[string]interface{})["skillVersion"] != from.Version {
		t.Fatalf("historical run changed: %#v err=%v", restoredHistory, err)
	}
}

func TestSkillReferenceUpgradeRejectsStaleImpactAndRequiresApprovalForContractChange(t *testing.T) {
	ctx := context.Background()
	engine, err := openseal.New()
	if err != nil {
		t.Fatal(err)
	}
	from, to := clonedSourceDefinition(t, "2.0.0"), clonedSourceDefinition(t, "3.0.0")
	action := to.Actions[source.ObserveFeed]
	action.Risk = openseal.SkillRiskWrite
	to.Actions[source.ObserveFeed] = action
	if err = engine.RegisterSkill(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, to); err != nil {
		t.Fatal(err)
	}
	scope := openseal.Scope{Kind: "tenant", ID: "approval"}
	skillScope := openseal.SkillScope{Kind: scope.Kind, ID: scope.ID}
	binding, err := engine.UpsertSkillBinding(ctx, openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: skillScope, DeploymentID: "researcher", SkillID: source.SkillID,
			SkillVersion: from.Version, AllowedActions: []string{source.ObserveFeed}, MaximumRisk: openseal.SkillRiskWrite,
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

func TestSkillReferenceUpgradeIncludesEventDrivenObjectivesAndRejectsStalePlans(t *testing.T) {
	ctx := context.Background()
	engine, err := openseal.New()
	if err != nil {
		t.Fatal(err)
	}
	from, to := clonedSourceDefinition(t, "4.0.0"), clonedSourceDefinition(t, "4.1.0")
	if err = engine.RegisterSkill(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err = engine.RegisterSkill(ctx, to); err != nil {
		t.Fatal(err)
	}
	scope := openseal.Scope{Kind: "tenant", ID: "events"}
	binding, err := engine.UpsertSkillBinding(ctx, openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: openseal.SkillScope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "watcher",
			SkillID: source.SkillID, SkillVersion: from.Version, AllowedActions: []string{source.ObserveFeed},
			MaximumRisk: openseal.SkillRiskRead,
		},
		Actor: openseal.SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Enable event observation",
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := openseal.ObjectiveEventRules{Version: "1", Rules: []openseal.ObjectiveEventRule{{
		ID: "release-event", EventType: "release.published", AssignedAgentID: "watcher",
		RunTemplate: &openseal.ObjectiveRunTemplate{Capability: &openseal.ObjectiveCapabilityInvocation{
			SkillID: source.SkillID, SkillVersion: from.Version, Action: source.ObserveFeed,
			Inputs: map[string]interface{}{"url": "https://example.com/releases.xml", "maxItems": 5},
		}},
	}}}
	encoded, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	eventRules := map[string]interface{}{}
	if err = json.Unmarshal(encoded, &eventRules); err != nil {
		t.Fatal(err)
	}
	objective, err := engine.CreateObjective(ctx, openseal.CreateObjectiveRequest{
		Scope: scope, Owner: openseal.ObjectiveOwner{Type: openseal.OwnerTypeAgent, ID: "watcher"},
		Title: "Observe releases", Goal: "React to release events", Status: openseal.ObjectiveStatusActive,
		Priority: 1, EventRules: eventRules, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
		Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
		Scope: scope, DeploymentID: "watcher", BindingID: binding.ID, ToVersion: to.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Objectives) != 1 || len(plan.Objectives[0].References) != 1 ||
		plan.Objectives[0].References[0].Kind != "event_rule" || plan.Objectives[0].References[0].ID != "release-event" {
		t.Fatalf("event reference missing from plan: %#v", plan)
	}
	changedGoal := "React to release events with a concise report"
	if _, err = engine.UpdateObjective(ctx, scope, objective.ID, openseal.UpdateObjectiveRequest{
		ExpectedRevision: objective.Revision, Goal: &changedGoal,
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Visibility: openseal.ActivityVisibilityScope,
		Summary: "Clarify the outcome",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Reason: "Apply stale plan",
	}); !errors.Is(err, openseal.ErrSkillReferenceUpgradeConflict) {
		t.Fatalf("expected stale plan conflict, got %v", err)
	}
	plan, err = engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
		Scope: scope, DeploymentID: "watcher", BindingID: binding.ID, ToVersion: to.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Reason: "Adopt reviewed event contract",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := engine.GetObjective(ctx, scope, objective.ID)
	if err != nil {
		t.Fatal(err)
	}
	updatedRules, err := openseal.DecodeObjectiveEventRules(updated.EventRules)
	if err != nil || updatedRules.Rules[0].RunTemplate.Capability.SkillVersion != to.Version {
		t.Fatalf("updated rules=%#v err=%v", updatedRules, err)
	}
}

func clonedSourceDefinition(t *testing.T, version string) *openseal.SkillDefinition {
	t.Helper()
	encoded, err := json.Marshal(source.SkillDefinition())
	if err != nil {
		t.Fatal(err)
	}
	var definition openseal.SkillDefinition
	if err = json.Unmarshal(encoded, &definition); err != nil {
		t.Fatal(err)
	}
	definition.Version = version
	return &definition
}
