package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *PostgresStore) migrateAgentAndSkillControlPlane(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("agent_definitions")+` (
			id TEXT NOT NULL, version TEXT NOT NULL, digest TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (id, version)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("agent_deployments")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			definition_id TEXT NOT NULL, active_version TEXT NOT NULL, revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("agent_definition_activations")+` (
			id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, deployment_revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			UNIQUE (scope_kind, scope_id, deployment_id, deployment_revision)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("agent_definition_amendments")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, status TEXT NOT NULL, revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("skill_definitions")+` (
			id TEXT NOT NULL, version TEXT NOT NULL, source_identity TEXT NOT NULL DEFAULT '', payload JSONB NOT NULL,
			PRIMARY KEY (id, version, source_identity)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("skill_bindings")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, deployment_id TEXT NOT NULL,
			id TEXT NOT NULL, skill_id TEXT NOT NULL, skill_version TEXT NOT NULL,
			source_identity TEXT NOT NULL DEFAULT '', revision BIGINT NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, deployment_id, id)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS agent_definition_versions_idx ON ` + s.table("agent_definitions") + ` (id, created_at)`,
		`CREATE INDEX IF NOT EXISTS agent_deployment_activations_idx ON ` + s.table("agent_definition_activations") + ` (scope_kind, scope_id, deployment_id, deployment_revision)`,
		`CREATE INDEX IF NOT EXISTS agent_definition_amendments_idx ON ` + s.table("agent_definition_amendments") + ` (scope_kind, scope_id, deployment_id, status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS skill_bindings_deployment_idx ON ` + s.table("skill_bindings") + ` (scope_kind, scope_id, deployment_id, id)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (4, 'agent and skill control plane') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) migrateSourceQualifiedSkillVariants(ctx context.Context, tx *sql.Tx) error {
	var applied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.table("schema_migrations")+` WHERE version = 18)`).Scan(&applied); err != nil {
		return err
	}
	var definitionColumn, bindingColumn bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='skill_definitions' AND column_name='source_identity'),
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='skill_bindings' AND column_name='source_identity')`, s.schema).Scan(&definitionColumn, &bindingColumn); err != nil {
		return err
	}
	if applied && definitionColumn && bindingColumn {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE `+s.table("skill_definitions")+` ADD COLUMN IF NOT EXISTS source_identity TEXT NOT NULL DEFAULT '';
		ALTER TABLE `+s.table("skill_bindings")+` ADD COLUMN IF NOT EXISTS source_identity TEXT NOT NULL DEFAULT '';
		UPDATE `+s.table("skill_definitions")+` SET source_identity = COALESCE(payload->'source'->>'identity', '') WHERE source_identity = '';
		UPDATE `+s.table("skill_bindings")+` SET source_identity = COALESCE(payload->>'sourceIdentity', '') WHERE source_identity = '';
		ALTER TABLE `+s.table("skill_definitions")+` DROP CONSTRAINT IF EXISTS skill_definitions_pkey;
		ALTER TABLE `+s.table("skill_definitions")+` ADD PRIMARY KEY (id, version, source_identity);
	`); err != nil {
		return fmt.Errorf("migrate source-qualified skill variants: %w", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (18, 'source-qualified skill variants') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) migrateAgentDefinitionCompilations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("agent_definition_compilations")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL, definition_id TEXT NOT NULL, status TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS agent_definition_compilations_idx ON `+s.table("agent_definition_compilations")+` (scope_kind, scope_id, deployment_id, created_at DESC, id DESC)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (15, 'agent definition compilations') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateCompilation(ctx context.Context, compilation *kernelagent.DefinitionCompilation) error {
	payload, err := json.Marshal(compilation)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("agent_definition_compilations")+` (id, scope_kind, scope_id, deployment_id, definition_id, status, created_at, payload) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb) ON CONFLICT (scope_kind, scope_id, id) DO NOTHING`, compilation.ID, compilation.Scope.Kind, compilation.Scope.ID, compilation.DeploymentID, compilation.DefinitionID, compilation.Status, compilation.CreatedAt, string(payload))
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows != 1 {
		return kernelagent.ErrCompilationImmutable
	}
	return nil
}

func (s *PostgresStore) GetCompilation(ctx context.Context, scope capability.ScopeReference, id string) (*kernelagent.DefinitionCompilation, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_definition_compilations")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kernelagent.ErrCompilationNotFound
		}
		return nil, err
	}
	var compilation kernelagent.DefinitionCompilation
	if err := json.Unmarshal([]byte(payload), &compilation); err != nil {
		return nil, err
	}
	return &compilation, nil
}

func (s *PostgresStore) ListCompilations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]*kernelagent.DefinitionCompilation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_definition_compilations")+` WHERE scope_kind = $1 AND scope_id = $2 AND deployment_id = $3 ORDER BY created_at DESC, id DESC`, scope.Kind, scope.ID, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*kernelagent.DefinitionCompilation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var compilation kernelagent.DefinitionCompilation
		if err := json.Unmarshal([]byte(payload), &compilation); err != nil {
			return nil, err
		}
		result = append(result, &compilation)
	}
	return result, rows.Err()
}

func (s *PostgresStore) CreateDefinition(ctx context.Context, definition *kernelagent.AgentDefinition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("agent_definitions")+` (id, version, digest, created_at, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb) ON CONFLICT (id, version) DO NOTHING`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload))
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return errors.New("agent definition versions are immutable")
	}
	return nil
}

func (s *PostgresStore) GetDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_definitions")+` WHERE id = $1 AND version = $2`, id, version).Scan(&payload); err != nil {
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

func (s *PostgresStore) ListDefinitionVersions(ctx context.Context, id string) ([]*kernelagent.AgentDefinition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_definitions")+` WHERE id = $1 ORDER BY version`, id)
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

func (s *PostgresStore) CreateDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := registryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_deployments")+`
		(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID,
		deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, deploymentPayload); err != nil {
		return err
	}
	if err := s.insertDefinitionActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_deployments")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
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

