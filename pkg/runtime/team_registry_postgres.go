package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func (s *PostgresStore) migrateTeamControlPlane(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("team_definitions")+` (
			id TEXT NOT NULL, version TEXT NOT NULL, digest TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (id, version)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("team_deployments")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			definition_id TEXT NOT NULL, active_version TEXT NOT NULL,
			revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("team_definition_activations")+` (
			id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, deployment_revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			UNIQUE (scope_kind, scope_id, deployment_id, deployment_revision)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("team_definition_amendments")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, status TEXT NOT NULL, revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		)`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS team_definition_versions_idx ON ` + s.table("team_definitions") + ` (id, created_at)`,
		`CREATE INDEX IF NOT EXISTS team_definition_activations_idx ON ` + s.table("team_definition_activations") + ` (scope_kind, scope_id, deployment_id, deployment_revision)`,
		`CREATE INDEX IF NOT EXISTS team_definition_amendments_idx ON ` + s.table("team_definition_amendments") + ` (scope_kind, scope_id, deployment_id, status, updated_at)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (11, 'first-class Team definitions and deployments') ON CONFLICT (version) DO NOTHING`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (12, 'governed Team definition amendments') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateTeamAmendment(ctx context.Context, amendment *kernelteam.DefinitionAmendment) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("team_definition_amendments")+`
		(id, scope_kind, scope_id, deployment_id, status, revision, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`, amendment.ID, amendment.Scope.Kind, amendment.Scope.ID,
		amendment.DeploymentID, amendment.Status, amendment.Revision, amendment.UpdatedAt, string(payload))
	return err
}

func (s *PostgresStore) GetTeamAmendment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelteam.DefinitionAmendment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("team_definition_amendments")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelteam.ErrAmendmentNotFound
		}
		return nil, err
	}
	var amendment kernelteam.DefinitionAmendment
	if err := json.Unmarshal([]byte(payload), &amendment); err != nil {
		return nil, err
	}
	return &amendment, nil
}

func (s *PostgresStore) UpdateTeamAmendment(ctx context.Context, amendment *kernelteam.DefinitionAmendment, expectedRevision int64) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("team_definition_amendments")+` SET status = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, amendment.Status, amendment.Revision,
		amendment.UpdatedAt, string(payload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedRevision)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelteam.ErrRevisionConflict
	}
	return nil
}

func (s *PostgresStore) ActivateTeamAmendment(ctx context.Context, amendment *kernelteam.DefinitionAmendment, expectedAmendmentRevision int64, definition *kernelteam.Definition, deployment *kernelteam.Deployment, expectedDeploymentRevision int64, activation workforce.DefinitionActivation) error {
	amendmentPayload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	definitionPayload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	deploymentPayload, activationPayload, err := teamRegistryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definitions")+` (id, version, digest, created_at, payload) VALUES ($1, $2, $3, $4, $5::jsonb)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(definitionPayload)); err != nil {
		return err
	}
	deploymentResult, err := tx.ExecContext(ctx, `UPDATE `+s.table("team_deployments")+` SET active_version = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, deployment.ActiveVersion, deployment.Revision,
		deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedDeploymentRevision)
	if err != nil {
		return err
	}
	amendmentResult, err := tx.ExecContext(ctx, `UPDATE `+s.table("team_definition_amendments")+` SET status = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, amendment.Status, amendment.Revision,
		amendment.UpdatedAt, string(amendmentPayload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedAmendmentRevision)
	if err != nil {
		return err
	}
	deploymentRows, _ := deploymentResult.RowsAffected()
	amendmentRows, _ := amendmentResult.RowsAffected()
	if deploymentRows != 1 || amendmentRows != 1 {
		return kernelteam.ErrRevisionConflict
	}
	if err := s.insertTeamActivationPostgres(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) CreateTeamDefinition(ctx context.Context, definition *kernelteam.Definition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("team_definitions")+` (id, version, digest, created_at, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb) ON CONFLICT (id, version) DO NOTHING`,
		definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload))
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return errors.New("team definition versions are immutable")
	}
	return nil
}

func (s *PostgresStore) GetTeamDefinition(ctx context.Context, id, version string) (*kernelteam.Definition, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("team_definitions")+` WHERE id = $1 AND version = $2`, id, version).Scan(&payload); err != nil {
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

func (s *PostgresStore) ListTeamDefinitionVersions(ctx context.Context, id string) ([]*kernelteam.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("team_definitions")+` WHERE id = $1 ORDER BY version`, id)
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

func (s *PostgresStore) CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := teamRegistryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("team_deployments")+`
		(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`, deployment.Scope.Kind, deployment.Scope.ID,
		deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload); err != nil {
		return err
	}
	if err := s.insertTeamActivationPostgres(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetTeamDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("team_deployments")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
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

func (s *PostgresStore) ListTeamDeployments(ctx context.Context, scope capability.ScopeReference) ([]*kernelteam.Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("team_deployments")+` WHERE scope_kind = $1 AND scope_id = $2 ORDER BY updated_at DESC, id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*kernelteam.Deployment, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var deployment kernelteam.Deployment
		if err := json.Unmarshal([]byte(payload), &deployment); err != nil {
			return nil, err
		}
		result = append(result, &deployment)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, expectedRevision int64, activation workforce.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := teamRegistryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("team_deployments")+` SET active_version = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, deployment.ActiveVersion, deployment.Revision,
		deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedRevision)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelteam.ErrRevisionConflict
	}
	if err := s.insertTeamActivationPostgres(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ListTeamDefinitionActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("team_definition_activations")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND deployment_id = $3 ORDER BY deployment_revision`, scope.Kind, scope.ID, deploymentID)
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

func (s *PostgresStore) insertTeamActivationPostgres(ctx context.Context, tx *sql.Tx, activation workforce.DefinitionActivation, payload string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definition_activations")+`
		(id, scope_kind, scope_id, deployment_id, deployment_revision, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`, activation.ID, activation.Scope.Kind, activation.Scope.ID,
		activation.DeploymentID, activation.DeploymentRevision, activation.CreatedAt, payload)
	return err
}

var _ kernelteam.Store = (*PostgresStore)(nil)
