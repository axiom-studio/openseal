package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresSourceMonitorIngestIsReplicaSafeAndRestartDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := "source_monitor_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if primary != nil {
			_ = primary.Close()
		}
	}()
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	var migration string
	if err = primary.db.QueryRowContext(ctx, `SELECT name FROM `+primary.table("schema_migrations")+` WHERE version=16`).Scan(&migration); err != nil || migration != "source monitor observations and checkpoints" {
		t.Fatalf("migration=%q err=%v", migration, err)
	}
	scope := Scope{Kind: "tenant", ID: "postgres-monitor"}
	initiative, runs := seedExecutableMonitorInitiative(t, primary, scope)
	request := sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-1", "PostgreSQL finding")
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	expires := base.Add(time.Hour)
	request.RetentionExpiresAt = &expires
	firstService := NewSourceMonitorService(primary, primary, primary, primary)
	secondService := NewSourceMonitorService(replica, replica, replica, replica)
	firstService.now = func() time.Time { return base }
	secondService.now = func() time.Time { return base }
	type outcome struct {
		result *SourceObservationIngestResult
		err    error
	}
	results := make(chan outcome, 2)
	go func() { result, ingestErr := firstService.Ingest(ctx, request); results <- outcome{result, ingestErr} }()
	go func() { result, ingestErr := secondService.Ingest(ctx, request); results <- outcome{result, ingestErr} }()
	succeeded, replayed, conflicted := 0, 0, 0
	for range 2 {
		value := <-results
		if value.err == nil {
			succeeded++
			if value.result.Replayed {
				replayed++
			}
		} else if value.err == ErrSourceMonitorCheckpoint {
			conflicted++
		} else {
			t.Fatal(value.err)
		}
	}
	if succeeded == 0 || succeeded+conflicted != 2 {
		t.Fatalf("succeeded=%d replayed=%d conflicted=%d", succeeded, replayed, conflicted)
	}
	noChangeRequest := checkpointRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 1, "cursor-1", "no-change-postgres")
	type checkpointOutcome struct {
		result *SourceMonitorCheckpointResult
		err    error
	}
	checkpointResults := make(chan checkpointOutcome, 2)
	go func() {
		result, checkpointErr := firstService.AdvanceCheckpoint(ctx, noChangeRequest)
		checkpointResults <- checkpointOutcome{result, checkpointErr}
	}()
	go func() {
		result, checkpointErr := secondService.AdvanceCheckpoint(ctx, noChangeRequest)
		checkpointResults <- checkpointOutcome{result, checkpointErr}
	}()
	checkpointSucceeded, checkpointReplayed := 0, 0
	for range 2 {
		value := <-checkpointResults
		if value.err != nil {
			t.Fatal(value.err)
		}
		checkpointSucceeded++
		if value.result.Replayed {
			checkpointReplayed++
		}
	}
	if checkpointSucceeded != 2 || checkpointReplayed != 1 {
		t.Fatalf("checkpoint succeeded=%d replayed=%d", checkpointSucceeded, checkpointReplayed)
	}
	primary.Close()
	primary, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := primary.GetSourceMonitorCheckpoint(ctx, scope, initiative.ID, "monitor-a")
	observations, listErr := primary.ListSourceObservations(ctx, SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, MonitorID: "monitor-a"})
	if err != nil || listErr != nil || checkpoint.Revision != 2 || checkpoint.ObservationCount != 1 || checkpoint.LastActionCallID != "no-change-postgres" || len(observations) != 1 {
		t.Fatalf("checkpoint=%#v observations=%#v err=%v listErr=%v", checkpoint, observations, err, listErr)
	}
	retainedService := NewSourceMonitorService(primary, primary, primary, primary)
	retainedService.now = func() time.Time { return expires }
	retained, retainedErr := retainedService.List(ctx, SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, MonitorID: "monitor-a"})
	if retainedErr != nil || len(retained) != 0 {
		t.Fatalf("expired PostgreSQL evidence remained product-visible: values=%#v err=%v", retained, retainedErr)
	}
}
