package openseal_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
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
	blockedProject, err := engine.GetProject(ctx, scope, project.ID)
	if err != nil || blockedProject.SourceMonitors[0].SkillVersion != from.Version {
		t.Fatalf("busy upgrade changed project: %#v err=%v", blockedProject, err)
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
	for _, storeName := range []string{"memory", "sqlite"} {
		for _, test := range []struct {
			name          string
			includeTarget bool
			source        string
		}{
			{name: "identical target grant coalesced", includeTarget: true},
			{name: "missing target grant evolves automatically"},
			{name: "exact publisher preserved", source: "github.com/axiom-studio/skills"},
			{name: "exact publisher target coalesced", includeTarget: true, source: "github.com/axiom-studio/skills"},
		} {
			t.Run(storeName+"/"+test.name, func(t *testing.T) {
				engine, reopen := newTeamUpgradeEngine(t, storeName)
				fixture := createTeamSkillUpgradeFixture(t, engine, teamSkillUpgradeFixtureOptions{
					includeTarget: test.includeTarget, source: test.source,
				})
				ctx := context.Background()
				plan, err := fixture.plan(ctx)
				if err != nil || plan.TeamAuthority == nil {
					t.Fatalf("Team authority impact=%#v err=%v", plan, err)
				}
				impact := plan.TeamAuthority
				if plan.ApprovalRequired || impact.ExpectedRevision != fixture.deployment.Revision ||
					impact.DefinitionID != fixture.definition.ID || impact.DefinitionVersion != fixture.definition.Version ||
					impact.DefinitionDigest != fixture.definition.Digest || impact.TargetDefinitionVersion == "" ||
					impact.TargetDefinitionVersion == fixture.definition.Version || impact.TargetDefinitionDigest == "" ||
					!reflect.DeepEqual(impact.AuthorizedRoleIDs, []string{"researcher"}) {
					t.Fatalf("unexpected compatible Team evolution: %#v", plan)
				}
				fixture.assertCurrent(t, fixture.definition.Version, fixture.deployment.Revision, fixture.from.Version, fixture.binding.Revision, 1, 1)
				replanned, err := fixture.plan(ctx)
				if err != nil || replanned.Digest != plan.Digest || !reflect.DeepEqual(replanned.TeamAuthority, impact) {
					t.Fatalf("Team evolution plan is not deterministic: first=%#v next=%#v err=%v", plan, replanned, err)
				}
				receipt, err := engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
					Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"},
					Reason: "Adopt compatible source runtime",
				})
				if err != nil || receipt.BindingRevision != fixture.binding.Revision+1 {
					t.Fatalf("receipt=%#v err=%v", receipt, err)
				}
				fixture.assertCurrent(t, impact.TargetDefinitionVersion, fixture.deployment.Revision+1, fixture.to.Version, receipt.BindingRevision, 2, 2)
				updatedDefinition, err := engine.GetTeamDefinition(ctx, fixture.definition.ID, impact.TargetDefinitionVersion)
				if err != nil || updatedDefinition.Digest != impact.TargetDefinitionDigest {
					t.Fatalf("derived definition=%#v err=%v", updatedDefinition, err)
				}
				if len(updatedDefinition.Roles) != len(fixture.definition.Roles) || len(updatedDefinition.Roles[0].SkillGrants) != 1 {
					t.Fatalf("old and target grants were not coalesced: %#v", updatedDefinition.Roles)
				}
				if len(updatedDefinition.Roles[1].SkillGrants) != 0 {
					t.Fatalf("upgrade restored authority to an ungranted role: %#v", updatedDefinition.Roles[1])
				}
				updatedGrant := updatedDefinition.Roles[0].SkillGrants[0]
				expectedIdentity := openseal.NewSkillIdentity(fixture.to.ID, fixture.to.Version, test.source)
				if !updatedGrant.ExactIdentity().Equal(expectedIdentity) || updatedGrant.CatalogID != fixture.grant.CatalogID ||
					updatedGrant.EnablePrompt != fixture.grant.EnablePrompt || updatedGrant.MaximumRisk != fixture.grant.MaximumRisk ||
					!reflect.DeepEqual(updatedGrant.AllowedActions, fixture.grant.AllowedActions) {
					t.Fatalf("derived role authority changed: before=%#v after=%#v", fixture.grant, updatedGrant)
				}
				beforeBehavior, afterBehavior := *fixture.definition, *updatedDefinition
				beforeBehavior.Roles = append([]openseal.TeamRoleSlot(nil), beforeBehavior.Roles...)
				afterBehavior.Roles = append([]openseal.TeamRoleSlot(nil), afterBehavior.Roles...)
				for index := range beforeBehavior.Roles {
					beforeBehavior.Roles[index].SkillGrants = nil
					afterBehavior.Roles[index].SkillGrants = nil
				}
				afterBehavior.Version, afterBehavior.Digest, afterBehavior.CreatedAt = beforeBehavior.Version, beforeBehavior.Digest, beforeBehavior.CreatedAt
				if !reflect.DeepEqual(beforeBehavior, afterBehavior) {
					t.Fatalf("upgrade altered unrelated Team behavior: before=%#v after=%#v", beforeBehavior, afterBehavior)
				}
				historical, err := engine.GetTeamDefinition(ctx, fixture.definition.ID, fixture.definition.Version)
				if err != nil || !reflect.DeepEqual(historical, fixture.definition) {
					t.Fatalf("historical Team definition changed: before=%#v after=%#v err=%v", fixture.definition, historical, err)
				}
				activations, err := engine.ListTeamDefinitionActivations(ctx, fixture.scope, fixture.deployment.ID)
				if err != nil {
					t.Fatal(err)
				}
				var foundUpgrade bool
				for _, activation := range activations {
					if activation.FromVersion == fixture.definition.Version && activation.ToVersion == impact.TargetDefinitionVersion &&
						activation.DeploymentRevision == fixture.deployment.Revision+1 && activation.ActorType == "user" &&
						activation.ActorID == "operator" && activation.Reason == "Adopt compatible source runtime" {
						foundUpgrade = true
					}
				}
				if !foundUpgrade {
					t.Fatalf("missing Team definition activation: %#v", activations)
				}
				fixture.assertAgentActivation(t, fixture.to.Version, receipt.BindingRevision)
				if reopen != nil {
					fixture.engine = reopen()
					fixture.assertCurrent(t, impact.TargetDefinitionVersion, fixture.deployment.Revision+1, fixture.to.Version, receipt.BindingRevision, 2, 2)
					fixture.assertAgentActivation(t, fixture.to.Version, receipt.BindingRevision)
				}
			})
		}
	}
}

