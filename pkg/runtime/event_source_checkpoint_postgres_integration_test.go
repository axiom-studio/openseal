//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresEventSourceCheckpointIsReplicaSafeAndRestartDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_event_source_" + uuid.NewString()[:8]
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

	scope := Scope{Kind: "tenant", ID: "postgres"}
	services := []*EventSourceCheckpointService{NewEventSourceCheckpointService(primary), NewEventSourceCheckpointService(replica)}
	var wait sync.WaitGroup
	errs := make(chan error, 2)
	for index, service := range services {
		wait.Add(1)
		go func(index int, service *EventSourceCheckpointService) {
			defer wait.Done()
			_, advanceErr := service.Advance(ctx, AdvanceEventSourceCheckpointRequest{
				Scope: scope, Source: "kubernetes:cluster:3", SubscriptionID: "default", EventIDs: []string{eventCheckpointTestID(index)},
			})
			errs <- advanceErr
		}(index, service)
	}
	wait.Wait()
	close(errs)
	succeeded, conflicted := 0, 0
	for advanceErr := range errs {
		if advanceErr == nil {
			succeeded++
		} else if errors.Is(advanceErr, ErrEventSourceCheckpointConflict) {
			conflicted++
		} else {
			t.Fatal(advanceErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	checkpoint, err := NewEventSourceCheckpointService(restarted).Get(ctx, scope, "kubernetes:cluster:3", "default")
	if err != nil || checkpoint == nil || checkpoint.Revision != 1 || len(checkpoint.RecentEventIDs) != 1 {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	if version, versionErr := restarted.PostgresSchemaVersion(ctx); versionErr != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, versionErr)
	}
}
