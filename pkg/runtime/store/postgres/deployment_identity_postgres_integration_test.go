//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestPostgresDeploymentIdentitiesReserveAndBackfillFailsOnCollision(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "deployment_ids_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
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
		if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table(table)+`(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES('tenant','one','dup','d','1',1,now(),'{}')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DROP TABLE `+pgSchema.Table("deployment_identities")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, deploymentIdentityMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); !errors.Is(err, workforce.ErrDeploymentIDTaken) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("migration with a shared Agent and Team ID: err = %v", err)
	}
}