func TestTeamSkillReferenceUpgradeUsesExistingTargetAuthorityWithoutDefinitionChange(t *testing.T) {
	for _, storeName := range []string{"memory", "sqlite"} {
		t.Run(storeName, func(t *testing.T) {
			engine, _ := newTeamUpgradeEngine(t, storeName)
			fixture := createTeamSkillUpgradeFixture(t, engine, teamSkillUpgradeFixtureOptions{
				includeTarget: true, omitPrevious: true,
			})
			ctx := context.Background()
			plan, err := fixture.plan(ctx)
			if err != nil || plan.TeamAuthority == nil || plan.ApprovalRequired {
				t.Fatalf("preauthorized Team impact=%#v err=%v", plan, err)
			}
			impact := plan.TeamAuthority
			if impact.TargetDefinitionVersion != fixture.definition.Version || impact.TargetDefinitionDigest != fixture.definition.Digest ||
				!reflect.DeepEqual(impact.AuthorizedRoleIDs, []string{"researcher"}) {
				t.Fatalf("preauthorized Team definition should be retained: %#v", impact)
			}
			receipt, err := engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
				Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Reason: "Use existing exact grant",
			})
			if err != nil || receipt.BindingRevision != fixture.binding.Revision+1 {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
			fixture.assertCurrent(t, fixture.definition.Version, fixture.deployment.Revision, fixture.to.Version, receipt.BindingRevision, 1, 1)
			fixture.assertAgentActivation(t, fixture.to.Version, receipt.BindingRevision)
		})
	}
}

