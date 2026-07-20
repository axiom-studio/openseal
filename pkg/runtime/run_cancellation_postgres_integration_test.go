//go:build integration

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresCanceledApprovalOutreachRepairsAfterRestart(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := "cancel_outreach_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
			_ = store.Close()
		}
	})
	assertCanceledApprovalOutreach(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	outreach := NewOutreachService(store, store, store, store)
	restarted, err := outreach.Get(ctx, Scope{Kind: "tenant", ID: "cancellation"}, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Messages[0].Status != OutreachMessageCanceled || restarted.Revision != 2 {
		t.Fatalf("restarted outreach = %#v", restarted)
	}
}
