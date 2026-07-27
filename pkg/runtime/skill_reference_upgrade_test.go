package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeEstablishesLifecycleForWorkforceBinding(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "browser", Version: version, Name: "Browser",
			Requirements: skill.Requirements{AlwaysAvailable: true}, Transport: skill.TransportReference{Kind: "tool", Endpoint: "browser"},
			Actions: map[string]skill.Action{"open": {
				Name: "open", Description: "Open a page", InputSchema: map[string]interface{}{"type": "object"},
				Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	// Workforce apply bindings intentionally have no management lifecycle yet.
	if err := store.SaveSkillBinding(ctx, &skill.Binding{
		ID: "browser", Scope: scope, DeploymentID: "researcher", SkillID: "browser", SkillVersion: "1.0.0",
		AllowedActions: []string{"open"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	service := NewSkillReferenceUpgradeService(store, catalog)
	service.now = func() time.Time { return now }
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", BindingID: "browser", ToVersion: "1.1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "Adopt managed Browser runtime",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := catalog.ListBindings(ctx, scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].SkillVersion != "1.1.0" || !bindings[0].CreatedAt.Equal(now) ||
		!bindings[0].UpdatedAt.Equal(now) || len(bindings[0].Lifecycle) != 1 || bindings[0].Lifecycle[0].Revision != 2 {
		t.Fatalf("upgraded workforce binding = %#v", bindings)
	}
}
