//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"
)

func TestPostgresSkillReferenceUpgradeMovesEveryBindingAtTheVersionTogether(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "openseal_group_upgrade_")
	dsn := pgSchema.dsn
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	runGroupUpgradeScenario(t, store)
}
