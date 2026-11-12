package sqlite

import (
	"context"
	"sync"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// runLoopCutoverLocalMu serializes the cutover inside one process for the
// SQLite store. A SQLite store is opened by one process (the
// desktop daemon); its write lock cannot be held across the cutover's own
// writes, which use other connections.
var runLoopCutoverLocalMu sync.Mutex

func withRunLoopCutoverLocalLock(ctx context.Context, fn func(context.Context) error) error {
	runLoopCutoverLocalMu.Lock()
	defer runLoopCutoverLocalMu.Unlock()
	return fn(ctx)
}

func (s *SQLiteStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	return withRunLoopCutoverLocalLock(ctx, fn)
}

func (s *SQLiteStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]models.RunLoopCutoverRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id FROM agent_runs
		WHERE status NOT IN ('completed', 'failed', 'canceled') AND COALESCE(json_extract(payload, '$.engine'), '') = ''
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return sqlstore.ScanRunLoopCutoverRefs(rows)
}
