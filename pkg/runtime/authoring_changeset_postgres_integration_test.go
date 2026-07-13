//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

type postgresRetryFailureGenerator struct{}

func (postgresRetryFailureGenerator) Generate(context.Context, authoring.GenerateRequest) ([]byte, error) {
	return nil, errors.New("provider unavailable")
}

func TestPostgresWorkforceAuthoringRecoveryScopesArePagedAndDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_recovery_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	compiler, _ := authoring.NewCompiler(testAuthoringGenerator(t))
	service, err := NewWorkforceAuthoringRunService(compiler, primary)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		request := testPrepareWorkforceRequest()
		request.Scope = capability.ScopeReference{Kind: "tenant", ID: id}
		request.IdempotencyKey = "postgres-recovery-" + id
		if _, _, _, err := service.Prepare(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	first, err := service.ListRecoveryScopes(ctx, Scope{}, 2)
	if err != nil || len(first) != 2 || first[0].ID != "alpha" || first[1].ID != "beta" {
		t.Fatalf("first recovery page = %#v, %v", first, err)
	}

	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	restarted, err := NewWorkforceAuthoringRunService(compiler, replica)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.ListRecoveryScopes(ctx, first[len(first)-1], 2)
	if err != nil || len(second) != 1 || second[0].ID != "gamma" {
		t.Fatalf("restart recovery page = %#v, %v", second, err)
	}
}

func TestPostgresGenerationRetryReceiptIsReplicaSafeAcrossRestart(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_retry_" + uuid.NewString()[:8]
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
	compiler, _ := authoring.NewCompiler(postgresRetryFailureGenerator{})
	primaryService, _ := authoring.NewChangeSetService(compiler, primary)
	replicaService, _ := authoring.NewChangeSetService(compiler, replica)
	request := authoring.CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "failed-generation",
	}
	prepared, _, err := primaryService.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := primaryService.GeneratePrepared(ctx, prepared.Scope, prepared.ID, prepared.Revision)
	if err == nil || failed.Status != authoring.ChangeSetFailed {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	retry := authoring.RetryChangeSetGenerationRequest{
		Scope: failed.Scope, ChangeSetID: failed.ID, ExpectedRevision: failed.Revision, Reason: "provider recovered",
		Actor: failed.Actor, IdempotencyKey: "retry-one",
	}
	services := []*authoring.ChangeSetService{primaryService, replicaService}
	var replayCount atomic.Int32
	var wait sync.WaitGroup
	for _, service := range services {
		wait.Add(1)
		go func(service *authoring.ChangeSetService) {
			defer wait.Done()
			value, replayed, retryErr := service.RetryGeneration(ctx, retry)
			if retryErr != nil || value == nil || value.Status != authoring.ChangeSetEvaluating || len(value.Generation.Retries) != 1 {
				t.Errorf("retry=%#v replay=%t err=%v", value, replayed, retryErr)
			}
			if replayed {
				replayCount.Add(1)
			}
		}(service)
	}
	wait.Wait()
	if replayCount.Load() != 1 {
		t.Fatalf("replica replay count=%d", replayCount.Load())
	}
	_ = replica.Close()
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedService, _ := authoring.NewChangeSetService(compiler, reopened)
	value, replayed, err := reopenedService.RetryGeneration(ctx, retry)
	if err != nil || !replayed || value.Generation.Retries[0].ExpectedRevision != failed.Revision {
		t.Fatalf("restart replay=%#v replayed=%t err=%v", value, replayed, err)
	}
}

