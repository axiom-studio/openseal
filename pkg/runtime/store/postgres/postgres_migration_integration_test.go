//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestPostgresConcurrentColdStartsSerializeMigrationLeadership(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_migration_race_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cleanup, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
		_ = cleanup.Close()
	})

	const replicas = 6
	start := make(chan struct{})
	stores := make(chan *PostgresStore, replicas)
	errorsFound := make(chan error, replicas)
	events := make(chan PostgresMigrationEvent, replicas*3)
	var wait sync.WaitGroup
	for range replicas {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			store, openErr := NewPostgresStore(ctx, dsn,
				WithPostgresSchema(schema),
				WithPostgresMigrationLock(30*time.Second, 10*time.Millisecond),
				WithPostgresMigrationObserver(func(event PostgresMigrationEvent) { events <- event }),
			)
			if openErr != nil {
				errorsFound <- openErr
				return
			}
			stores <- store
		}()
	}
	close(start)
	wait.Wait()
	close(stores)
	close(errorsFound)
	close(events)
	for openErr := range errorsFound {
		t.Errorf("concurrent startup: %v", openErr)
	}
	opened := 0
	for store := range stores {
		opened++
		if stats := store.MigrationStats(); stats.SchemaVersion != currentPostgresSchemaVersion || stats.TotalDuration <= 0 {
			t.Errorf("migration stats = %#v", stats)
		}
		if version, versionErr := store.PostgresSchemaVersion(ctx); versionErr != nil || version != currentPostgresSchemaVersion {
			t.Errorf("schema version = %d, err = %v", version, versionErr)
		}
		_ = store.Close()
	}
	if opened != replicas {
		t.Fatalf("opened replicas = %d, want %d", opened, replicas)
	}
	completed, current := 0, 0
	for event := range events {
		switch event.Phase {
		case PostgresMigrationComplete:
			completed++
		case PostgresMigrationCurrent:
			current++
		}
	}
	if completed != 1 || current != replicas-1 {
		t.Fatalf("migration outcomes: completed=%d current=%d, want 1 and %d", completed, current, replicas-1)
	}
}

func TestPostgresCurrentSchemaStartupIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, pgSchema := openTestPostgresStore(t, "openseal_migration_current_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN

	events := make(chan PostgresMigrationEvent, 4)
	startedAt := time.Now()
	reopened, err := NewPostgresStore(ctx, dsn,
		WithPostgresSchema(schema),
		WithPostgresMigrationLock(5*time.Second, 10*time.Millisecond),
		WithPostgresMigrationObserver(func(event PostgresMigrationEvent) { events <- event }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("current-schema startup took %s, want under 2s", elapsed)
	}
	if acquired, outcome := <-events, <-events; acquired.Phase != PostgresMigrationAcquired ||
		outcome.Phase != PostgresMigrationCurrent || outcome.SchemaVersion != currentPostgresSchemaVersion {
		t.Fatalf("current-schema events = %#v, %#v", acquired, outcome)
	}
	if stats := reopened.MigrationStats(); stats.SchemaVersion != currentPostgresSchemaVersion ||
		stats.TotalDuration <= 0 || stats.TotalDuration >= 2*time.Second {
		t.Fatalf("current-schema stats = %#v", stats)
	}
}

func TestPostgresLaterMigrationDoesNotReplayActivityProjectionBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, pgSchema := openTestPostgresStore(t, "openseal_migration_activity_gate_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("run_activity")+`
		(scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ('tenant','1','migration-proof',1,'migration-proof','proof',CURRENT_TIMESTAMP,'{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `
		CREATE FUNCTION `+pgSchema.Quoted()+`.reject_activity_backfill() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'activity projection backfill replayed'; END $$;
		CREATE TRIGGER reject_activity_backfill BEFORE UPDATE ON `+pgSchema.Table("run_activity")+`
		FOR EACH STATEMENT EXECUTE FUNCTION `+pgSchema.Quoted()+`.reject_activity_backfill()
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, currentPostgresSchemaVersion); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now()
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatalf("later migration replayed an applied activity backfill: %v", err)
	}
	defer reopened.Close()
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("later migration startup took %s, want under 2s", elapsed)
	}
	if version, versionErr := reopened.PostgresSchemaVersion(ctx); versionErr != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version after later migration = %d, err = %v", version, versionErr)
	}
}

