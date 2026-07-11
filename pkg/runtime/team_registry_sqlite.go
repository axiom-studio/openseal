package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func migrateTeamRegistry(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS team_definitions (
			id TEXT NOT NULL, version TEXT NOT NULL, digest TEXT NOT NULL,
			created_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (id, version)
		);
		CREATE TABLE IF NOT EXISTS team_deployments (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			definition_id TEXT NOT NULL, active_version TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS team_definition_activations (
			id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, deployment_revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL, payload TEXT NOT NULL,
			UNIQUE (scope_kind, scope_id, deployment_id, deployment_revision)
		);
		CREATE INDEX IF NOT EXISTS idx_team_definition_versions ON team_definitions(id, created_at);
		CREATE INDEX IF NOT EXISTS idx_team_definition_activations
			ON team_definition_activations(scope_kind, scope_id, deployment_id, deployment_revision);
	`)
	return err
}

func (s *SQLiteStore) CreateTeamDefinition(ctx context.Context, definition *kernelteam.Definition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO team_definitions(id, version, digest, created_at, payload) VALUES(?, ?, ?, ?, ?)`,
		definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetTeamDefinition(ctx context.Context, id, version string) (*kernelteam.Definition, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_definitions WHERE id = ? AND version = ?`, id, version).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelteam.ErrDefinitionNotFound
		}
		return nil, err
	}
	var definition kernelteam.Definition
	if err := json.Unmarshal([]byte(payload), &definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func (s *SQLiteStore) ListTeamDefinitionVersions(ctx context.Context, id string) ([]*kernelteam.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM team_definitions WHERE id = ? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*kernelteam.Definition, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var definition kernelteam.Definition
		if err := json.Unmarshal([]byte(payload), &definition); err != nil {
			return nil, err
		}
		result = append(result, &definition)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := teamRegistryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_deployments(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion,
		deployment.Revision, deployment.UpdatedAt, deploymentPayload); err != nil {
		return err
	}
	if err := insertTeamActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetTeamDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_deployments WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelteam.ErrDeploymentNotFound
		}
		return nil, err
	}
	var deployment kernelteam.Deployment
	if err := json.Unmarshal([]byte(payload), &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func (s *SQLiteStore) UpdateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, expectedRevision int64, activation workforce.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := teamRegistryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE team_deployments SET active_version = ?, revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload,
		deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedRevision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return kernelteam.ErrRevisionConflict
	}
	if err := insertTeamActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListTeamDefinitionActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM team_definition_activations WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? ORDER BY deployment_revision`, scope.Kind, scope.ID, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]workforce.DefinitionActivation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var activation workforce.DefinitionActivation
		if err := json.Unmarshal([]byte(payload), &activation); err != nil {
			return nil, err
		}
		result = append(result, activation)
	}
	return result, rows.Err()
}

func teamRegistryPayloads(deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) (string, string, error) {
	deploymentPayload, err := json.Marshal(deployment)
	if err != nil {
		return "", "", err
	}
	activationPayload, err := json.Marshal(activation)
	if err != nil {
		return "", "", err
	}
	return string(deploymentPayload), string(activationPayload), nil
}

func insertTeamActivation(ctx context.Context, tx *sql.Tx, activation workforce.DefinitionActivation, payload string) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO team_definition_activations(id, scope_kind, scope_id, deployment_id, deployment_revision, created_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		activation.ID, activation.Scope.Kind, activation.Scope.ID, activation.DeploymentID, activation.DeploymentRevision, activation.CreatedAt, payload)
	if err != nil {
		return fmt.Errorf("persist Team definition activation: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("Team definition activation was not persisted")
	}
	return nil
}

var _ kernelteam.Store = (*SQLiteStore)(nil)
