package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeStoreRollsBackEveryResourceOnConflict(t *testing.T) {
	tests := map[string]func(*testing.T) SkillReferenceUpgradeStore{
		"memory": func(*testing.T) SkillReferenceUpgradeStore {
			return NewMemoryStore()
		},
		"sqlite": func(t *testing.T) SkillReferenceUpgradeStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, factory := range tests {
		t.Run(name, func(t *testing.T) {
			assertSkillReferenceUpgradeRollback(t, factory(t))
		})
	}
}

func assertSkillReferenceUpgradeRollback(t *testing.T, store SkillReferenceUpgradeStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	scope := Scope{Kind: "tenant", ID: "atomic"}
	bindingScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	binding := &skill.Binding{
		ID: "source", Scope: bindingScope, DeploymentID: "researcher", SkillID: "source",
		SkillVersion: "1.0.0", AllowedActions: []string{"observe"}, MaximumRisk: skill.RiskLevelRead,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveSkillBinding(ctx, binding, 0); err != nil {
		t.Fatal(err)
	}
	objective := &Objective{
		ID: "objective", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		Title: "Observe", Goal: "Observe safely", Status: ObjectiveStatusActive, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateObjective(ctx, objective); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID: "project", Scope: scope, Owner: objective.Owner, Title: "Research", Purpose: "Coordinate research",
		Status: ProjectStatusActive, ObjectiveRefs: []string{objective.ID}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	nextBinding := cloneUpgradeBinding(binding)
	nextBinding.SkillVersion = "1.1.0"
	nextBinding.Revision = 2
	nextBinding.UpdatedAt = now.Add(time.Minute)
	nextObjective := cloneObjective(objective)
	nextObjective.Revision = 2
	nextObjective.UpdatedAt = nextBinding.UpdatedAt
	nextProject := cloneProject(project)
	// The reviewed expected revision is deliberately stale. The candidate is
	// internally valid for that expectation so the conflict occurs in storage
	// after the binding and Objective update statements have run.
	nextProject.Revision = 3
	nextProject.UpdatedAt = nextBinding.UpdatedAt
	plan := &SkillReferenceUpgradePlan{
		APIVersion: SkillReferenceUpgradeAPIVersion, Scope: scope, DeploymentID: binding.DeploymentID,
		BindingID: binding.ID, ExpectedBindingRevision: 1,
		From:       SkillReferenceIdentity{ID: binding.SkillID, Version: binding.SkillVersion},
		To:         SkillReferenceIdentity{ID: binding.SkillID, Version: nextBinding.SkillVersion},
		Objectives: []SkillReferenceObjectiveImpact{{ID: objective.ID, ExpectedRevision: 1}},
		Projects:   []SkillReferenceProjectImpact{{ID: project.ID, ExpectedRevision: 2}},
		Digest:     "sha256:reviewed",
	}
	objectiveEvent := objectiveActivityEvent(nextObjective, ActivityActor{Type: "user", ID: "operator"},
		ActivityVisibilityScope, "objective.skill_reference_upgraded", "Upgrade Objective reference",
		nextObjective.UpdatedAt, map[string]interface{}{"planDigest": plan.Digest})
	projectEvent := projectEvent(nextProject, "project.skill_reference_upgraded",
		ActivityActor{Type: "user", ID: "operator"}, ActivityVisibilityScope, "Upgrade Project reference")
	mutation := &SkillReferenceUpgradeMutation{
		Plan: plan, Binding: nextBinding,
		Objectives: []SkillReferenceObjectiveMutation{{
			Value: nextObjective, ExpectedRevision: 1, Event: objectiveEvent,
		}},
		Projects: []SkillReferenceProjectMutation{{
			Value: nextProject, ExpectedRevision: 2, Event: projectEvent,
		}},
		Receipt: &SkillReferenceUpgradeReceipt{
			APIVersion: SkillReferenceUpgradeAPIVersion, PlanDigest: plan.Digest, Scope: scope,
			DeploymentID: binding.DeploymentID, BindingID: binding.ID, BindingRevision: 2,
			From: plan.From, To: plan.To, AppliedAt: nextBinding.UpdatedAt,
		},
	}
	if err := store.ApplySkillReferenceUpgrade(ctx, mutation); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
		t.Fatalf("expected atomic revision conflict, got %v", err)
	}
	bindings, err := store.ListSkillBindings(ctx, bindingScope, binding.DeploymentID)
	if err != nil || len(bindings) != 1 || bindings[0].Revision != 1 || bindings[0].SkillVersion != "1.0.0" {
		t.Fatalf("binding changed after rollback: %#v err=%v", bindings, err)
	}
	restoredObjective, err := store.GetObjective(ctx, scope, objective.ID)
	if err != nil || restoredObjective.Revision != 1 {
		t.Fatalf("objective changed after rollback: %#v err=%v", restoredObjective, err)
	}
	restoredProject, err := store.GetProject(ctx, scope, project.ID)
	if err != nil || restoredProject.Revision != 1 {
		t.Fatalf("project changed after rollback: %#v err=%v", restoredProject, err)
	}
}
