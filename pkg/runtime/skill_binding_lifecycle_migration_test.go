package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSQLiteSkillBindingLifecycleRepairRestoresUpgradedWorkforceBinding(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	malformed := &skill.Binding{
		ID: "browser", Scope: scope, DeploymentID: "researcher", SkillID: "browser", SkillVersion: "1.1.1",
		AllowedActions: []string{"open"}, MaximumRisk: skill.RiskLevelRead, Revision: 2, UpdatedAt: now,
		Lifecycle: []skill.BindingLifecycleEntry{{
			Revision: 2, Action: skill.BindingLifecycleUpdated,
			Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Upgrade Browser", At: now,
		}},
	}
	payload, err := json.Marshal(malformed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(context.Background(), `INSERT INTO skill_bindings
		(scope_kind,scope_id,deployment_id,id,skill_id,skill_version,source_identity,revision,payload)
		VALUES(?,?,?,?,?,?,?,?,?)`, scope.Kind, scope.ID, malformed.DeploymentID, malformed.ID,
		malformed.SkillID, malformed.SkillVersion, malformed.SourceIdentity, malformed.Revision, string(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err = skill.NewCatalogWithStore(store).ListBindings(context.Background(), scope, malformed.DeploymentID); err == nil {
		t.Fatal("malformed upgraded binding unexpectedly remained readable")
	}
	if err = migrateSkillBindingLifecycleRepairSQLite(store.db); err != nil {
		t.Fatal(err)
	}
	bindings, err := skill.NewCatalogWithStore(store).ListBindings(context.Background(), scope, malformed.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || !bindings[0].CreatedAt.Equal(now) || !bindings[0].UpdatedAt.Equal(now) {
		t.Fatalf("repaired binding = %#v", bindings)
	}
}