func TestPostgresWorkforceChangeSetsAreReplicaSafeAndDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_changeset_" + uuid.NewString()[:8]
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
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != 13 {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	value := testWorkforceChangeSet(scope, "change-one")
	stores := []*PostgresStore{primary, replica}
	var created, replayed atomic.Int32
	var wait sync.WaitGroup
	for _, store := range stores {
		wait.Add(1)
		go func(store *PostgresStore) {
			defer wait.Done()
			result, replay, err := store.CreateChangeSet(ctx, value, "intent-one", "request-one")
			if err != nil || result == nil || result.ID != value.ID {
				t.Errorf("create = %#v, replay = %t, err = %v", result, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}(store)
	}
	wait.Wait()
	if created.Load() != 1 || replayed.Load() != 1 {
		t.Fatalf("created = %d, replayed = %d", created.Load(), replayed.Load())
	}
	if _, _, err := replica.GetChangeSetByIdempotency(ctx, scope, "intent-one", "different"); !errors.Is(err, authoring.ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	restored, err := replica.GetChangeSet(ctx, scope, value.ID)
	if err != nil || restored.CandidateDigest != value.CandidateDigest {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
	updated := *restored
	updated.Status, updated.Revision, updated.UpdatedAt = authoring.ChangeSetReady, 2, restored.UpdatedAt.Add(time.Minute)
	updated.ApprovalDecisions = []authoring.ChangeSetApprovalDecision{{ID: "decision", EvaluationID: "evaluation", PolicyID: "production", Role: "operator", Approved: true, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, DecidedAt: updated.UpdatedAt}}
	if persisted, err := primary.UpdateChangeSet(ctx, &updated, 1); err != nil || persisted.Revision != 2 || len(persisted.ApprovalDecisions) != 1 {
		t.Fatalf("update = %#v, err = %v", persisted, err)
	}
	stale := updated
	stale.Status, stale.Revision = authoring.ChangeSetRejected, 3
	if _, err := replica.UpdateChangeSet(ctx, &stale, 1); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale replica update = %v", err)
	}
	modifiedCandidate := updated
	modifiedCandidate.CandidateDigest, modifiedCandidate.Revision = "changed", 3
	if _, err := replica.UpdateChangeSet(ctx, &modifiedCandidate, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("candidate mutation = %v", err)
	}
}

func TestPostgresAtomicWorkforceApplyHasOneReplicaWinner(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_apply_" + uuid.NewString()[:8]
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
	registerInitiativeSourceSkill(t, primary)
	ready := testInitiativeWorkforceChangeSet()
	if _, _, err = primary.CreateChangeSet(ctx, ready, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	candidate := cloneRuntimeChangeSet(ready)
	candidate.Status = authoring.ChangeSetApplied
	candidate.Revision = 3
	candidate.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: ready.CandidateDigest, Actor: ready.Actor, AppliedAt: ready.UpdatedAt.Add(time.Minute)}
	candidate.UpdatedAt = candidate.ApplyReceipt.AppliedAt
	stores := []*PostgresStore{primary, replica}
	results := make(chan *authoring.ChangeSet, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *PostgresStore) {
			defer wg.Done()
			result, err := store.ApplyChangeSet(ctx, cloneRuntimeChangeSet(candidate), 2)
			results <- result
			errs <- err
		}(store)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.ApplyReceipt == nil || result.ApplyReceipt.ID != "receipt" || result.Status != authoring.ChangeSetApplied {
			t.Fatalf("result=%#v", result)
		}
	}
	if versions, err := replica.ListDefinitionVersions(ctx, "agent"); err != nil || len(versions) != 1 {
		t.Fatalf("versions=%d err=%v", len(versions), err)
	}
	if objectives, err := replica.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: "tenant", ID: "one"}}); err != nil || len(objectives) != 2 {
		t.Fatalf("objectives=%d err=%v", len(objectives), err)
	}
	initiative, err := replica.GetInitiative(ctx, Scope{Kind: "tenant", ID: "one"}, ready.Placement.InitiativeID)
	if err != nil || initiative.Revision != 1 || len(initiative.SourceMonitors) != 1 || initiative.SourceMonitors[0].AssignedAgentID != "agent-live" {
		t.Fatalf("Initiative=%#v err=%v", initiative, err)
	}
}
