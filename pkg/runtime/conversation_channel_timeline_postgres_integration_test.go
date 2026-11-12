//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresChannelTimelinePaginatesRootsAndBroadcastsBeforeLimiting(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_channel_timeline_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	testChannelTimelineStore(t, store)
}
