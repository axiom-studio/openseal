//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

func TestPostgresDeploymentIdentitiesReserveAndBackfillFailsOnCollision(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := "deployment_ids_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	if err := store.ReserveDeploymentID(ctx, scope, "a", workforce.DeploymentKindAgent); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveDeploymentID(ctx, scope, "a", workforce.DeploymentKindAgent); err != nil {
		t.Fatalf("same-kind reservation must be idempotent: %v", err)
	}
	if err := store.ReserveDeploymentID(ctx, scope, "a", workforce.DeploymentKindTeam); !errors.Is(err, workforce.ErrDeploymentIDTaken) {
		t.Fatalf("cross-kind reservation err = %v", err)
	}
	if kind, err := store.DeploymentIDKind(ctx, scope, "a"); err != nil || kind != workforce.DeploymentKindAgent {
		t.Fatalf("kind = %q, %v", kind, err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table(table)+`(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES('tenant','one','dup','d','1',1,now(),'{}')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE `+store.table("deployment_identities")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version = $1`, deploymentIdentityMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); !errors.Is(err, workforce.ErrDeploymentIDTaken) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("migration with a shared Agent and Team ID: err = %v", err)
	}
}
