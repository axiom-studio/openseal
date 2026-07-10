package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func migrateSkillCatalog(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS skill_definitions (
			id TEXT NOT NULL,
			version TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (id, version)
		);
		CREATE TABLE IF NOT EXISTS skill_bindings (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			id TEXT NOT NULL,
			skill_id TEXT NOT NULL,
			skill_version TEXT NOT NULL,
			revision INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, deployment_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_skill_bindings_deployment
			ON skill_bindings(scope_kind, scope_id, deployment_id, id);
	`)
	return err
}

func (s *SQLiteStore) CreateSkillDefinition(ctx context.Context, definition *skill.Definition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_definitions(id, version, payload) VALUES(?, ?, ?)`, definition.ID, definition.Version, string(payload))
	if err != nil {
		return fmt.Errorf("persist immutable skill definition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return skill.ErrDefinitionImmutable
	}
	return nil
}

func (s *SQLiteStore) GetSkillDefinition(ctx context.Context, id, version string) (*skill.Definition, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM skill_definitions WHERE id = ? AND version = ?`, id, version).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var definition skill.Definition
	if err := json.Unmarshal([]byte(payload), &definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func (s *SQLiteStore) SaveSkillBinding(ctx context.Context, binding *skill.Binding, expectedRevision int64) error {
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		if binding.Revision != 1 {
			return skill.ErrBindingRevisionConflict
		}
		result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_bindings(scope_kind, scope_id, deployment_id, id, skill_id, skill_version, revision, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.Revision, string(payload))
		if err != nil {
			return fmt.Errorf("persist initial skill binding: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return skill.ErrBindingRevisionConflict
		}
		return nil
	}
	if binding.Revision != expectedRevision+1 {
		return skill.ErrBindingRevisionConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE skill_bindings SET skill_id = ?, skill_version = ?, revision = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? AND id = ? AND revision = ?`, binding.SkillID, binding.SkillVersion, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, expectedRevision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return skill.ErrBindingRevisionConflict
	}
	return nil
}

func (s *SQLiteStore) ListSkillBindings(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*skill.Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM skill_bindings WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? ORDER BY id`, scope.Kind, scope.ID, deploymentID)
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
