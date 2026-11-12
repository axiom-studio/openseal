package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeMaintenanceRequiresOwnedExactTargetAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
		publisher := "https://skills.invalid/reader"
		for _, version := range []string{"0.9.0", "1.0.0", "1.1.0", "1.2.0"} {
			if err := catalog.Register(t.Context(), &skill.Definition{
				ID: "reader", Version: version, Name: "Reader", Requirements: skill.Requirements{AlwaysAvailable: true},
				Source:    &skill.SourceProvenance{Identity: publisher, Format: "native", Digest: strings.Repeat("a", 64)},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: "reader"},
				Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read", Risk: skill.RiskLevelRead,
					SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		scope := models.Scope{Kind: "tenant", ID: "maintenance-binding"}
		if err := catalog.Bind(t.Context(), &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "reader",
			SkillVersion: "0.9.0", SourceIdentity: publisher, AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		gate, err := store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), models.SkillRuntimeMaintenanceRequest{
			Scope: scope, SkillID: "reader", SourceIdentity: publisher, RuntimeIdentity: "installed-reader", OperationID: "upgrade-reader",
			FromVersion: "1.0.0", ToVersion: "1.1.0", TargetSourceDigest: "sha256:" + strings.Repeat("a", 64), FromSourceDigest: "sha256:" + strings.Repeat("a", 64),
			FromArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("b", 64), ToArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("c", 64),
			Owner: "controller", LeaseDuration: time.Minute, Now: now,
			ExpectedBindings: []models.SkillRuntimeMaintenanceBindingRevision{{DeploymentID: "agent", BindingID: "account", Revision: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		service := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
		plan, err := service.Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: publisher})
		if err != nil || plan.ApprovalRequired {
			t.Fatalf("compatible old binding plan: %#v %v", plan, err)
		}
		request := skillbinding.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "controller"}, Reason: "Advance verified compatible binding"}
		if _, err := service.Apply(t.Context(), request); !errors.Is(err, models.ErrSkillRuntimeMaintenance) {
			t.Fatalf("ordinary caller crossed maintenance gate: %v", err)
		}
		stale := *gate
		stale.Revision++
		if _, err := service.Apply(models.WithSkillRuntimeMaintenance(t.Context(), &stale), request); !errors.Is(err, models.ErrSkillRuntimeMaintenance) {
			t.Fatalf("stale owner crossed maintenance gate: %v", err)
		}
		third, err := service.Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.2.0", ToSourceIdentity: publisher})
		if err != nil {
			t.Fatal(err)
		}
		other := request
		other.Plan = third
		if _, err := service.Apply(models.WithSkillRuntimeMaintenance(t.Context(), gate), other); !errors.Is(err, models.ErrSkillRuntimeMaintenanceConflict) {
			t.Fatalf("maintenance owner selected unverified third target: %v", err)
		}
		if _, err := service.Apply(models.WithSkillRuntimeMaintenance(t.Context(), gate), request); err != nil {
			t.Fatalf("compatible old binding could not reach verified target: %v", err)
		}
		binding, _ := catalog.GetBinding(t.Context(), skill.ScopeReference(scope), "agent", "account")
		if binding.Revision != 2 || binding.SkillVersion != gate.ToVersion || binding.SourceIdentity != publisher {
			t.Fatalf("binding target or authority changed: %#v", binding)
		}
		current, _ := store.(models.SkillRuntimeMaintenanceStore).GetSkillRuntimeMaintenance(t.Context(), scope, "reader")
		if !current.Active || current.Revision != gate.Revision {
			t.Fatalf("binding migration prematurely reopened execution: %#v", current)
		}
	})
}
