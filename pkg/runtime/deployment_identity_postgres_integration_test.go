//go:build integration

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestPostgresDeploymentIdentitiesReserveAndBackfillFailsOnCollision(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "deployment_ids_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.quoted()+` CASCADE`)
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
		if _, err := pgSchema.db.ExecContext(ctx, `INSERT INTO `+pgSchema.table(table)+`(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES('tenant','one','dup','d','1',1,now(),'{}')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pgSchema.db.ExecContext(ctx, `DROP TABLE `+pgSchema.table("deployment_identities")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.db.ExecContext(ctx, `DELETE FROM `+pgSchema.table("schema_migrations")+` WHERE version = $1`, deploymentIdentityMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); !errors.Is(err, workforce.ErrDeploymentIDTaken) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("migration with a shared Agent and Team ID: err = %v", err)
	}
}
