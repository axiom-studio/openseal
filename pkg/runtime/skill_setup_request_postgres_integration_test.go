//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresSkillSetupLifecycleAndMigration(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_setup_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := skillbindingtest.SkillSetupRequest()
	if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	if found, err := store.GetSkillSetupRequest(ctx, Scope{Kind: "tenant", ID: "foreign"}, request.ID); err != nil || found != nil {
		t.Fatalf("cross-tenant lookup: %#v %v", found, err)
	}
	request.Status = "dismissed"
	request.ResolvedBy = "user"
	request.Revision++
	request.UpdatedAt = time.Now().UTC()
	if err := store.SaveSkillSetupRequest(ctx, request, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSkillSetupRequest(ctx, request, 1); !errors.Is(err, ErrSkillSetupConflict) {
		t.Fatalf("stale resolution accepted: %v", err)
	}
	records, err := store.ListSkillSetupRequests(ctx, request.Scope, request.DeploymentID, request.ConversationID)
	if err != nil || len(records) != 1 || records[0].Status != "dismissed" {
		t.Fatalf("stored requests: %#v %v", records, err)
	}
	if err := store.RollbackPostgresMigrations(ctx, 44); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != 44 {
		t.Fatalf("rollback schema: %d %v", version, err)
	}
	reopened, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if records, err := reopened.ListSkillSetupRequests(ctx, request.Scope, request.DeploymentID, request.ConversationID); err != nil || len(records) != 0 {
		t.Fatalf("migration replay: %#v %v", records, err)
	}
}
