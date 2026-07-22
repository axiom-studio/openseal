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

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/source"
	"github.com/google/uuid"
)

func TestPostgresSourcePolicyLifecycleIsReplicaSafeAndRestartDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_source_policy_" + uuid.NewString()[:8]
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
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}

	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	primaryService, _ := source.NewLifecycleService(primary)
	replicaService, _ := source.NewLifecycleService(replica)
	for _, version := range []string{"1", "2", "3"} {
		registerSourcePolicyVersion(t, ctx, primaryService, scope, version)
	}
	if _, err := primaryService.Activate(ctx, source.ActivateRequest{
		Scope: scope, PolicyID: "community-research", Version: "1", ExpectedRevision: 0,
		ActorType: "user", ActorID: "operator", Reason: "reviewed initial source authority",
	}); err != nil {
		t.Fatal(err)
	}

	services := []*source.LifecycleService{primaryService, replicaService}
	versions := []string{"2", "3"}
	var winners atomic.Int32
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, activateErr := services[index].Activate(ctx, source.ActivateRequest{
				Scope: scope, PolicyID: "community-research", Version: versions[index], ExpectedRevision: 1,
				ActorType: "user", ActorID: "operator", Reason: "concurrent reviewed replacement",
			})
			if activateErr == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(activateErr, source.ErrPolicyRevision) {
				t.Errorf("concurrent activation error = %v", activateErr)
			}
		}(index)
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("concurrent activation winners = %d, want 1", winners.Load())
	}

	current, err := replicaService.GetLifecycle(ctx, scope, "community-research")
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.PreviousVersion != "1" {
		t.Fatalf("replica lifecycle = %#v", current)
	}
	if _, err := replicaService.Revoke(ctx, source.RevokeRequest{
		Scope: scope, PolicyID: current.PolicyID, ExpectedRevision: current.Revision,
		ActorType: "user", ActorID: "operator", Reason: "source authority withdrawn",
	}); err != nil {
		t.Fatal(err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedService, _ := source.NewLifecycleService(restarted)
	if _, _, err := restartedService.ResolveActive(ctx, scope, current.PolicyID); !errors.Is(err, source.ErrPolicyRevoked) {
		t.Fatalf("resolution after restart = %v, want %v", err, source.ErrPolicyRevoked)
	}
	events, err := restartedService.ListEvents(ctx, scope, current.PolicyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].PolicyRevision != 3 {
		t.Fatalf("events after restart = %#v", events)
	}
}
