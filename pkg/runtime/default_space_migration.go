package runtime

import (
	"context"
	"database/sql"
	"strings"
)

// defaultSpaceMigrationVersion renames each scope's Personal space to its
// Default space: the column is_personal becomes is_default, the id prefix
// personal- becomes default-, and a space still named Personal is named
// Default. Members, conversations and artifact provenance follow the new id.
const defaultSpaceMigrationVersion int64 = 78

// defaultSpaceRewrites move every personal-<md5> space to default-<md5>. The
// old rows give up is_default first so the new rows fit the one-per-scope
// index; members move before the old rows go, so the cascade drops nothing.
func defaultSpaceRewrites(spaces, members, conversations, artifacts string, postgres bool) []string {
	newID := func(column string) string { return `'default-' || substr(` + column + `, 10)` }
	isTrue, isFalse := "1", "0"
	conversationPayload := `json_set(payload, '$.spaceId', ` + newID("space_id") + `)`
	artifactSpaceID := `json_extract(payload, '$.provenance.spaceId')`
	artifactPayload := `json_set(payload, '$.provenance.spaceId', ` + newID(artifactSpaceID) + `)`
	if postgres {
		isTrue, isFalse = "TRUE", "FALSE"
		conversationPayload = `jsonb_set(payload, '{spaceId}', to_jsonb(` + newID("space_id") + `), true)`
		artifactSpaceID = `(payload->'provenance'->>'spaceId')`
		artifactPayload = `jsonb_set(payload, '{provenance,spaceId}', to_jsonb(` + newID(artifactSpaceID) + `), true)`
	}
	return []string{
		`UPDATE ` + spaces + ` SET is_default = ` + isFalse + ` WHERE id LIKE 'personal-%'`,
		`INSERT INTO ` + spaces + ` (id, scope_kind, scope_id, name, look, position, is_default, created_by, created_at, updated_at)
			SELECT ` + newID("id") + `, scope_kind, scope_id, CASE WHEN name = 'Personal' THEN 'Default' ELSE name END,
				look, position, ` + isTrue + `, created_by, created_at, updated_at
			FROM ` + spaces + ` WHERE id LIKE 'personal-%'`,
		`UPDATE ` + members + ` SET space_id = ` + newID("space_id") + ` WHERE space_id LIKE 'personal-%'`,
		`UPDATE ` + conversations + ` SET space_id = ` + newID("space_id") + `, payload = ` + conversationPayload + `
			WHERE space_id LIKE 'personal-%'`,
		`UPDATE ` + artifacts + ` SET payload = ` + artifactPayload + ` WHERE ` + artifactSpaceID + ` LIKE 'personal-%'`,
		`DELETE FROM ` + spaces + ` WHERE id LIKE 'personal-%'`,
	}
}

// migrateDefaultSpace is PostgreSQL migration 78.
func (s *PostgresStore) migrateDefaultSpace(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, defaultSpaceMigrationVersion)
	if err != nil || applied {
		return err
	}
	var legacy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'spaces' AND column_name = 'is_personal')`, s.schema).Scan(&legacy); err != nil {
		return err
	}
	var statements []string
	if legacy {
		statements = append(statements,
			`ALTER TABLE `+s.table("spaces")+` RENAME COLUMN is_personal TO is_default`,
			`ALTER INDEX IF EXISTS `+s.table("spaces_personal_idx")+` RENAME TO spaces_default_idx`,
		)
	}
	statements = append(statements, defaultSpaceRewrites(s.table("spaces"), s.table("space_members"), s.table("conversations"), s.table("artifacts"), true)...)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'rename the Personal space to the Default space') ON CONFLICT (version) DO NOTHING`, defaultSpaceMigrationVersion)
	return err
}

// migrateDefaultSpaceSQLite mirrors PostgreSQL migration 78. It runs before
// the spaces tables are ensured, whose index names is_default, and does
// nothing once is_personal is gone.
func migrateDefaultSpaceSQLite(db *sql.DB) error {
	legacy, err := sqliteColumnExists(db, "spaces", "is_personal")
	if err != nil || !legacy {
		return err
	}
	// The artifact store may keep its own database.
	artifacts, err := sqliteColumnExists(db, "artifacts", "payload")
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := append([]string{
		`DROP INDEX IF EXISTS idx_spaces_personal`,
		`ALTER TABLE spaces RENAME COLUMN is_personal TO is_default`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_spaces_default ON spaces(scope_kind, scope_id) WHERE is_default = 1`,
	}, defaultSpaceRewrites("spaces", "space_members", "conversations", "artifacts", false)...)
	if !artifacts {
		statements = removeArtifactRewrite(statements)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func removeArtifactRewrite(statements []string) []string {
	kept := statements[:0]
	for _, statement := range statements {
		if !strings.HasPrefix(statement, "UPDATE artifacts ") {
			kept = append(kept, statement)
		}
	}
	return kept
}
