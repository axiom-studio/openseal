package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

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
	firstService := NewSourceMonitorService(primary, primary, primary, primary)
	secondService := NewSourceMonitorService(replica, replica, replica, replica)
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
	primary.Close()
	primary, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := primary.GetSourceMonitorCheckpoint(ctx, scope, initiative.ID, "monitor-a")
	observations, listErr := primary.ListSourceObservations(ctx, SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, MonitorID: "monitor-a"})
	if err != nil || listErr != nil || checkpoint.ObservationCount != 1 || len(observations) != 1 {
		t.Fatalf("checkpoint=%#v observations=%#v err=%v listErr=%v", checkpoint, observations, err, listErr)
	}
}
