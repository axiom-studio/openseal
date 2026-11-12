package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// artifactWorkingFilesMigrationVersion adds the artifact_working_files table.
// It is its own migration, not part of the artifact catalog's (8): a database
// whose ledger is already current skips every earlier migration, so a table
// added to an old one would never be created there.
const artifactWorkingFilesMigrationVersion int64 = 83

// migrateArtifactWorkingFiles is PostgreSQL migration 83.
func (s *Store) migrateArtifactWorkingFiles(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, artifactWorkingFilesMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("artifact_working_files")+` (
		scope_kind TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		id TEXT NOT NULL,
		marked_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id)
	)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'artifact working files') ON CONFLICT (version) DO NOTHING`, artifactWorkingFilesMigrationVersion)
	return err
}

func (s *Store) MarkArtifactFiles(ctx context.Context, scope models.Scope, working, results []string) error {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("artifact_working_files")+` (scope_kind, scope_id, id, marked_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (scope_kind, scope_id, id) DO NOTHING`, scope.Kind, scope.ID, id, now); err != nil {
			return err
		}
	}
	for _, id := range results {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("artifact_working_files")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var _ models.ArtifactWorkingFileStore = (*Store)(nil)