func TestPostgresMigrationLockWaitIsObservableAndBounded(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_migration_wait_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lockName := "openseal:migrate:" + schema
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, lockName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, lockName)
		_ = connection.Close()
		_ = blocker.Close()
	})

	events := make(chan PostgresMigrationEvent, 8)
	result := make(chan *PostgresStore, 1)
	openErrors := make(chan error, 1)
	go func() {
		store, openErr := NewPostgresStore(ctx, dsn,
			WithPostgresSchema(schema),
			WithPostgresMigrationLock(5*time.Second, 10*time.Millisecond),
			WithPostgresMigrationObserver(func(event PostgresMigrationEvent) { events <- event }),
		)
		result <- store
		openErrors <- openErr
	}()
	select {
	case event := <-events:
		if event.Phase != PostgresMigrationWaiting {
			t.Fatalf("first event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("migration contention was not observed")
	}
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, lockName); err != nil {
		t.Fatal(err)
	}
	store := <-result
	if err := <-openErrors; err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if stats := store.MigrationStats(); stats.WaitDuration <= 0 || stats.SchemaVersion != currentPostgresSchemaVersion {
		t.Fatalf("migration stats = %#v", stats)
	}
	phases := []PostgresMigrationPhase{PostgresMigrationAcquired, PostgresMigrationComplete}
	for _, phase := range phases {
		select {
		case event := <-events:
			if event.Phase != phase {
				t.Fatalf("event = %#v, want phase %q", event, phase)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing migration phase %q", phase)
		}
	}

	timeoutSchema := "openseal_migration_timeout_" + uuid.NewString()[:8]
	timeoutLock := "openseal:migrate:" + timeoutSchema
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, timeoutLock); err != nil {
		t.Fatal(err)
	}
	defer connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, timeoutLock)
	timeoutEvents := make(chan PostgresMigrationEvent, 4)
	_, err = NewPostgresStore(ctx, dsn,
		WithPostgresSchema(timeoutSchema),
		WithPostgresMigrationLock(150*time.Millisecond, 10*time.Millisecond),
		WithPostgresMigrationObserver(func(event PostgresMigrationEvent) { timeoutEvents <- event }),
	)
	if !errors.Is(err, ErrPostgresMigrationLockTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	if first, second := <-timeoutEvents, <-timeoutEvents; first.Phase != PostgresMigrationWaiting || second.Phase != PostgresMigrationTimeout {
		t.Fatalf("timeout events = %#v, %#v", first, second)
	}
}

func TestPostgresSkillCatalogSourceVariantsMigrateAndRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "openseal_skill_variants_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	catalog := skill.NewCatalogWithStore(primary)
	identities := []string{"catalog::@alice/research", "catalog::@bob/research"}
	for _, identity := range identities {
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "research", Version: "1.0.0", Name: "Research",
			Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1"},
			Prompt: &skill.PromptModule{Instructions: "Use the exact installed source."},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	for index, identity := range identities {
		if err := catalog.Bind(ctx, &skill.Binding{
			ID: []string{"alice", "bob"}[index], Scope: scope, DeploymentID: "analyst",
			SkillID: "research", SkillVersion: "1.0.0", SourceIdentity: identity,
			EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	restarted := skill.NewCatalogWithStore(replica)
	if _, err := restarted.GetDefinition(ctx, "research", "1.0.0"); !errors.Is(err, skill.ErrDefinitionAmbiguous) {
		t.Fatalf("restarted ambiguous definition = %v", err)
	}
	prompts, err := restarted.ListModelPrompts(ctx, scope, "analyst")
	if err != nil || len(prompts) != 2 {
		t.Fatalf("restarted prompts = %#v, %v", prompts, err)
	}
	if err := primary.RollbackPostgresMigrations(ctx, 17); err == nil {
		t.Fatal("publisher-colliding definitions unexpectedly allowed a lossy rollback")
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("failed rollback changed schema version = %d, %v", version, err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("skill_bindings")+` WHERE source_identity = $1`, identities[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("skill_definitions")+` WHERE source_identity = $1`, identities[1]); err != nil {
		t.Fatal(err)
	}
	if err := primary.RollbackPostgresMigrations(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if err := primary.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded := skill.NewCatalogWithStore(primary)
	definition, err := reloaded.GetDefinitionVariant(ctx, "research", "1.0.0", identities[0])
	if err != nil || definition == nil || skill.DefinitionSourceIdentity(definition) != identities[0] {
		t.Fatalf("reapplied source identity = %#v, %v", definition, err)
	}
}
