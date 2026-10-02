package runtime

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestSkillReferenceUpgradeStorageContractChangesRequireReviewAcrossStores(t *testing.T) {
	mutations := map[string]func(*skill.StorageRequirement){
		"name": func(s *skill.StorageRequirement) { s.Name = "another-profile" },
		"path": func(s *skill.StorageRequirement) { s.MountPath = "/var/lib/another-profile" },
		"durability": func(s *skill.StorageRequirement) {
			s.Durability, s.Retention = skill.StorageDurabilityEphemeral, skill.StorageRetentionDelete
		},
		"capacity":  func(s *skill.StorageRequirement) { s.MinimumCapacity = "2Gi" },
		"retention": func(s *skill.StorageRequirement) { s.Retention = skill.StorageRetentionDelete },
		"writer": func(s *skill.StorageRequirement) {
			group := int64(2000)
			s.WritableGroup = &group
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := fixture.store.(KernelStore)
				catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
				group := int64(1000)
				before := skill.StorageRequirement{Name: "profile", MountPath: "/var/lib/profile", Durability: skill.StorageDurabilityPersistent,
					MinimumCapacity: "1Gi", Retention: skill.StorageRetentionRetain, WritableGroup: &group}
				after := before
				mutate(&after)
				for version, storage := range map[string]skill.StorageRequirement{"1.0.0": before, "1.1.0": after} {
					definition := &skill.Definition{
						ID: "profile-reader", Version: version, Name: "Profile reader", Requirements: skill.Requirements{AlwaysAvailable: true, Storage: []skill.StorageRequirement{storage}},
						Transport: skill.TransportReference{Kind: "tool", Endpoint: "profile-reader"},
						Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read profile", Risk: skill.RiskLevelRead,
							SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
					}
					if err := catalog.Register(t.Context(), definition); err != nil {
						t.Fatal(err)
					}
				}
				scope := Scope{Kind: "tenant", ID: "storage-review"}
				if err := catalog.Bind(t.Context(), &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "profile-reader",
					SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
					t.Fatal(err)
				}
				service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
				plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0"})
				if err != nil || !plan.ApprovalRequired || len(plan.Findings) != 1 || plan.Findings[0].Code != "storage_contract_changed" {
					t.Fatalf("storage migration bypassed review with unchanged actions: %#v %v", plan, err)
				}
				if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Automatic update"}); !errors.Is(err, ErrSkillReferenceUpgradeApproval) {
					t.Fatalf("unattended profile migration was applied: %v", err)
				}
				binding, _ := catalog.GetBinding(t.Context(), skill.ScopeReference(scope), "agent", "account")
				if binding.SkillVersion != "1.0.0" || binding.Revision != 1 {
					t.Fatalf("profile migration changed authority: %#v", binding)
				}
			})
		})
	}
}

func TestTeamSkillReferenceUpgradeStorageMigrationKeepsExistingRoleAuthority(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
		catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
		definition, err := catalog.GetDefinition(t.Context(), plan.From.ID, plan.To.Version)
		if err != nil {
			t.Fatal(err)
		}
		definition.Version = "1.2.0"
		definition.Requirements.Storage = []skill.StorageRequirement{{Name: "new-profile", MountPath: "/var/lib/new-profile", Durability: skill.StorageDurabilityPersistent, MinimumCapacity: "1Gi", Retention: skill.StorageRetentionRetain}}
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
		service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
		if _, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: plan.Scope, DeploymentID: plan.DeploymentID, BindingID: plan.BindingID, ToVersion: definition.Version}); !errors.Is(err, ErrSkillReferenceUpgradeInvalid) {
			t.Fatalf("Team grant automatically crossed storage migration: %v", err)
		}
		assertTeamSkillUpgradeUnchanged(t, store, plan)
		current, _ := store.(kernelteam.Store).GetTeamDefinition(t.Context(), plan.TeamAuthority.DefinitionID, plan.TeamAuthority.DefinitionVersion)
		if !skillUpgradeTeamDefinitionEqual(current, wrapped.mutation.TeamAuthority.PreviousDefinition) ||
			!current.Roles[0].SkillGrants[0].ExactIdentity().Equal(capability.NewSkillIdentity(plan.From.ID, plan.From.Version, plan.From.SourceIdentity)) {
			t.Fatalf("profile migration rewrote Team role authority: %#v", current)
		}
	})
}
