package openseal_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runbook"
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
		Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Visibility: openseal.ActivityVisibilityScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.CreateRunbookActivation(ctx, openseal.CreateRunbookActivationRequest{
		ID: "release-monitor", Scope: scope, Owner: objective.Owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "source-monitor", DefinitionVersion: "1", TriggerID: "releases",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 * * * * *", Timezone: "UTC"}, Entrypoint: "monitor"},
		Input:   map[string]interface{}{"projectId": "release-research", "sourceMonitorId": "releases", "url": "https://example.com/feed.xml", "maxItems": 3},
		Policy:  map[string]interface{}{"sourcePolicyRef": "public-feed@1"}, Budget: &openseal.BudgetPolicy{MaxAttempts: 2, MaxTurns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := engine.CreateProject(ctx, openseal.CreateProjectRequest{
		Project: &openseal.Project{
			ID: "release-research", Scope: scope, Owner: objective.Owner, Title: "Release research",
			Purpose: "Track releases", Status: openseal.ProjectStatusActive, ObjectiveRefs: []string{objective.ID},
			SourceMonitors: []openseal.ProjectSourceMonitorReference{{
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
	if plan.ApprovalRequired || len(plan.Objectives) != 0 || len(plan.Projects) != 1 || len(plan.Projects[0].MonitorIDs) != 1 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	receipt, err := engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
		Reason: "Adopt the reviewed source runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BindingRevision != binding.Revision+1 || len(receipt.ActivityIDs) != 1 {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	projectActivity, err := engine.ListActivityFeed(ctx, openseal.ActivityFeedRequest{
		Scope: scope, ProjectID: project.ID, EventTypes: []string{"project.skill_reference_upgraded"},
	})
	if err != nil || len(projectActivity.Items) != 1 {
		t.Fatalf("project upgrade activity=%#v err=%v", projectActivity, err)
	}
	updatedBinding, err := engine.GetSkillBinding(ctx, skillScope, "researcher", binding.ID)
	if err != nil || updatedBinding.SkillVersion != to.Version || updatedBinding.Revision != receipt.BindingRevision ||
		len(updatedBinding.Lifecycle) != 2 || updatedBinding.Lifecycle[1].Reason != "Adopt the reviewed source runtime" {
		t.Fatalf("binding=%#v err=%v", updatedBinding, err)
	}
	updatedProject, err := engine.GetProject(ctx, scope, project.ID)
	if err != nil || updatedProject.SourceMonitors[0].SkillVersion != to.Version {
		t.Fatalf("project=%#v err=%v", updatedProject, err)
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

func TestTeamSkillReferenceUpgradeRequiresTargetRoleAuthority(t *testing.T) {
	for _, test := range []struct {
		name          string
		includeTarget bool
		wantError     bool
	}{
		{name: "target grant ready", includeTarget: true},
		{name: "target grant missing", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			engine, err := openseal.New()
			if err != nil {
				t.Fatal(err)
			}
			from, to := clonedSourceDefinition(t, "5.0.0"), clonedSourceDefinition(t, "5.1.0")
			if err = engine.RegisterSkill(ctx, from); err != nil {
				t.Fatal(err)
			}
			if err = engine.RegisterSkill(ctx, to); err != nil {
				t.Fatal(err)
			}
			scope := openseal.SkillScope{Kind: "tenant", ID: "team-upgrade"}
			agentDefinition, err := engine.RegisterAgentDefinition(ctx, &openseal.AgentDefinition{
				ID: "researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Observe sources",
				SystemPrompt: "Observe carefully.",
				Authority:    openseal.AgentAuthorityPolicy{MaximumRisk: openseal.SkillRiskRead, MaxConcurrentRuns: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = engine.CreateAgentDeployment(ctx, &openseal.AgentDeployment{
				ID: "researcher-live", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
				RolloutStatus: openseal.AgentRolloutActive, Environment: "test",
				Capacity: openseal.AgentDeploymentCapacity{MaxConcurrentRuns: 1},
			}, "service", "test", "activate"); err != nil {
				t.Fatal(err)
			}
			grants := []openseal.TeamRoleSkillGrant{{
				SkillID: source.SkillID, SkillVersion: from.Version, AllowedActions: []string{source.ObserveFeed},
				MaximumRisk: openseal.SkillRiskRead,
			}}
			if test.includeTarget {
				grants = append(grants, openseal.TeamRoleSkillGrant{
					SkillID: source.SkillID, SkillVersion: to.Version, AllowedActions: []string{source.ObserveFeed},
					MaximumRisk: openseal.SkillRiskRead,
				})
			}
			teamDefinition, err := engine.RegisterTeamDefinition(ctx, &openseal.TeamDefinition{
				ID: "research-team", Version: "1.0.0", DisplayName: "Research Team", Purpose: "Coordinate research",
				Roles: []openseal.TeamRoleSlot{{
					ID: "researcher", DisplayName: "Researcher", Purpose: "Observe sources",
					MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{agentDefinition.ID},
					SkillGrants: grants, ChannelParticipation: openseal.TeamRoleChannelActive,
				}},
				Coordination: openseal.TeamCoordinationPolicy{},
				Approvals:    openseal.TeamApprovalPolicy{MaximumRisk: openseal.SkillRiskRead},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = engine.CreateTeamDeployment(ctx, &openseal.TeamDeployment{
				ID: "research-team-live", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
				Roster: []openseal.TeamRosterAssignment{{
					ID: "researcher", RoleID: "researcher", AgentDeploymentID: "researcher-live", DisplayName: "Researcher",
				}},
				Restrictions: openseal.TeamDeploymentRestrictions{MaximumRisk: openseal.SkillRiskRead, MaximumConcurrency: 1},
				Status:       openseal.TeamDeploymentActive,
			}, "service", "test", "activate"); err != nil {
				t.Fatal(err)
			}
			binding, err := engine.UpsertTeamSkillBinding(ctx, "research-team-live", openseal.UpsertSkillBindingRequest{
				Binding: &openseal.SkillBinding{
					ID: "source", Scope: scope, DeploymentID: "research-team-live", SkillID: source.SkillID,
					SkillVersion: from.Version, AllowedActions: []string{source.ObserveFeed}, MaximumRisk: openseal.SkillRiskRead,
				},
				Actor: openseal.SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Enable source",
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
				Scope: openseal.Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-team-live",
				BindingID: binding.ID, ToVersion: to.Version,
			})
			if test.wantError {
				if !errors.Is(err, openseal.ErrSkillReferenceUpgradeInvalid) {
					t.Fatalf("expected missing Team authority error, plan=%#v err=%v", plan, err)
				}
				return
			}
			if err != nil || plan.TeamAuthority == nil || plan.TeamAuthority.ExpectedRevision != 1 ||
				len(plan.TeamAuthority.AuthorizedRoleIDs) != 1 || plan.TeamAuthority.AuthorizedRoleIDs[0] != "researcher" {
				t.Fatalf("Team authority impact=%#v err=%v", plan, err)
			}
		})
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
