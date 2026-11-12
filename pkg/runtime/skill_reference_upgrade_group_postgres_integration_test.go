//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresSkillReferenceUpgradeMovesEveryBindingAtTheVersionTogether(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_group_upgrade_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	runGroupUpgradeScenario(t, store)
}
