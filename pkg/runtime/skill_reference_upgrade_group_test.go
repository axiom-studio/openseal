package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type groupUpgradeStore interface {
	SkillReferenceUpgradeStore
	SaveSkillBinding(context.Context, *skill.Binding, int64) error
}

// A Skill split across several bindings (as the built-in tools are) must
// upgrade as one group: the version-keyed references cannot be attributed to
// one binding, so every binding at that version moves together.
func TestSkillReferenceUpgradeMovesEveryBindingAtTheVersionTogether(t *testing.T) {
	for name, open := range map[string]func(*testing.T) groupUpgradeStore{
		"memory": func(*testing.T) groupUpgradeStore { return NewMemoryStore() },
		"sqlite": func(t *testing.T) groupUpgradeStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "group-upgrade.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := open(t)
			catalog := skill.NewCatalogWithStore(store)
			registerGroupUpgradeVersions(t, catalog)
			scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
			save := func(id, version string, actions []string, disabled bool) {
				t.Helper()
				if err := store.SaveSkillBinding(ctx, &skill.Binding{
					ID: id, Scope: scope, DeploymentID: "agent", SkillID: "tools", SkillVersion: version,
					AllowedActions: actions, MaximumRisk: skill.RiskLevelWrite, Revision: 1, Disabled: disabled,
				}, 0); err != nil {
					t.Fatal(err)
				}
			}
			save("bundled:builtin", "1.0.0", []string{"read"}, false)
			save("bundled:documents", "1.0.0", []string{"write"}, false)
			save("bundled:retired", "1.0.0", []string{"read"}, true)
			save("bundled:apps", "1.1.0", []string{"build"}, false)

			service := NewSkillReferenceUpgradeService(store, catalog)
			plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
				Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent", BindingID: "bundled:builtin", ToVersion: "1.1.0",
			})
			if err != nil {
				t.Fatalf("a Skill split across bindings must be upgradable: %v", err)
			}
			if len(plan.Siblings) != 1 || plan.Siblings[0] != (SkillReferenceBindingRevision{ID: "bundled:documents", ExpectedRevision: 1}) || plan.ApprovalRequired {
				t.Fatalf("plan siblings = %#v approval=%v findings=%#v", plan.Siblings, plan.ApprovalRequired, plan.Findings)
			}
			receipt, err := service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "reconciler"}, Reason: "Adopt tools 1.1.0"})
			if err != nil {
				t.Fatal(err)
			}
			if len(receipt.Siblings) != 1 {
				t.Fatalf("receipt siblings = %#v", receipt.Siblings)
			}
			bindings, err := catalog.ListBindings(ctx, scope, "agent")
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]*skill.Binding{}
			for _, binding := range bindings {
				byID[binding.ID] = binding
			}
			for _, id := range []string{"bundled:builtin", "bundled:documents"} {
				binding := byID[id]
				if binding == nil || binding.SkillVersion != "1.1.0" || binding.Revision != 2 || len(binding.Lifecycle) != 1 ||
					binding.Lifecycle[0].SkillUpgrade == nil || binding.Lifecycle[0].SkillUpgrade.ExpectedBindingRevision != 1 ||
					binding.Lifecycle[0].SkillUpgrade.PlanDigest != plan.Digest {
					t.Fatalf("%s after group upgrade = %#v", id, binding)
				}
			}
			if byID["bundled:documents"].AllowedActions[0] != "write" {
				t.Fatal("each binding keeps its own grant")
			}
			if retired := byID["bundled:retired"]; retired == nil || retired.SkillVersion != "1.0.0" || retired.Revision != 1 {
				t.Fatalf("a disabled binding is a tombstone and must not move: %#v", retired)
			}
			if apps := byID["bundled:apps"]; apps == nil || apps.Revision != 1 {
				t.Fatalf("a binding already at the target must be untouched: %#v", apps)
			}
			// A sibling moved by the group reports that it is current, so a
			// reconciler working from an earlier snapshot treats it as done.
			if _, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
				BindingID: "bundled:documents", ToVersion: "1.1.0"}); !errors.Is(err, ErrSkillReferenceUpgradeCurrent) || !errors.Is(err, ErrSkillReferenceUpgradeInvalid) {
				t.Fatalf("plan for a moved sibling = %v", err)
			}
		})
	}
}

func TestSkillReferenceGroupUpgradeRejectsStaleSiblings(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	registerGroupUpgradeVersions(t, catalog)
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	for _, id := range []string{"bundled:builtin", "bundled:documents"} {
		if err := store.SaveSkillBinding(ctx, &skill.Binding{ID: id, Scope: scope, DeploymentID: "agent", SkillID: "tools", SkillVersion: "1.0.0",
			AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelWrite, Revision: 1}, 0); err != nil {
			t.Fatal(err)
		}
	}
	service := NewSkillReferenceUpgradeService(store, catalog)
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent", BindingID: "bundled:builtin", ToVersion: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	// The sibling changes after review: the reviewed plan no longer applies.
	if err := store.SaveSkillBinding(ctx, &skill.Binding{ID: "bundled:documents", Scope: scope, DeploymentID: "agent", SkillID: "tools", SkillVersion: "1.0.0",
		AllowedActions: []string{"read", "write"}, MaximumRisk: skill.RiskLevelWrite, Revision: 2}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "reconciler"}, Reason: "Adopt tools 1.1.0"}); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
		t.Fatalf("stale sibling apply = %v", err)
	}
	bindings, _ := catalog.ListBindings(ctx, scope, "agent")
	for _, binding := range bindings {
		if binding.SkillVersion != "1.0.0" {
			t.Fatalf("a refused group upgrade must move nothing: %#v", binding)
		}
	}

	// A store refuses a mutation whose siblings do not match the plan.
	fresh, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent", BindingID: "bundled:builtin", ToVersion: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	primary := &skill.Binding{ID: "bundled:builtin", Scope: scope, DeploymentID: "agent", SkillID: "tools", SkillVersion: "1.1.0", Revision: 2}
	if err := store.ApplySkillReferenceUpgrade(ctx, &SkillReferenceUpgradeMutation{Plan: fresh, Binding: primary,
		Receipt: &SkillReferenceUpgradeReceipt{PlanDigest: fresh.Digest}}); !errors.Is(err, ErrSkillReferenceUpgradeInvalid) {
		t.Fatalf("mutation without its siblings = %v", err)
	}
}

func registerGroupUpgradeVersions(t *testing.T, catalog *skill.Catalog) {
	t.Helper()
	for _, version := range []string{"1.0.0", "1.1.0"} {
		actions := map[string]skill.Action{}
		for _, name := range []string{"read", "write", "build"} {
			actions[name] = skill.Action{Name: name, Description: name, InputSchema: map[string]interface{}{"type": "object"},
				Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}
		}
		if err := catalog.Register(context.Background(), &skill.Definition{
			ID: "tools", Version: version, Name: "Tools", Requirements: skill.Requirements{AlwaysAvailable: true},
			Transport: skill.TransportReference{Kind: "tool", Endpoint: "tools"}, Actions: actions,
		}); err != nil {
			t.Fatal(err)
		}
	}
}