func (s *PostgresStore) ListDeployments(ctx context.Context, scope capability.ScopeReference) ([]*kernelagent.AgentDeployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_deployments")+` WHERE scope_kind = $1 AND scope_id = $2 ORDER BY updated_at DESC, id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*kernelagent.AgentDeployment, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var deployment kernelagent.AgentDeployment
		if err := json.Unmarshal([]byte(payload), &deployment); err != nil {
			return nil, err
		}
		result = append(result, &deployment)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, expectedRevision int64, activation kernelagent.DefinitionActivation) error {
	deploymentPayload, activationPayload, err := registryPayloads(deployment, activation)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_deployments")+` SET active_version = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, deployment.ActiveVersion, deployment.Revision,
		deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedRevision)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelagent.ErrRevisionConflict
	}
	if err := s.insertDefinitionActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]kernelagent.DefinitionActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_definition_activations")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND deployment_id = $3 ORDER BY deployment_revision`, scope.Kind, scope.ID, deploymentID)
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

func (s *PostgresStore) insertDefinitionActivation(ctx context.Context, tx *sql.Tx, activation kernelagent.DefinitionActivation, payload string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definition_activations")+`
		(id, scope_kind, scope_id, deployment_id, deployment_revision, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`, activation.ID, activation.Scope.Kind, activation.Scope.ID,
		activation.DeploymentID, activation.DeploymentRevision, activation.CreatedAt, payload)
	return err
}

func (s *PostgresStore) CreateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("agent_definition_amendments")+`
		(id, scope_kind, scope_id, deployment_id, status, revision, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`, amendment.ID, amendment.Scope.Kind, amendment.Scope.ID,
		amendment.DeploymentID, amendment.Status, amendment.Revision, amendment.UpdatedAt, string(payload))
	return err
}

func (s *PostgresStore) GetAmendment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelagent.DefinitionAmendment, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_definition_amendments")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
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

func (s *PostgresStore) UpdateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment, expectedRevision int64) error {
	payload, err := json.Marshal(amendment)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("agent_definition_amendments")+` SET status = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, amendment.Status, amendment.Revision,
		amendment.UpdatedAt, string(payload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedRevision)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelagent.ErrRevisionConflict
	}
	return nil
}

func (s *PostgresStore) ActivateAmendment(ctx context.Context, amendment *kernelagent.DefinitionAmendment, expectedAmendmentRevision int64, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, expectedDeploymentRevision int64, activation kernelagent.DefinitionActivation) error {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definitions")+` (id, version, digest, created_at, payload) VALUES ($1, $2, $3, $4, $5::jsonb)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(definitionPayload)); err != nil {
		return err
	}
	deploymentResult, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_deployments")+` SET active_version = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, deployment.ActiveVersion, deployment.Revision,
		deployment.UpdatedAt, deploymentPayload, deployment.Scope.Kind, deployment.Scope.ID, deployment.ID, expectedDeploymentRevision)
	if err != nil {
		return err
	}
	if rows, err := deploymentResult.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelagent.ErrRevisionConflict
	}
	if err := s.insertDefinitionActivation(ctx, tx, activation, activationPayload); err != nil {
		return err
	}
	amendmentResult, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_definition_amendments")+` SET status = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, amendment.Status, amendment.Revision,
		amendment.UpdatedAt, string(amendmentPayload), amendment.Scope.Kind, amendment.Scope.ID, amendment.ID, expectedAmendmentRevision)
	if err != nil {
		return err
	}
	if rows, err := amendmentResult.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return kernelagent.ErrRevisionConflict
	}
	return tx.Commit()
}

