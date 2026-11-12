package sqlite

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

var _ workforce.DeploymentIDReserver = (*SQLiteStore)(nil)

// migrateDeploymentIdentitiesSQLite mirrors PostgreSQL migration 70 and is
// safe to replay on every open: it fails on an ID shared by an Agent and a
// Team, and otherwise backfills any deployment not reserved yet.
func migrateDeploymentIdentitiesSQLite(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS deployment_identities (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('agent', 'team')),
		PRIMARY KEY (scope_kind, scope_id, id)
	)`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT a.scope_kind, a.scope_id, a.id FROM agent_deployments a
		JOIN team_deployments t ON t.scope_kind = a.scope_kind AND t.scope_id = a.scope_id AND t.id = a.id
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
	if _, err := tx.Exec(`INSERT OR IGNORE INTO deployment_identities (scope_kind, scope_id, id, kind)
		SELECT scope_kind, scope_id, id, 'agent' FROM agent_deployments
		UNION ALL SELECT scope_kind, scope_id, id, 'team' FROM team_deployments`); err != nil {
		return err
	}
	return tx.Commit()
}

func reserveDeploymentIDSQLite(ctx context.Context, db sqlstore.ExecRowQuerier, scope capability.ScopeReference, id string, kind workforce.DeploymentKind) error {
	if err := workforce.ValidDeploymentReservation(scope, id, kind); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO deployment_identities (scope_kind, scope_id, id, kind) VALUES (?, ?, ?, ?)`, scope.Kind, scope.ID, id, string(kind)); err != nil {
		return err
	}
	var existing string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM deployment_identities WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&existing); err != nil {
		return err
	}
	if existing != string(kind) {
		return workforce.ErrDeploymentIDTaken
	}
	return nil
}

func (s *SQLiteStore) ReserveDeploymentID(ctx context.Context, scope capability.ScopeReference, id string, kind workforce.DeploymentKind) error {
	return reserveDeploymentIDSQLite(ctx, s.db, scope, id, kind)
}

func (s *SQLiteStore) DeploymentIDKind(ctx context.Context, scope capability.ScopeReference, id string) (workforce.DeploymentKind, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM deployment_identities WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", workforce.ErrDeploymentIDNotReserved
	}
	return workforce.DeploymentKind(kind), err
}