func TestTeamSkillReferenceUpgradeRejectsMissingOrChangedAuthority(t *testing.T) {
	for _, storeName := range []string{"memory", "sqlite"} {
		for _, test := range []struct {
			name    string
			options teamSkillUpgradeFixtureOptions
		}{
			{name: "old grant revoked", options: teamSkillUpgradeFixtureOptions{omitPrevious: true}},
			{name: "publisher changes", options: teamSkillUpgradeFixtureOptions{
				source: "github.com/axiom-studio/skills", targetSource: "github.com/other/skills",
			}},
			{name: "target prompt differs", options: teamSkillUpgradeFixtureOptions{
				includeTarget: true, changeTargetGrant: func(grant *openseal.TeamRoleSkillGrant) { grant.EnablePrompt = false },
			}},
			{name: "target actions differ", options: teamSkillUpgradeFixtureOptions{
				includeTarget: true, changeTargetGrant: func(grant *openseal.TeamRoleSkillGrant) { grant.AllowedActions = nil },
			}},
			{name: "target risk differs", options: teamSkillUpgradeFixtureOptions{
				includeTarget: true, changeTargetGrant: func(grant *openseal.TeamRoleSkillGrant) { grant.MaximumRisk = openseal.SkillRiskWrite },
			}},
			{name: "target catalog selection differs", options: teamSkillUpgradeFixtureOptions{
				includeTarget: true, changeTargetGrant: func(grant *openseal.TeamRoleSkillGrant) { grant.CatalogID = "other-listing" },
			}},
		} {
			t.Run(storeName+"/"+test.name, func(t *testing.T) {
				engine, _ := newTeamUpgradeEngine(t, storeName)
				fixture := createTeamSkillUpgradeFixture(t, engine, test.options)
				plan, err := fixture.plan(context.Background())
				if !errors.Is(err, openseal.ErrSkillReferenceUpgradeInvalid) {
					t.Fatalf("expected exact Team authority rejection, plan=%#v err=%v", plan, err)
				}
				fixture.assertCurrent(t, fixture.definition.Version, fixture.deployment.Revision, fixture.from.Version, fixture.binding.Revision, 1, 1)
			})
		}
	}
}

func TestTeamSkillReferenceUpgradeRejectsStaleDeploymentWithoutPartialChanges(t *testing.T) {
	for _, storeName := range []string{"memory", "sqlite"} {
		t.Run(storeName, func(t *testing.T) {
			engine, _ := newTeamUpgradeEngine(t, storeName)
			fixture := createTeamSkillUpgradeFixture(t, engine, teamSkillUpgradeFixtureOptions{})
			ctx := context.Background()
			plan, err := fixture.plan(ctx)
			if err != nil {
				t.Fatal(err)
			}
			concurrent := *fixture.deployment
			concurrent.Restrictions.MaximumConcurrency = 2
			updated, _, err := engine.UpdateTeamDeployment(ctx, &concurrent, fixture.deployment.Revision, "user", "operator", "Adjust concurrency")
			if err != nil {
				t.Fatal(err)
			}
			_, err = engine.ApplySkillReferenceUpgrade(ctx, openseal.ApplySkillReferenceUpgradeRequest{
				Plan: plan, Actor: openseal.ActivityActor{Type: "user", ID: "operator"}, Reason: "Use stale plan",
			})
			if !errors.Is(err, openseal.ErrSkillReferenceUpgradeConflict) {
				t.Fatalf("stale Team authority error=%v", err)
			}
			fixture.deployment = updated
			fixture.assertCurrent(t, fixture.definition.Version, updated.Revision, fixture.from.Version, fixture.binding.Revision, 1, 2)
			fixture.assertAgentActivation(t, fixture.from.Version, fixture.binding.Revision)
		})
	}
}

