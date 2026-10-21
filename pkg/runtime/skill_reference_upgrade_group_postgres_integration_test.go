//go:build integration

package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresSkillReferenceUpgradeMovesEveryBindingAtTheVersionTogether(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_group_upgrade_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	runGroupUpgradeScenario(t, store)
}
