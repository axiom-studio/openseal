package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const skillBindingTombstoneMigrationVersion int64 = 60

// migrateSkillBindingTombstones adds the revision floor of deleted Skill
// bindings. A binding later created under the same ID starts above it, so a
// reference to the deleted binding can never match its successor.
func (s *PostgresStore) migrateSkillBindingTombstones(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, skillBindingTombstoneMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("skill_binding_tombstones")+` (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, deployment_id TEXT NOT NULL, id TEXT NOT NULL,
		revision BIGINT NOT NULL, deleted_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, deployment_id, id)
	)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version,name) VALUES ($1,'deleted Skill binding revision floors') ON CONFLICT (version) DO NOTHING`, skillBindingTombstoneMigrationVersion)
	return err
}

type postgresExecer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func (s *PostgresStore) SkillBindingRevisionFloor(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string) (int64, error) {
	return s.skillBindingRevisionFloor(ctx, s.db, scope, deploymentID, bindingID, false)
}

func (s *PostgresStore) skillBindingRevisionFloor(ctx context.Context, query postgresMigrationQuerier, scope skill.ScopeReference, deploymentID, bindingID string, lock bool) (int64, error) {
	statement := `SELECT revision FROM ` + s.table("skill_binding_tombstones") + ` WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND id=$4`
	if lock {
		statement += ` FOR UPDATE`
	}
	var floor int64
	err := query.QueryRowContext(ctx, statement, scope.Kind, scope.ID, deploymentID, bindingID).Scan(&floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return floor, err
}

// recordSkillBindingTombstone raises a deleted binding ID's revision floor.
// The floor never decreases.
func (s *PostgresStore) recordSkillBindingTombstone(ctx context.Context, exec postgresExecer, scope skill.ScopeReference, deploymentID, bindingID string, revision int64) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO `+s.table("skill_binding_tombstones")+` (scope_kind, scope_id, deployment_id, id, revision, deleted_at)
		VALUES ($1, $2, $3, $4, $5, CURRENT_TIMESTAMP)
		ON CONFLICT (scope_kind, scope_id, deployment_id, id) DO UPDATE
		SET revision = GREATEST(`+s.table("skill_binding_tombstones")+`.revision, EXCLUDED.revision), deleted_at = EXCLUDED.deleted_at`,
		scope.Kind, scope.ID, deploymentID, bindingID, revision)
	return err
}
