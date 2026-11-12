//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"
)

func TestPostgresChannelTimelinePaginatesRootsAndBroadcastsBeforeLimiting(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "openseal_channel_timeline_")
	dsn := pgSchema.dsn
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	testChannelTimelineStore(t, store)
}
