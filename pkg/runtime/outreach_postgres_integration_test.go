package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresOutreachIsReplicaSafeAndRestartDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := "outreach_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := primary.db.QueryRowContext(ctx, `SELECT name FROM `+primary.table("schema_migrations")+` WHERE version=18`).Scan(&migration); err != nil || migration != "governed outreach threads" {
		t.Fatalf("migration=%q err=%v", migration, err)
	}
	scope := Scope{Kind: "tenant", ID: "postgres-outreach"}
	initiative, runs := seedExecutableMonitorInitiative(t, primary, scope)
	sourceService := NewSourceMonitorService(primary, primary, primary, primary)
	observation, err := sourceService.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-1", "PostgreSQL evidence"))
	if err != nil {
		t.Fatal(err)
	}
	disclosure := "Disclosure: I work on OpenSeal."
	body := "Which workflow was difficult? " + disclosure
	draft := &OutreachThread{
		Scope: scope, InitiativeID: initiative.ID, SourceObservationID: observation.Observation.ID, MonitorID: "monitor-a", StableSourceID: "thread-1",
		TargetURI: observation.Observation.SourceURI, Owner: initiative.Owner, AssignedAgentID: "researcher", SourcePolicyRef: "approved-forums", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile:research", DisplayName: "Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Messages: []OutreachMessage{{Direction: OutreachMessageOutbound, Intent: OutreachIntentRequestFeedback, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1", Action: "reply", TargetArgument: "url", BodyArgument: "body", Arguments: map[string]interface{}{"url": observation.Observation.SourceURI, "body": body}}}},
	}
	type createResult struct {
		thread *OutreachThread
		err    error
	}
	results := make(chan createResult, 2)
	for _, store := range []*PostgresStore{primary, replica} {
		go func(candidate *PostgresStore) {
			service := NewOutreachService(candidate, candidate, candidate, candidate)
			thread, _, createErr := service.Create(ctx, CreateOutreachThreadRequest{Thread: draft, IdempotencyKey: "same-draft"})
			results <- createResult{thread, createErr}
		}(store)
	}
	ids := map[string]bool{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		ids[result.thread.ID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("idempotent create ids=%#v", ids)
	}
	var threadID string
	for id := range ids {
		threadID = id
	}
	current, err := primary.GetOutreachThread(ctx, scope, threadID)
	if err != nil {
		t.Fatal(err)
	}
	updated := cloneOutreachThread(current)
	updated.Revision, updated.UpdatedAt = 2, time.Now().UTC()
	updated.Messages[0].Status, updated.Messages[0].RunID, updated.Messages[0].ActionCallID = OutreachMessageReady, "run-1", "action-1"
	updated.Messages[0].UpdatedAt = updated.UpdatedAt
	event := outreachEvent(updated, &updated.Messages[0], "outreach.action_linked", "Outreach message ready for governed delivery", ActivityActor{Type: "worker", ID: "worker"}, ActivityVisibilityScope, updated.UpdatedAt)
	type updateResult struct{ err error }
	updates := make(chan updateResult, 2)
	for _, store := range []*PostgresStore{primary, replica} {
		go func(candidate *PostgresStore) {
			_, updateErr := candidate.UpdateOutreachThreadWithEvent(ctx, updated, 1, event)
			updates <- updateResult{updateErr}
		}(store)
	}
	succeeded, conflicted := 0, 0
	for range 2 {
		result := <-updates
		if result.err == nil {
			succeeded++
		} else if result.err == ErrOutreachThreadConflict {
			conflicted++
		} else {
			t.Fatal(result.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("updates succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	primary, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := primary.GetOutreachThread(ctx, scope, threadID)
	listed, listErr := primary.ListOutreachThreads(ctx, OutreachThreadFilter{Scope: scope, SourceObservationID: observation.Observation.ID, Limit: 10})
	if err != nil || listErr != nil || restarted.Revision != 2 || restarted.Messages[0].ActionCallID != "action-1" || len(listed) != 1 {
		t.Fatalf("restarted=%#v listed=%#v err=%v listErr=%v", restarted, listed, err, listErr)
	}
}
