package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const deploymentIdentityMigrationVersion int64 = 70

var _ workforce.DeploymentIDReserver = (*PostgresStore)(nil)

// migrateDeploymentIdentities creates the shared Agent and Team deployment-ID
// namespace and backfills it from both registries. An ID used by both an
// Agent and a Team aborts the migration: renaming one is a manual decision.
func (s *PostgresStore) migrateDeploymentIdentities(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, deploymentIdentityMigrationVersion)
	if err != nil || applied {
		return err
	}
	identities, agents, teams := s.table("deployment_identities"), s.table("agent_deployments"), s.table("team_deployments")
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+identities+` (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, kind TEXT NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id), CHECK (kind IN ('agent', 'team'))
	)`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.scope_kind, a.scope_id, a.id FROM `+agents+` a
		JOIN `+teams+` t ON t.scope_kind = a.scope_kind AND t.scope_id = a.scope_id AND t.id = a.id
		ORDER BY a.scope_kind, a.scope_id, a.id`)
	if err != nil {
		return err
	}
	collisions, err := sqlstore.DeploymentIdentityCollisions(rows)
	if err != nil {
		return err
	}
	if len(collisions) > 0 {
		return fmt.Errorf("%w: Agent and Team deployments share IDs %s; rename one of each before migrating", workforce.ErrDeploymentIDTaken, strings.Join(collisions, ", "))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+identities+` (scope_kind, scope_id, id, kind)
		SELECT scope_kind, scope_id, id, 'agent' FROM `+agents+`
		UNION ALL SELECT scope_kind, scope_id, id, 'team' FROM `+teams+`
		ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'Agent and Team deployment IDs share one namespace') ON CONFLICT(version) DO NOTHING`, deploymentIdentityMigrationVersion)
	return err
}

func (s *PostgresStore) reserveDeploymentID(ctx context.Context, db sqlstore.ExecRowQuerier, scope capability.ScopeReference, id string, kind workforce.DeploymentKind) error {
	if err := workforce.ValidDeploymentReservation(scope, id, kind); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO `+s.table("deployment_identities")+` (scope_kind, scope_id, id, kind)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, scope.Kind, scope.ID, id, string(kind)); err != nil {
		return err
	}
	var existing string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM `+s.table("deployment_identities")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&existing); err != nil {
		return err
	}
	if existing != string(kind) {
		return workforce.ErrDeploymentIDTaken
	}
	return nil
}

func (s *PostgresStore) ReserveDeploymentID(ctx context.Context, scope capability.ScopeReference, id string, kind workforce.DeploymentKind) error {
	return s.reserveDeploymentID(ctx, s.db, scope, id, kind)
}

func (s *PostgresStore) DeploymentIDKind(ctx context.Context, scope capability.ScopeReference, id string) (workforce.DeploymentKind, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM `+s.table("deployment_identities")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", workforce.ErrDeploymentIDNotReserved
	}
	return workforce.DeploymentKind(kind), err
}
