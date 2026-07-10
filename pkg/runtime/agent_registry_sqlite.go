package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func migrateAgentRegistry(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_definitions (
			id TEXT NOT NULL,
			version TEXT NOT NULL,
			digest TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (id, version)
		);
		CREATE TABLE IF NOT EXISTS agent_deployments (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			definition_id TEXT NOT NULL,
			active_version TEXT NOT NULL,
			revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS agent_definition_activations (
			id TEXT PRIMARY KEY,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			deployment_revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			UNIQUE (scope_kind, scope_id, deployment_id, deployment_revision)
		);
		CREATE TABLE IF NOT EXISTS agent_definition_amendments (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			status TEXT NOT NULL,
			revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_definition_versions ON agent_definitions(id, created_at);
		CREATE INDEX IF NOT EXISTS idx_agent_deployment_activations
			ON agent_definition_activations(scope_kind, scope_id, deployment_id, deployment_revision);
		CREATE INDEX IF NOT EXISTS idx_agent_definition_amendments
			ON agent_definition_amendments(scope_kind, scope_id, deployment_id, status, updated_at);
	`)
	return err
}

func (s *SQLiteStore) CreateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_definition_amendments(id, scope_kind, scope_id, deployment_id, status, revision, updated_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, amendment.ID, amendment.Scope.Kind, amendment.Scope.ID, amendment.DeploymentID, amendment.Status, amendment.Revision, amendment.UpdatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetAmendment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelagent.DefinitionAmendment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_definition_amendments WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelagent.ErrAmendmentNotFound
		}
		return nil, err
	}
	var amendment kernelagent.DefinitionAmendment
	if err := json.Unmarshal([]byte(payload), &amendment); err != nil {
		return nil, err
	}
	return &amendment, nil
}

func (s *SQLiteStore) UpdateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment, expectedRevision int64) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE agent_definition_amendments SET status = ?, revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, amendment.Status, amendment.Revision, amendment.UpdatedAt, string(payload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedRevision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return kernelagent.ErrRevisionConflict
	}
	return nil
}

func (s *SQLiteStore) ActivateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment, expectedAmendmentRevision int64, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, expectedDeploymentRevision int64, activation kernelagent.DefinitionActivation) error {
	definitionPayload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	deploymentPayload, activationPayload, err := registryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	amendmentPayload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_definitions(id, version, digest, created_at, payload) VALUES(?, ?, ?, ?, ?)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(definitionPayload)); err != nil {
		return err
	}
	deploymentResult, err := tx.ExecContext(ctx, `UPDATE agent_deployments SET active_version = ?, revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedDeploymentRevision)
	if err != nil {
		return err
	}
	deploymentRows, err := deploymentResult.RowsAffected()
	if err != nil {
		return err
	}
	if deploymentRows != 1 {
		return kernelagent.ErrRevisionConflict
	}
	if err := insertActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	amendmentResult, err := tx.ExecContext(ctx, `UPDATE agent_definition_amendments SET status = ?, revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, amendment.Status, amendment.Revision, amendment.UpdatedAt, string(amendmentPayload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedAmendmentRevision)
	if err != nil {
		return err
	}
	amendmentRows, err := amendmentResult.RowsAffected()
	if err != nil {
		return err
	}
	if amendmentRows != 1 {
		return kernelagent.ErrRevisionConflict
	}
	return tx.Commit()
}

func (s *SQLiteStore) CreateDefinition(ctx context.Context, definition *kernelagent.AgentDefinition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_definitions(id, version, digest, created_at, payload) VALUES(?, ?, ?, ?, ?)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_definitions WHERE id = ? AND version = ?`, id, version).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelagent.ErrDefinitionNotFound
		}
		return nil, err
	}
	var definition kernelagent.AgentDefinition
	if err := json.Unmarshal([]byte(payload), &definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func (s *SQLiteStore) ListDefinitionVersions(ctx context.Context, id string) ([]*kernelagent.AgentDefinition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agent_definitions WHERE id = ? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*kernelagent.AgentDefinition, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var definition kernelagent.AgentDefinition
		if err := json.Unmarshal([]byte(payload), &definition); err != nil {
			return nil, err
		}
		result = append(result, &definition)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) CreateDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := registryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_deployments(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload); err != nil {
		return err
	}
	if err := insertActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_deployments WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelagent.ErrDeploymentNotFound
		}
		return nil, err
	}
	var deployment kernelagent.AgentDeployment
	if err := json.Unmarshal([]byte(payload), &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func (s *SQLiteStore) UpdateDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, expectedRevision int64, activation kernelagent.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := registryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_deployments SET active_version = ?, revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedRevision)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return kernelagent.ErrRevisionConflict
	}
	if err := insertActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]kernelagent.DefinitionActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agent_definition_activations WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? ORDER BY deployment_revision`, scope.Kind, scope.ID, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]kernelagent.DefinitionActivation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var activation kernelagent.DefinitionActivation
		if err := json.Unmarshal([]byte(payload), &activation); err != nil {
			return nil, err
		}
		result = append(result, activation)
	}
	return result, rows.Err()
}

func registryPayloads(deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) (string, string, error) {
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

func insertActivation(ctx context.Context, tx *sql.Tx, activation kernelagent.DefinitionActivation, payload string) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO agent_definition_activations(id, scope_kind, scope_id, deployment_id, deployment_revision, created_at, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`, activation.ID, activation.Scope.Kind, activation.Scope.ID, activation.DeploymentID, activation.DeploymentRevision, activation.CreatedAt, payload)
	if err != nil {
		return fmt.Errorf("persist definition activation: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("definition activation was not persisted")
	}
	return nil
}

var _ kernelagent.Store = (*SQLiteStore)(nil)