// newTeamUpgradeEngine exercises the public default Engine wiring separately
// from a shared durable store. The SQLite reopen also verifies upgrade state
// survives a complete store and Engine restart.
func newTeamUpgradeEngine(t *testing.T, storeName string) (*openseal.Engine, func() *openseal.Engine) {
	t.Helper()
	if storeName == "memory" {
		engine, err := openseal.New()
		if err != nil {
			t.Fatal(err)
		}
		return engine, nil
	}
	path := filepath.Join(t.TempDir(), "team-upgrade.db")
	store, err := openseal.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := openseal.New(openseal.WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	return engine, func() *openseal.Engine {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = openseal.NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := openseal.New(openseal.WithPersistentStore(store))
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}
}

type teamSkillUpgradeFixtureOptions struct {
	includeTarget     bool
	omitPrevious      bool
	source            string
	targetSource      string
	changeTargetGrant func(*openseal.TeamRoleSkillGrant)
}

type teamSkillUpgradeFixture struct {
	engine     *openseal.Engine
	scope      openseal.SkillScope
	from       *openseal.SkillDefinition
	to         *openseal.SkillDefinition
	binding    *openseal.SkillBinding
	definition *openseal.TeamDefinition
	deployment *openseal.TeamDeployment
	grant      openseal.TeamRoleSkillGrant
}

func createTeamSkillUpgradeFixture(t *testing.T, engine *openseal.Engine, options teamSkillUpgradeFixtureOptions) *teamSkillUpgradeFixture {
	t.Helper()
	ctx := context.Background()
	from, to := clonedSourceDefinition(t, "5.0.0"), clonedSourceDefinition(t, "5.1.0")
	targetSource := options.source
	if options.targetSource != "" {
		targetSource = options.targetSource
	}
	if options.source != "" {
		from.Source = &capability.SourceProvenance{Identity: options.source, Format: "test"}
	}
	if targetSource != "" {
		to.Source = &capability.SourceProvenance{Identity: targetSource, Format: "test"}
	}
	for _, definition := range []*openseal.SkillDefinition{from, to} {
		if err := engine.RegisterSkill(ctx, definition); err != nil {
			t.Fatal(err)
		}
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
	grant := openseal.TeamRoleSkillGrant{
		SkillID: source.SkillID, SkillVersion: from.Version, CatalogID: "source-observer-listing",
		AllowedActions: []string{source.ObserveFeed}, EnablePrompt: true, MaximumRisk: openseal.SkillRiskRead,
	}
	if options.source != "" {
		identity := openseal.NewSkillIdentity(from.ID, from.Version, options.source)
		grant.RuntimeIdentity = &identity
	}
	var grants []openseal.TeamRoleSkillGrant
	if !options.omitPrevious {
		grants = append(grants, grant)
	}
	if options.includeTarget {
		targetGrant := grant
		targetGrant.SkillVersion = to.Version
		if targetSource != "" {
			identity := openseal.NewSkillIdentity(to.ID, to.Version, targetSource)
			targetGrant.RuntimeIdentity = &identity
		}
		if options.changeTargetGrant != nil {
			options.changeTargetGrant(&targetGrant)
		}
		grants = append(grants, targetGrant)
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &openseal.TeamDefinition{
		ID: "research-team", Version: "1.0.0", DisplayName: "Research Team", Purpose: "Coordinate research",
		OperatingPrinciples: []string{"Preserve source provenance"},
		Roles: []openseal.TeamRoleSlot{{
			ID: "researcher", DisplayName: "Researcher", Purpose: "Observe sources",
			MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{agentDefinition.ID},
			SkillGrants: grants, ChannelParticipation: openseal.TeamRoleChannelActive,
		}, {
			ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review research without source access",
			MaximumMembers: 1, ChannelParticipation: openseal.TeamRoleChannelObserveOnly,
		}},
		Coordination: openseal.TeamCoordinationPolicy{QuietByDefault: true, SuppressDuplicateContent: true},
		Approvals:    openseal.TeamApprovalPolicy{MaximumRisk: openseal.SkillRiskWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamDeployment, _, err := engine.CreateTeamDeployment(ctx, &openseal.TeamDeployment{
		ID: "research-team-live", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Roster: []openseal.TeamRosterAssignment{{
			ID: "researcher", RoleID: "researcher", AgentDeploymentID: "researcher-live", DisplayName: "Researcher",
		}},
		Restrictions: openseal.TeamDeploymentRestrictions{MaximumRisk: openseal.SkillRiskRead, MaximumConcurrency: 1},
		Status:       openseal.TeamDeploymentActive,
	}, "service", "test", "activate")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := engine.UpsertTeamSkillBinding(ctx, teamDeployment.ID, openseal.UpsertSkillBindingRequest{
		Binding: &openseal.SkillBinding{
			ID: "source", Scope: scope, DeploymentID: teamDeployment.ID, SkillID: source.SkillID,
			SkillVersion: from.Version, SourceIdentity: options.source, AllowedActions: []string{source.ObserveFeed},
			EnablePrompt: true, MaximumRisk: openseal.SkillRiskRead,
		},
		Actor: openseal.SkillBindingActor{Type: "user", ID: "operator"}, Reason: "Enable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &teamSkillUpgradeFixture{
		engine: engine, scope: scope, from: from, to: to, binding: binding,
		definition: teamDefinition, deployment: teamDeployment, grant: grant,
	}
}

func (f *teamSkillUpgradeFixture) plan(ctx context.Context) (*openseal.SkillReferenceUpgradePlan, error) {
	var sourceIdentity string
	if f.to.Source != nil {
		sourceIdentity = f.to.Source.Identity
	}
	return f.engine.PlanSkillReferenceUpgrade(ctx, openseal.PlanSkillReferenceUpgradeRequest{
		Scope: openseal.Scope{Kind: f.scope.Kind, ID: f.scope.ID}, DeploymentID: f.deployment.ID,
		BindingID: f.binding.ID, ToVersion: f.to.Version, ToSourceIdentity: sourceIdentity,
	})
}

func (f *teamSkillUpgradeFixture) assertCurrent(t *testing.T, teamVersion string, teamRevision int64, skillVersion string, bindingRevision int64, definitionCount, activationCount int) {
	t.Helper()
	ctx := context.Background()
	deployment, err := f.engine.GetTeamDeployment(ctx, f.scope, f.deployment.ID)
	if err != nil || deployment.ActiveVersion != teamVersion || deployment.Revision != teamRevision ||
		deployment.DefinitionID != f.deployment.DefinitionID || deployment.Status != f.deployment.Status ||
		!reflect.DeepEqual(deployment.Roster, f.deployment.Roster) || !reflect.DeepEqual(deployment.Restrictions, f.deployment.Restrictions) {
		t.Fatalf("Team deployment=%#v err=%v", deployment, err)
	}
	binding, err := f.engine.GetSkillBinding(ctx, f.scope, f.deployment.ID, f.binding.ID)
	if err != nil || binding.SkillVersion != skillVersion || binding.Revision != bindingRevision ||
		binding.SourceIdentity != f.binding.SourceIdentity || binding.EnablePrompt != f.binding.EnablePrompt ||
		binding.MaximumRisk != f.binding.MaximumRisk || !reflect.DeepEqual(binding.AllowedActions, f.binding.AllowedActions) {
		t.Fatalf("Team binding=%#v err=%v", binding, err)
	}
	definitions, err := f.engine.ListTeamDefinitionVersions(ctx, f.definition.ID)
	if err != nil || len(definitions) != definitionCount {
		t.Fatalf("Team definitions=%#v err=%v", definitions, err)
	}
	activations, err := f.engine.ListTeamDefinitionActivations(ctx, f.scope, f.deployment.ID)
	if err != nil || len(activations) != activationCount {
		t.Fatalf("Team activations=%#v err=%v", activations, err)
	}
}

func (f *teamSkillUpgradeFixture) assertAgentActivation(t *testing.T, version string, bindingRevision int64) {
	t.Helper()
	activation, err := f.engine.ActivateTeamSkillsForAgent(context.Background(), f.scope, f.deployment.ID, "researcher-live", openseal.SkillHostCapabilityState{})
	if err != nil || len(activation.Skills) != 1 || len(activation.Unavailable) != 0 {
		t.Fatalf("Team roster Agent activation=%#v err=%v", activation, err)
	}
	activated := activation.Skills[0]
	if activated.SkillID != f.to.ID || activated.SkillVersion != version || activated.BindingRevision != bindingRevision ||
		activated.SourceIdentity != f.binding.SourceIdentity || activated.Prompt == nil || len(activated.Actions) != 1 ||
		activated.Actions[0].Action != source.ObserveFeed {
		t.Fatalf("Team roster Agent did not receive exact approved authority: %#v", activated)
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
