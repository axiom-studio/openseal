package runtime

import (
	"context"
	"database/sql"
	"time"
)

// ArtifactFiles narrows an artifact list by what a Run made a file for.
//
// A Run saves every file it writes, but its reply hands over only the ones it
// names as its result or shows in the chat. The rest (scripts, drafts, the
// panels of a composite) were made only to build that result. They stay in the
// catalog as working files, so a later Run can still use them, and a list can
// leave them out.
type ArtifactFiles string

const (
	// ArtifactFilesAll lists results and working files alike.
	ArtifactFilesAll ArtifactFiles = ""
	// ArtifactFilesResults leaves working files out. A file nothing has
	// marked (an upload, or one saved before files were marked) is a result.
	ArtifactFilesResults ArtifactFiles = "results"
	// ArtifactFilesWorking lists only working files.
	ArtifactFilesWorking ArtifactFiles = "working"
)

func (f ArtifactFiles) Valid() bool {
	return f == ArtifactFilesAll || f == ArtifactFilesResults || f == ArtifactFilesWorking
}

func (f ArtifactFiles) Keeps(working bool) bool {
	switch f {
	case ArtifactFilesResults:
		return !working
	case ArtifactFilesWorking:
		return working
	}
	return true
}

// ArtifactWorkingFileStore is an ArtifactStore that remembers which files are
// working files. Marking is by artifact id, across its versions: handing any
// version over makes the file a result again.
type ArtifactWorkingFileStore interface {
	MarkArtifactFiles(ctx context.Context, scope Scope, working, results []string) error
}

// markRunFiles records which of a Run's saved files its reply handed over and
// which it did not. It is a no-op for a store that keeps no such record.
func markRunFiles(ctx context.Context, store ArtifactStore, scope Scope, saved []savedArtifact, handedOver []ConversationReference) error {
	marker, ok := store.(ArtifactWorkingFileStore)
	if !ok || len(saved) == 0 {
		return nil
	}
	handed := make(map[string]bool, len(handedOver))
	for _, ref := range handedOver {
		if ref.Kind == ConversationReferenceArtifact {
			handed[ref.ID] = true
		}
	}
	seen := make(map[string]bool, len(saved))
	var working, results []string
	for _, file := range saved {
		id := file.Ref.ID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if handed[id] {
			results = append(results, id)
		} else {
			working = append(working, id)
		}
	}
	return marker.MarkArtifactFiles(ctx, scope, working, results)
}

// artifactWorkingFilesMigrationVersion adds the artifact_working_files table.
// It is its own migration, not part of the artifact catalog's (8): a database
// whose ledger is already current skips every earlier migration, so a table
// added to an old one would never be created there.
const artifactWorkingFilesMigrationVersion int64 = 83

// migrateArtifactWorkingFiles is PostgreSQL migration 83.
func (s *PostgresStore) migrateArtifactWorkingFiles(ctx context.Context, tx *sql.Tx) error {
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

// SQLite has no ledger: the table is created, if missing, on every open.
const sqliteWorkingFilesTable = `
	CREATE TABLE IF NOT EXISTS artifact_working_files (
		scope_kind TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		id TEXT NOT NULL,
		marked_at DATETIME NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id)
	);`

func (s *SQLiteStore) MarkArtifactFiles(ctx context.Context, scope Scope, working, results []string) error {
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

func (s *PostgresStore) MarkArtifactFiles(ctx context.Context, scope Scope, working, results []string) error {
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

// workingArtifactIDs reads a scope's working files for the list paths that
// filter in Go.
func workingArtifactIDs(ctx context.Context, db *sql.DB, query string, scope Scope) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, query, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

var (
	_ ArtifactWorkingFileStore = (*SQLiteStore)(nil)
	_ ArtifactWorkingFileStore = (*PostgresStore)(nil)
)
