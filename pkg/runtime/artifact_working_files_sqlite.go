package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// SQLite has no ledger: the table is created, if missing, on every open.
const sqliteWorkingFilesTable = `
	CREATE TABLE IF NOT EXISTS artifact_working_files (
		scope_kind TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		id TEXT NOT NULL,
		marked_at DATETIME NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id)
	);`

func (s *SQLiteStore) MarkArtifactFiles(ctx context.Context, scope models.Scope, working, results []string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, id := range working {
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_working_files (scope_kind, scope_id, id, marked_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (scope_kind, scope_id, id) DO NOTHING`, scope.Kind, scope.ID, id, now); err != nil {
			return err
		}
	}
	for _, id := range results {
		if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_working_files WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var _ models.ArtifactWorkingFileStore = (*SQLiteStore)(nil)
