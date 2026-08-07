//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestPostgresConcurrentColdStartsSerializeMigrationLeadership(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_migration_race_" + uuid.NewString()[:8]
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
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	schema := "openseal_migration_current_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})

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
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	schema := "openseal_migration_activity_gate_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	if _, err = primary.db.ExecContext(ctx, `INSERT INTO `+primary.table("run_activity")+`
		(scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ('tenant','1','migration-proof',1,'migration-proof','proof',CURRENT_TIMESTAMP,'{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err = primary.db.ExecContext(ctx, `
		CREATE FUNCTION `+primary.quotedSchema()+`.reject_activity_backfill() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'activity projection backfill replayed'; END $$;
		CREATE TRIGGER reject_activity_backfill BEFORE UPDATE ON `+primary.table("run_activity")+`
		FOR EACH STATEMENT EXECUTE FUNCTION `+primary.quotedSchema()+`.reject_activity_backfill()
	`); err != nil {
		t.Fatal(err)
	}
	if _, err = primary.db.ExecContext(ctx, `DELETE FROM `+primary.table("schema_migrations")+` WHERE version = $1`, currentPostgresSchemaVersion); err != nil {
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
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	schema := "openseal_migration_wait_" + uuid.NewString()[:8]
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
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_skill_variants_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	catalog := skill.NewCatalogWithStore(primary)
	identities := []string{"clawhub::@alice/research", "clawhub::@bob/research"}
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
	if _, err := primary.db.ExecContext(ctx, `DELETE FROM `+primary.table("skill_bindings")+` WHERE source_identity = $1`, identities[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.db.ExecContext(ctx, `DELETE FROM `+primary.table("skill_definitions")+` WHERE source_identity = $1`, identities[1]); err != nil {
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

func TestPostgresCanonicalStoreConformanceAndReplicaClaims(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_test_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	var retiredTable sql.NullString
	if err := primary.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, schema+".runs").Scan(&retiredTable); err != nil {
		t.Fatal(err)
	}
	if retiredTable.Valid {
		t.Fatalf("fresh canonical schema created retired table %q", retiredTable.String)
	}

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(primary)
	portfolio.now = func() time.Time { return now }
	scope := Scope{Kind: "tenant", ID: "postgres-e2e"}
	otherScope := Scope{Kind: "tenant", ID: "other"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"}
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Operate", Goal: "Keep services healthy", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if crossScope, err := primary.GetObjective(ctx, otherScope, objective.ID); err != nil || crossScope != nil {
		t.Fatalf("cross-scope objective = %#v, %v", crossScope, err)
	}
	summary := "Healthy"
	updated, err := portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: objective.Revision, ProgressSummary: &summary})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("updated objective = %#v, %v", updated, err)
	}
	if _, err := portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: objective.Revision, ProgressSummary: &summary}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale objective update = %v", err)
	}
	conversationRun, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, Goal: "Coordinate channel participation", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimedConversation, err := primary.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "conversation-worker", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimedConversation == nil || claimedConversation.ID != conversationRun.ID {
		t.Fatalf("postgres conversation claim = %#v, %v", claimedConversation, err)
	}

	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	stores := []*PostgresStore{primary, replica}
	var wait sync.WaitGroup

	const agentRunCount = 16
	agentRunIDs := make(map[string]bool, agentRunCount)
	for index := 0; index < agentRunCount; index++ {
		run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "operator", Goal: "Inspect shard", Source: RunSourceObjective, Priority: index % 3})
		if err != nil {
			t.Fatal(err)
		}
		agentRunIDs[run.ID] = true
	}
	agentClaims := make(chan string, agentRunCount)
	agentClaimErrors := make(chan error, 8)
	wait = sync.WaitGroup{}
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			store := stores[worker%len(stores)]
			for {
				run, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "agent-worker-" + string(rune('a'+worker)), Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
				if claimErr != nil {
					agentClaimErrors <- claimErr
					return
				}
				if run == nil {
					return
				}
				agentClaims <- run.ID
			}
		}(worker)
	}
	wait.Wait()
	close(agentClaims)
	close(agentClaimErrors)
	for claimErr := range agentClaimErrors {
		t.Fatal(claimErr)
	}
	claimedAgentRuns := make(map[string]bool)
	for runID := range agentClaims {
		if claimedAgentRuns[runID] || !agentRunIDs[runID] {
			t.Fatalf("invalid duplicate agent run claim %q", runID)
		}
		claimedAgentRuns[runID] = true
	}
	if len(claimedAgentRuns) != agentRunCount {
		t.Fatalf("unique agent run claims = %d, want %d", len(claimedAgentRuns), agentRunCount)
	}

	for index := 0; index < 2; index++ {
		if _, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "limited"}, AssignedAgentID: "limited", Goal: "Capacity limited work", Source: RunSourceObjective}); err != nil {
			t.Fatal(err)
		}
	}
	limited := make(chan *AgentRun, 2)
	limitedErrors := make(chan error, 2)
	wait = sync.WaitGroup{}
	for worker := 0; worker < 2; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			run, claimErr := stores[worker].ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "limited-worker-" + string(rune('a'+worker)), AssignedAgentID: "limited", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 1})
			if claimErr != nil {
				limitedErrors <- claimErr
				return
			}
			limited <- run
		}(worker)
	}
	wait.Wait()
	close(limited)
	close(limitedErrors)
	for claimErr := range limitedErrors {
		t.Fatal(claimErr)
	}
	limitedClaims := 0
	for run := range limited {
		if run != nil {
			limitedClaims++
		}
	}
	if limitedClaims != 1 {
		t.Fatalf("capacity-limited claims = %d, want 1", limitedClaims)
	}

	for _, candidate := range []struct {
		key      string
		priority int
	}{
		{key: "channel-a", priority: 10},
		{key: "channel-a", priority: 9},
		{key: "channel-b", priority: 1},
	} {
		if _, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, Kind: RunKindConversation, Owner: owner, ConcurrencyKey: candidate.key,
			Goal: "Coordinate " + candidate.key, Source: RunSourceChat, Priority: candidate.priority,
		}); err != nil {
			t.Fatal(err)
		}
	}
	keyed := make(chan *AgentRun, 3)
	keyedErrors := make(chan error, 3)
	wait = sync.WaitGroup{}
	for worker := 0; worker < 3; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			run, claimErr := stores[worker%len(stores)].ClaimNextAgentRun(ctx, AgentRunClaim{
				Scope: scope, Kind: RunKindConversation, WorkerID: "keyed-worker-" + string(rune('a'+worker)),
				Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForConcurrencyKey: 1,
			})
			if claimErr != nil {
				keyedErrors <- claimErr
				return
			}
			keyed <- run
		}(worker)
	}
	wait.Wait()
	close(keyed)
	close(keyedErrors)
	for claimErr := range keyedErrors {
		t.Fatal(claimErr)
	}
	keyedClaims := 0
	keyedClaimCounts := make(map[string]int)
	for run := range keyed {
		if run == nil {
			continue
		}
		keyedClaims++
		keyedClaimCounts[run.ConcurrencyKey]++
	}
	if keyedClaims != 2 || keyedClaimCounts["channel-a"] != 1 || keyedClaimCounts["channel-b"] != 1 {
		t.Fatalf("cross-replica keyed claims = %d, counts = %#v", keyedClaims, keyedClaimCounts)
	}

	auditRun, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "auditor"}, AssignedAgentID: "auditor", Goal: "Audit activity", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	claimedAuditRun, err := primary.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "audit-worker", AssignedAgentID: "auditor", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimedAuditRun == nil || claimedAuditRun.ID != auditRun.ID {
		t.Fatalf("claimed audit run = %#v, %v", claimedAuditRun, err)
	}
	const activityCount = 12
	activityErrors := make(chan error, activityCount)
	wait = sync.WaitGroup{}
	for index := 0; index < activityCount; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			service := NewRunActivityService(stores[index%len(stores)], stores[index%len(stores)])
			_, appendErr := service.AppendActivity(ctx, &ActivityEvent{ID: uuid.NewString(), Scope: scope, RunID: auditRun.ID, EventType: "audit.observed", Summary: "Observation", Actor: ActivityActor{Type: "agent", ID: "auditor"}, Visibility: ActivityVisibilityTeam, CreatedAt: now})
			activityErrors <- appendErr
		}(index)
	}
	wait.Wait()
	close(activityErrors)
	for appendErr := range activityErrors {
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	activity, err := replica.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: auditRun.ID, Limit: 100})
	if err != nil || len(activity) != activityCount {
		t.Fatalf("activity count = %d, %v", len(activity), err)
	}
	for index, event := range activity {
		if event.Sequence != int64(index+1) {
			t.Fatalf("activity sequence[%d] = %d", index, event.Sequence)
		}
	}

	turnServices := []*AgentTurnService{NewAgentTurnService(primary, primary), NewAgentTurnService(replica, replica)}
	for _, service := range turnServices {
		service.now = func() time.Time { return now }
	}
	turns := make(chan *AgentTurn, 2)
	turnErrors := make(chan error, 2)
	wait = sync.WaitGroup{}
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			turn, beginErr := turnServices[index].BeginTurn(ctx, BeginAgentTurnRequest{Scope: scope, RunID: auditRun.ID, WorkerID: "turn-worker-" + string(rune('a'+index)), LeaseDuration: time.Minute})
			turns <- turn
			turnErrors <- beginErr
		}(index)
	}
	wait.Wait()
	close(turns)
	close(turnErrors)
	createdTurns, activeConflicts := 0, 0
	var activeTurn *AgentTurn
	for turn := range turns {
		if turn != nil {
			createdTurns++
			activeTurn = turn
		}
	}
	for beginErr := range turnErrors {
		if errors.Is(beginErr, ErrActiveTurnExists) {
			activeConflicts++
		} else if beginErr != nil {
			t.Fatal(beginErr)
		}
	}
	if createdTurns != 1 || activeConflicts != 1 {
		t.Fatalf("turn race created=%d conflicts=%d", createdTurns, activeConflicts)
	}
	finished, err := turnServices[0].FinishTurn(ctx, scope, activeTurn.ID, FinishAgentTurnRequest{ExpectedRevision: activeTurn.Revision, Status: AgentTurnStatusCompleted, WorkerID: activeTurn.LeaseOwner, OutputSummary: "Audit complete"})
	if err != nil || finished.Status != AgentTurnStatusCompleted {
		t.Fatalf("finished turn = %#v, %v", finished, err)
	}
	if crossScopeTurn, err := replica.GetAgentTurn(ctx, otherScope, activeTurn.ID); err != nil || crossScopeTurn != nil {
		t.Fatalf("cross-scope turn = %#v, %v", crossScopeTurn, err)
	}

	agentRegistry := kernelagent.NewRegistryWithStore(primary)
	for _, version := range []string{"1", "2"} {
		if _, err := agentRegistry.RegisterDefinition(ctx, sqliteAgentDefinition(version)); err != nil {
			t.Fatal(err)
		}
	}
	agentScope := capability.ScopeReference{Kind: "tenant", ID: "postgres-e2e"}
	deployment, _, err := agentRegistry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "operator", Scope: agentScope, DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	replicaRegistry := kernelagent.NewRegistryWithStore(replica)
	activated, _, err := replicaRegistry.ActivateDefinition(ctx, agentScope, deployment.ID, "2", deployment.Revision, "user", "admin", "roll forward")
	if err != nil || activated.ActiveVersion != "2" {
		t.Fatalf("replica definition activation = %#v, %v", activated, err)
	}
	if restoredDeployment, err := agentRegistry.GetDeployment(ctx, agentScope, deployment.ID); err != nil || restoredDeployment.ActiveVersion != "2" {
		t.Fatalf("restored deployment = %#v, %v", restoredDeployment, err)
	}
	placement := *activated
	placement.RolloutStatus = kernelagent.RolloutPaused
	placement.Credentials = map[string]capability.CredentialReference{
		"MODEL_PROVIDER": {Kind: "model-provider", ID: "credential://postgres-e2e/model-provider"},
	}
	updatedPlacement, placementAudit, err := agentRegistry.UpdateDeployment(ctx, &placement, activated.Revision, "system", "reconciler", "place model provider")
	if err != nil || updatedPlacement.RolloutStatus != kernelagent.RolloutPaused || placementAudit.ChangeKind != "configuration-updated" {
		t.Fatalf("deployment placement = %#v %#v, %v", updatedPlacement, placementAudit, err)
	}
	restoredPlacement, err := replicaRegistry.GetDeployment(ctx, agentScope, deployment.ID)
	if err != nil || restoredPlacement.Revision != updatedPlacement.Revision || restoredPlacement.Credentials["MODEL_PROVIDER"].ID != "credential://postgres-e2e/model-provider" {
		t.Fatalf("restored placement = %#v, %v", restoredPlacement, err)
	}
	placementHistory, err := replicaRegistry.ListActivations(ctx, agentScope, deployment.ID)
	if err != nil || len(placementHistory) != 3 || placementHistory[2].ChangeKind != "configuration-updated" || placementHistory[2].FromVersion != placementHistory[2].ToVersion {
		t.Fatalf("placement history = %#v, %v", placementHistory, err)
	}
	compilation, err := agentRegistry.RecordCompilation(ctx, &kernelagent.DefinitionCompilation{
		ID: "operator-source-2", Scope: agentScope, DeploymentID: deployment.ID, DefinitionID: "operator", CandidateVersion: "2",
		Source:       kernelagent.CompilationSource{Kind: "runbook", ID: "operator-source", Version: "2", Digest: "sha256:source-2"},
		TargetDigest: "sha256:target-2", Status: kernelagent.CompilationClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if values, err := replicaRegistry.ListCompilations(ctx, agentScope, deployment.ID); err != nil || len(values) != 1 || values[0].ID != compilation.ID {
		t.Fatalf("replica compilations = %#v, %v", values, err)
	}

	skillCatalog := skill.NewCatalogWithStore(primary)
	skillDefinition := &skill.Definition{ID: "research", Version: "1", Name: "Research", Prompt: &skill.PromptModule{Instructions: "Research with cited evidence."}}
	if err := skillCatalog.Register(ctx, skillDefinition); err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: "tenant", ID: "postgres-e2e"}
	if err := skillCatalog.Bind(ctx, &skill.Binding{ID: "research", Scope: skillScope, DeploymentID: deployment.ID, SkillID: skillDefinition.ID, SkillVersion: skillDefinition.Version, EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	replicaCatalog := skill.NewCatalogWithStore(replica)
	prompts, err := replicaCatalog.ListModelPrompts(ctx, skillScope, deployment.ID)
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != skillDefinition.ID {
		t.Fatalf("replica skill prompts = %#v, %v", prompts, err)
	}

	actionRun, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release"}, Goal: "Deploy safely", Source: RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	const proposalContenders = 20
	proposalResults := make(chan *ActionProposalResult, proposalContenders)
	proposalErrors := make(chan error, proposalContenders)
	wait = sync.WaitGroup{}
	for index := 0; index < proposalContenders; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			record := sqliteApprovalProposal(actionRun, "postgres-call-"+string(rune('a'+index)), "postgres-idempotency", "postgres-proposal-"+string(rune('a'+index)))
			record.Run.WakeCondition.Reference = record.Approval.ID
			result, proposalErr := stores[index%len(stores)].CreateActionProposal(ctx, record)
			proposalResults <- result
			proposalErrors <- proposalErr
		}(index)
	}
	wait.Wait()
	close(proposalResults)
	close(proposalErrors)
	for proposalErr := range proposalErrors {
		if proposalErr != nil {
			t.Fatal(proposalErr)
		}
	}
	createdProposals := 0
	var proposal *ActionProposalResult
	for result := range proposalResults {
		if result.Created {
			createdProposals++
			proposal = result
		}
	}
	if createdProposals != 1 || proposal == nil || proposal.Approval == nil {
		t.Fatalf("created proposals = %d, proposal=%#v", createdProposals, proposal)
	}
	approvals := NewApprovalCoordinator(replica, replica, ApprovalAuthorizerFunc(func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error { return nil }))
	approvalNow := proposal.Approval.CreatedAt.Add(time.Second)
	approvals.now = func() time.Time { return approvalNow }
	resolved, err := approvals.Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "postgres-decision", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "role", ID: "release-manager"}})
	if err != nil || !resolved.Resolved || resolved.Call.Status != ActionCallStatusReady || resolved.Run.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("resolved approval = %#v, %v", resolved, err)
	}
	replayed, err := approvals.Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "postgres-decision", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "role", ID: "release-manager"}})
	if err != nil || replayed.Resolved {
		t.Fatalf("replayed approval = %#v, %v", replayed, err)
	}
	actionClaims := make(chan *ActionCall, 2)
	actionClaimErrors := make(chan error, 2)
	wait = sync.WaitGroup{}
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			call, claimErr := stores[index].ClaimNextAction(ctx, ActionClaim{Scope: scope, WorkerID: "action-worker-" + string(rune('a'+index)), Now: approvalNow, LeaseDuration: time.Minute})
			actionClaims <- call
			actionClaimErrors <- claimErr
		}(index)
	}
	wait.Wait()
	close(actionClaims)
	close(actionClaimErrors)
	for claimErr := range actionClaimErrors {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	var claimedAction *ActionCall
	actionClaimCount := 0
	for call := range actionClaims {
		if call != nil {
			actionClaimCount++
			claimedAction = call
		}
	}
	if actionClaimCount != 1 {
		t.Fatalf("action claims = %d, want 1", actionClaimCount)
	}
	currentRun, err := primary.GetAgentRun(ctx, scope, actionRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedCall := cloneActionCall(claimedAction)
	completedCall.Status = ActionCallStatusSucceeded
	completedCall.Output = map[string]interface{}{"deployment": "complete"}
	completedCall.LeaseOwner, completedCall.LeaseExpiresAt = "", nil
	completedCall.Revision++
	completedCall.UpdatedAt = approvalNow.Add(time.Second)
	completedCall.CompletedAt = &completedCall.UpdatedAt
	resumedRun := cloneAgentRun(currentRun)
	resumedRun.Status = AgentRunStatusQueued
	resumedRun.WakeCondition = nil
	resumedRun.AvailableAt, resumedRun.QueueEnteredAt = completedCall.UpdatedAt, completedCall.UpdatedAt
	resumedRun.Revision++
	resumedRun.UpdatedAt = completedCall.UpdatedAt
	duplicateEvent := &ActivityEvent{ID: proposal.Event.ID, Scope: scope, RunID: actionRun.ID, EventType: "action.succeeded", Summary: "Deployment completed", CreatedAt: completedCall.UpdatedAt}
	execution := ActionExecutionRecord{Call: completedCall, ExpectedCallRevision: claimedAction.Revision, Run: resumedRun, ExpectedRunRevision: currentRun.Revision, WorkerID: claimedAction.LeaseOwner, Now: approvalNow, Event: duplicateEvent}
	if _, err := replica.PersistActionExecution(ctx, execution); err == nil {
		t.Fatal("duplicate activity should roll back the action outcome")
	}
	rolledBackCall, err := primary.GetActionCall(ctx, scope, claimedAction.ID)
	if err != nil || rolledBackCall.Status != ActionCallStatusRunning || rolledBackCall.Revision != claimedAction.Revision {
		t.Fatalf("rolled back call = %#v, %v", rolledBackCall, err)
	}
	execution.Event.ID = uuid.NewString()
	persistedExecution, err := primary.PersistActionExecution(ctx, execution)
	if err != nil || persistedExecution.Call.Status != ActionCallStatusSucceeded || persistedExecution.Run.Status != AgentRunStatusQueued {
		t.Fatalf("persisted execution = %#v, %v", persistedExecution, err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := primary.RollbackPostgresMigrations(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != 4 {
		t.Fatalf("rolled-back schema version = %d, %v", version, err)
	}
	var actionTables int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name IN ('action_calls','approval_checkpoints')`, primary.schema).Scan(&actionTables); err != nil || actionTables != 0 {
		t.Fatalf("action tables after rollback = %d, %v", actionTables, err)
	}
	var recentTables int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name IN ('projects','workforce_change_sets')`, primary.schema).Scan(&recentTables); err != nil || recentTables != 0 {
		t.Fatalf("recent tables after rollback = %d, %v", recentTables, err)
	}
	if err := primary.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("reapplied schema version = %d, %v", version, err)
	}
}

func TestPostgresAgentRequestAcceptanceIsAtomicAndRecoverable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_collab_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(primary)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}, AssignedAgentID: "developer",
		Goal: "Ship", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(primary)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "Launch",
		IdempotencyKey: "postgres-launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitingSource, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, source.ID)
	if err != nil || waitingSource.Status != AgentRunStatusWaitingForAgent || waitingSource.WakeCondition == nil ||
		waitingSource.WakeCondition.Type != "agent_request_decision" ||
		waitingSource.WakeCondition.Reference != created.Request.ID {
		t.Fatalf("atomically waiting source = %#v, %v", waitingSource, err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewCollaborationService(replica).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.ChildRunID != accepted.Child.ID || restored.Status != AgentRequestStatusAccepted {
		t.Fatalf("restored request = %#v, %v", restored, err)
	}
	child, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, accepted.Child.ID)
	if err != nil || child.ParentRunID != source.ID || child.AssignedAgentID != "marketing" {
		t.Fatalf("restored child = %#v, %v", child, err)
	}
	completed, err := NewCollaborationService(replica).CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: child.Revision, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
		Summary: "Launch completed", CompletionKey: "postgres-launch-complete",
	})
	if err != nil || completed.Request.Status != AgentRequestStatusCompleted || completed.Source.Status != AgentRunStatusQueued || completed.Child.Status != AgentRunStatusCompleted {
		t.Fatalf("complete request = %#v, %v", completed, err)
	}
	restored, err = NewCollaborationService(primary).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.Status != AgentRequestStatusCompleted || restored.CompletionKey != "postgres-launch-complete" {
		t.Fatalf("restored completion = %#v, %v", restored, err)
	}
	activity, err := replica.ListActivity(ctx, ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 10})
	if err != nil || len(activity) != 5 || activity[0].EventType != "collaboration.completed" {
		t.Fatalf("team activity = %#v, %v", activity, err)
	}
	if version, err := replica.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestPostgresRunCommandsAreAtomicIdempotentAndRecoverable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_commands_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := Scope{Kind: "tenant", ID: "acme"}
	request := CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator",
		Goal: "Inspect production health", Source: RunSourceManual, IdempotencyKey: "health-run",
		Actor: ActivityActor{Type: "user", ID: "7"},
	}
	services := []*RunCommandService{NewRunCommandService(primary), NewRunCommandService(replica)}
	results := make(chan *AgentRunCommandResult, len(services))
	errorsFound := make(chan error, len(services))
	var group sync.WaitGroup
	for _, service := range services {
		group.Add(1)
		go func(service *RunCommandService) {
			defer group.Done()
			result, createErr := service.CreateAgentRun(ctx, request)
			if createErr != nil {
				errorsFound <- createErr
				return
			}
			results <- result
		}(service)
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for createErr := range errorsFound {
		t.Fatal(createErr)
	}
	var run *AgentRun
	creationEvents := 0
	resultCount := 0
	for result := range results {
		resultCount++
		if run == nil {
			run = result.Run
		} else if result.Run.ID != run.ID {
			t.Fatalf("idempotent run ids differ: %s and %s", run.ID, result.Run.ID)
		}
		if result.Event != nil {
			creationEvents++
		}
	}
	if resultCount != 2 || creationEvents != 1 || run == nil {
		t.Fatalf("create results = %d, creation events = %d, run = %#v", resultCount, creationEvents, run)
	}
	conflict := request
	conflict.Goal = "Do different work"
	if _, err := services[0].CreateAgentRun(ctx, conflict); !errors.Is(err, ErrRunIdempotency) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	events, err := replica.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, Limit: 10})
	if err != nil || len(events) != 1 || events[0].EventType != "run.created" {
		t.Fatalf("creation activity = %#v, %v", events, err)
	}

	paused, err := services[1].CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: run.ID, ExpectedRevision: run.Revision, Kind: AgentRunCommandPause,
		Actor: ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil || paused.Run.Status != AgentRunStatusPaused {
		t.Fatalf("paused = %#v, %v", paused, err)
	}
	intervened, err := services[0].CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: run.ID, ExpectedRevision: paused.Run.Revision, Kind: AgentRunCommandIntervene,
		Actor: ActivityActor{Type: "user", ID: "7"}, Instruction: "Check database saturation first",
	})
	if err != nil || len(intervened.Run.PendingInterventions) != 1 {
		t.Fatalf("intervened = %#v, %v", intervened, err)
	}
	resumed, err := services[1].CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: run.ID, ExpectedRevision: intervened.Run.Revision, Kind: AgentRunCommandResume,
		Actor: ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil || resumed.Run.Status != AgentRunStatusQueued {
		t.Fatalf("resumed = %#v, %v", resumed, err)
	}
	canceled, err := services[0].CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: run.ID, ExpectedRevision: resumed.Run.Revision, Kind: AgentRunCommandCancel,
		Actor: ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil || canceled.Run.Status != AgentRunStatusCanceled || canceled.Run.CompletedAt == nil {
		t.Fatalf("canceled = %#v, %v", canceled, err)
	}
	restored, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || restored.Status != AgentRunStatusCanceled || len(restored.PendingInterventions) != 1 {
		t.Fatalf("restored = %#v, %v", restored, err)
	}
	if _, err := NewPortfolioService(primary).GetAgentRun(ctx, Scope{Kind: "tenant", ID: "other"}, run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-scope get error = %v", err)
	}
	events, err = primary.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, Limit: 10})
	if err != nil || len(events) != 5 || events[4].EventType != "run.canceled" {
		t.Fatalf("command activity = %#v, %v", events, err)
	}
}

func TestPostgresPoolBudgetBoundsConcurrentConnectionsAndExposesWaits(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_pool_budget_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn,
		WithPostgresSchema(schema),
		WithPostgresPool(PostgresPoolConfig{
			MaxOpenConnections: 4, MaxIdleConnections: 2,
			ConnectionLifetime: time.Minute, ConnectionIdleTime: time.Minute,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})

	const callers = 24
	start := make(chan struct{})
	errorsFound := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, queryErr := store.db.ExecContext(ctx, `SELECT pg_sleep(0.1)`)
			errorsFound <- queryErr
		}()
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for queryErr := range errorsFound {
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	stats := store.PoolStats()
	if stats.MaxOpenConnections != 4 || stats.OpenConnections > 4 || stats.InUseConnections > 4 || stats.WaitCount < callers-4 || stats.WaitDuration <= 0 {
		t.Fatalf("bounded pool stats = %#v", stats)
	}
}

func TestPostgresPoolBudgetBoundsTwoOverlappingReplicas(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_pool_replicas_" + uuid.NewString()[:8]
	pool := PostgresPoolConfig{
		MaxOpenConnections: 3, MaxIdleConnections: 1,
		ConnectionLifetime: time.Minute, ConnectionIdleTime: time.Minute,
	}
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema), WithPostgresPool(pool))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema), WithPostgresPool(pool))
	if err != nil {
		_ = primary.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = replacement.Close()
		_ = primary.Close()
	})

	const callersPerReplica = 15
	start := make(chan struct{})
	errorsFound := make(chan error, callersPerReplica*2)
	var group sync.WaitGroup
	for _, store := range []*PostgresStore{primary, replacement} {
		for index := 0; index < callersPerReplica; index++ {
			group.Add(1)
			go func(store *PostgresStore) {
				defer group.Done()
				<-start
				_, queryErr := store.db.ExecContext(ctx, `SELECT pg_sleep(0.1)`)
				errorsFound <- queryErr
			}(store)
		}
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for queryErr := range errorsFound {
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	primaryStats, replacementStats := primary.PoolStats(), replacement.PoolStats()
	for name, stats := range map[string]PostgresPoolStats{"primary": primaryStats, "replacement": replacementStats} {
		if stats.MaxOpenConnections != 3 || stats.OpenConnections > 3 || stats.InUseConnections > 3 || stats.IdleConnections > 1 || stats.WaitCount < callersPerReplica-3 || stats.WaitDuration <= 0 {
			t.Fatalf("%s replica pool stats = %#v", name, stats)
		}
	}
	if primaryStats.OpenConnections+replacementStats.OpenConnections > 6 {
		t.Fatalf("overlapping replica connections exceed aggregate budget: primary=%#v replacement=%#v", primaryStats, replacementStats)
	}
}
