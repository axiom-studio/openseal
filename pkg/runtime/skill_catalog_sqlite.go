package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func migrateSkillCatalog(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS skill_definitions (
			id TEXT NOT NULL,
			version TEXT NOT NULL,
			source_identity TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL,
			PRIMARY KEY (id, version, source_identity)
		);
		CREATE TABLE IF NOT EXISTS skill_bindings (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			id TEXT NOT NULL,
			skill_id TEXT NOT NULL,
			skill_version TEXT NOT NULL,
			source_identity TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, deployment_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_skill_bindings_deployment
			ON skill_bindings(scope_kind, scope_id, deployment_id, id);
		CREATE TABLE IF NOT EXISTS skill_binding_tombstones (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			deleted_at TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, deployment_id, id)
		);
	`); err != nil {
		return err
	}
	definitionColumns, err := sqliteTableColumns(tx, "skill_definitions")
	if err != nil {
		return err
	}
	if !definitionColumns["source_identity"] {
		if _, err = tx.Exec(`
			ALTER TABLE skill_definitions RENAME TO skill_definitions_before_source_identity;
			CREATE TABLE skill_definitions (
				id TEXT NOT NULL,
				version TEXT NOT NULL,
				source_identity TEXT NOT NULL DEFAULT '',
				payload TEXT NOT NULL,
				PRIMARY KEY (id, version, source_identity)
			);
			INSERT INTO skill_definitions(id, version, source_identity, payload)
				SELECT id, version, '', payload FROM skill_definitions_before_source_identity;
			DROP TABLE skill_definitions_before_source_identity;
		`); err != nil {
			return fmt.Errorf("migrate skill definitions to source-qualified identity: %w", err)
		}
	}
	bindingColumns, err := sqliteTableColumns(tx, "skill_bindings")
	if err != nil {
		return err
	}
	if !bindingColumns["source_identity"] {
		if _, err = tx.Exec(`ALTER TABLE skill_bindings ADD COLUMN source_identity TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate skill bindings to source-qualified identity: %w", err)
		}
	}
	return tx.Commit()
}

type sqliteQueryer interface {
	Query(string, ...interface{}) (*sql.Rows, error)
}

func sqliteTableColumns(queryer sqliteQueryer, table string) (map[string]bool, error) {
	rows, err := queryer.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, dataType string
		var defaultValue interface{}
		if err := rows.Scan(&sequence, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (s *SQLiteStore) CreateSkillDefinition(ctx context.Context, definition *skill.Definition) error {
	payload, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	sourceIdentity := skill.DefinitionSourceIdentity(definition)
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_definitions(id, version, source_identity, payload) VALUES(?, ?, ?, ?)`, definition.ID, definition.Version, sourceIdentity, string(payload))
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

func (s *SQLiteStore) ListSkillDefinitionVariants(ctx context.Context, id, version string) ([]*skill.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_identity, payload FROM skill_definitions WHERE id = ? AND version = ? ORDER BY source_identity`, id, version)
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
		if definition.Source == nil && sourceIdentity != "" {
			return nil, errors.New("stored skill source identity is missing from its immutable payload")
		}
		if skill.DefinitionSourceIdentity(&definition) != sourceIdentity {
			return nil, errors.New("stored skill source identity does not match its immutable payload")
		}
		definitions = append(definitions, &definition)
	}
	return definitions, rows.Err()
}

