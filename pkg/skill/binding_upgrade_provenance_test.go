package skill

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestOrdinaryBindingUpsertCannotChangeIdentityOrMintUpgradeProof(t *testing.T) {
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	binding, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: &Binding{ID: "account", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction,
		Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://git"}}}, Actor: BindingActor{Type: "user", ID: "operator"}, Reason: "Connect account"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skill", "version", "source"} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneBinding(binding)
			switch name {
			case "skill":
				candidate.SkillID = "other"
			case "version":
				candidate.SkillVersion = "2.0.0"
			case "source":
				candidate.SourceIdentity = "other"
			}
			if _, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: candidate, ExpectedRevision: 1, Actor: BindingActor{Type: "user", ID: "operator"}, Reason: "Change reference"}); err == nil {
				t.Fatal("normal save bypassed the canonical upgrade")
			}
		})
	}
	forged := cloneBinding(binding)
	forged.Lifecycle = []BindingLifecycleEntry{{Revision: 2, SkillUpgrade: &capability.BindingSkillUpgradeProvenance{
		From: capability.NewSkillIdentity(definition.ID, definition.Version, ""), To: capability.NewSkillIdentity(definition.ID, "2.0.0", ""),
		ExpectedBindingRevision: 1, PlanDigest: "sha256:" + strings.Repeat("a", 64)}}}
	saved, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: forged, ExpectedRevision: 1, Actor: BindingActor{Type: "user", ID: "operator"}, Reason: "Save configuration"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range saved.Lifecycle {
		if entry.SkillUpgrade != nil {
			t.Fatal("caller minted canonical upgrade provenance")
		}
	}
	disabled, err := catalog.DisableBinding(t.Context(), DisableBindingRequest{Scope: saved.Scope, DeploymentID: saved.DeploymentID, BindingID: saved.ID, ExpectedRevision: saved.Revision, Actor: BindingActor{Type: "user", ID: "operator"}, Reason: "Temporarily disconnect"})
	if err != nil {
		t.Fatal(err)
	}
	disabled.Disabled = false
	reenabled, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: disabled, ExpectedRevision: disabled.Revision, Actor: BindingActor{Type: "user", ID: "operator"}, Reason: "Reconnect same identity"})
	if err != nil || reenabled.Disabled || reenabled.SkillVersion != definition.Version || reenabled.Lifecycle[len(reenabled.Lifecycle)-1].Action != BindingLifecycleEnabled {
		t.Fatalf("same-identity reenable broken: %#v %v", reenabled, err)
	}
}