func (s *PostgresStore) CreateSkillDefinition(ctx context.Context, definition *skill.Definition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	sourceIdentity := skill.DefinitionSourceIdentity(definition)
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("skill_definitions")+` (id, version, source_identity, payload)
		VALUES ($1, $2, $3, $4::jsonb) ON CONFLICT (id, version, source_identity) DO NOTHING`, definition.ID, definition.Version, sourceIdentity, string(payload))
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return skill.ErrDefinitionImmutable
	}
	return nil
}

func (s *PostgresStore) ListSkillDefinitionVariants(ctx context.Context, id, version string) ([]*skill.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_identity, payload FROM `+s.table("skill_definitions")+` WHERE id = $1 AND version = $2 ORDER BY source_identity`, id, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	definitions := make([]*skill.Definition, 0)
	for rows.Next() {
		var sourceIdentity, payload string
		if err := rows.Scan(&sourceIdentity, &payload); err != nil {
			return nil, err
		}
		var definition skill.Definition
		if err := json.Unmarshal([]byte(payload), &definition); err != nil {
			return nil, err
		}
		if skill.DefinitionSourceIdentity(&definition) != sourceIdentity {
			return nil, errors.New("stored skill source identity does not match its immutable payload")
		}
		definitions = append(definitions, &definition)
	}
	return definitions, rows.Err()
}

func (s *PostgresStore) SaveSkillBinding(ctx context.Context, binding *skill.Binding, expectedRevision int64) error {
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		if binding.Revision != 1 {
			return skill.ErrBindingRevisionConflict
		}
		result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("skill_bindings")+`
			(scope_kind, scope_id, deployment_id, id, skill_id, skill_version, source_identity, revision, payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb) ON CONFLICT DO NOTHING`, binding.Scope.Kind, binding.Scope.ID,
			binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload))
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return err
			}
			return skill.ErrBindingRevisionConflict
		}
		return nil
	}
	if binding.Revision != expectedRevision+1 {
		return skill.ErrBindingRevisionConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("skill_bindings")+` SET skill_id = $1, skill_version = $2, source_identity = $3, revision = $4, payload = $5::jsonb
		WHERE scope_kind = $6 AND scope_id = $7 AND deployment_id = $8 AND id = $9 AND revision = $10`, binding.SkillID,
		binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, expectedRevision)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return skill.ErrBindingRevisionConflict
	}
	return nil
}

func (s *PostgresStore) ListSkillBindings(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*skill.Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("skill_bindings")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND deployment_id = $3 ORDER BY id`, scope.Kind, scope.ID, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]*skill.Binding, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var binding skill.Binding
		if err := json.Unmarshal([]byte(payload), &binding); err != nil {
			return nil, err
		}
		bindings = append(bindings, &binding)
	}
	return bindings, rows.Err()
}

var _ skill.CatalogStore = (*PostgresStore)(nil)
