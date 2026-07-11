package runtime

import (
	"context"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
)

func TestPostgresInitiativeRestartAndAtomicActivity(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := "initiative_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	seedInitiativeObjectives(t, store, scope)
	svc := NewInitiativeService(store, store)
	created, event, err := svc.Create(ctx, CreateInitiativeRequest{Initiative: initiativeFixture(scope), IdempotencyKey: "restart-key", Actor: ActivityActor{Type: "user", ID: "u1"}})
	if err != nil || event == nil {
		t.Fatalf("create event=%#v err=%v", event, err)
	}
	store.Close()
	store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	got, err := store.GetInitiative(ctx, scope, created.ID)
	if err != nil || got.Revision != 1 {
		t.Fatalf("restart initiative=%#v err=%v", got, err)
	}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}
	listed, err := store.ListInitiatives(ctx, InitiativeFilter{Scope: scope, Owner: &owner, Statuses: []InitiativeStatus{InitiativeStatusDraft}, ObjectiveID: "objective-a", Limit: 1})
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("filtered list=%#v err=%v", listed, err)
	}
	feed, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, Descending: true, Limit: 10})
	if err != nil || len(feed) != 1 || feed[0].InitiativeID != created.ID {
		t.Fatalf("restart activity=%#v err=%v", feed, err)
	}
	got.Status = InitiativeStatusActive
	got.Revision = 1
	updated, _, err := NewInitiativeService(store, store).Update(ctx, got, 1, ActivityActor{Type: "user", ID: "u1"}, ActivityVisibilityScope)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update=%#v err=%v", updated, err)
	}
}