func (s *SQLiteStore) ListSkillDefinitionIdentities(ctx context.Context, id, version string) ([]capability.SkillIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version, source_identity FROM skill_definitions WHERE id = ? AND version = ? ORDER BY source_identity`, id, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := make([]capability.SkillIdentity, 0)
	for rows.Next() {
		var identity capability.SkillIdentity
		if err := rows.Scan(&identity.ID, &identity.Version, &identity.SourceIdentity); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func (s *SQLiteStore) SaveSkillBinding(ctx context.Context, binding *skill.Binding, expectedRevision int64) error {
	if err := skill.ValidateBindingShape(binding); err != nil {
		return err
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	maintenanceScope := Scope{Kind: binding.Scope.Kind, ID: binding.Scope.ID}
	for _, skillID := range []string{binding.SkillID} {
		g, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, maintenanceScope, skillID)
		if err != nil {
			return err
		}
		if g != nil {
			return &SkillRuntimeMaintenanceError{Maintenance: *g}
		}
	}
	var previousSkillID string
	lookupErr := conn.QueryRowContext(ctx, `SELECT skill_id FROM skill_bindings WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=?`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID).Scan(&previousSkillID)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return lookupErr
	}
	exists := lookupErr == nil
	if previousSkillID != "" && previousSkillID != binding.SkillID {
		g, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, maintenanceScope, previousSkillID)
		if err != nil {
			return err
		}
		if g != nil {
			return &SkillRuntimeMaintenanceError{Maintenance: *g}
		}
	}
	if !exists {
		floor, err := sqliteSkillBindingRevisionFloor(ctx, conn, binding.Scope, binding.DeploymentID, binding.ID)
		if err != nil {
			return err
		}
		if expectedRevision != floor || binding.Revision != floor+1 {
			return skill.ErrBindingRevisionConflict
		}
		result, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO skill_bindings(scope_kind, scope_id, deployment_id, id, skill_id, skill_version, source_identity, revision, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload))
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
		_, err = conn.ExecContext(ctx, "COMMIT")
		committed = err == nil
		return err
	}
	if binding.Revision != expectedRevision+1 {
		return skill.ErrBindingRevisionConflict
	}
	result, err := conn.ExecContext(ctx, `UPDATE skill_bindings SET skill_id = ?, skill_version = ?, source_identity = ?, revision = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? AND id = ? AND revision = ?`, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, expectedRevision)
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
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
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

// DeleteSkillBinding removes one binding row at its expected revision under the
// same Skill maintenance fence as SaveSkillBinding.
func (s *SQLiteStore) DeleteSkillBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string, expectedRevision int64) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var skillID string
	var revision int64
	err = conn.QueryRowContext(ctx, `SELECT skill_id, revision FROM skill_bindings WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=?`,
		scope.Kind, scope.ID, deploymentID, bindingID).Scan(&skillID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return skill.ErrBindingNotFound
	}
	if err != nil {
		return err
	}
	if revision != expectedRevision {
		return skill.ErrBindingRevisionConflict
	}
	if g, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, Scope{Kind: scope.Kind, ID: scope.ID}, skillID); err != nil {
		return err
	} else if g != nil {
		return &SkillRuntimeMaintenanceError{Maintenance: *g}
	}
	result, err := conn.ExecContext(ctx, `DELETE FROM skill_bindings WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`,
		scope.Kind, scope.ID, deploymentID, bindingID, expectedRevision)
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
	if err := sqliteRecordSkillBindingTombstone(ctx, conn, scope, deploymentID, bindingID, expectedRevision+1); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}

func (s *SQLiteStore) SkillBindingRevisionFloor(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string) (int64, error) {
	return sqliteSkillBindingRevisionFloor(ctx, s.db, scope, deploymentID, bindingID)
}

type sqliteRowQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

type sqliteExecer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func sqliteSkillBindingRevisionFloor(ctx context.Context, query sqliteRowQuerier, scope skill.ScopeReference, deploymentID, bindingID string) (int64, error) {
	var floor int64
	err := query.QueryRowContext(ctx, `SELECT revision FROM skill_binding_tombstones WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=?`,
		scope.Kind, scope.ID, deploymentID, bindingID).Scan(&floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return floor, err
}

// sqliteRecordSkillBindingTombstone raises a deleted binding ID's revision
// floor. The floor never decreases.
func sqliteRecordSkillBindingTombstone(ctx context.Context, exec sqliteExecer, scope skill.ScopeReference, deploymentID, bindingID string, revision int64) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO skill_binding_tombstones(scope_kind, scope_id, deployment_id, id, revision, deleted_at) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope_kind, scope_id, deployment_id, id) DO UPDATE SET revision=MAX(revision, excluded.revision), deleted_at=excluded.deleted_at`,
		scope.Kind, scope.ID, deploymentID, bindingID, revision, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

var _ skill.CatalogStore = (*SQLiteStore)(nil)
var _ skill.CatalogDefinitionIdentityStore = (*SQLiteStore)(nil)
