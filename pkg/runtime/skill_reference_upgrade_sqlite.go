package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *SQLiteStore) ApplySkillReferenceUpgrade(ctx context.Context, application *SkillReferenceUpgradeMutation) error {
	if err := validateSkillReferenceUpgradeMutation(application); err != nil {
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
	gate, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, application.Plan.Scope, application.Plan.From.ID)
	if err != nil {
		return err
	}
	if err := validateSkillUpgradeMaintenance(ctx, gate, application, time.Now().UTC()); err != nil {
		return err
	}
	var bindingPayload string
	if err = conn.QueryRowContext(ctx, `SELECT payload FROM skill_bindings
		WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`,
		application.Plan.Scope.Kind, application.Plan.Scope.ID, application.Plan.DeploymentID,
		application.Plan.BindingID, application.Plan.ExpectedBindingRevision).Scan(&bindingPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSkillReferenceUpgradeConflict
		}
		return err
	}
	var currentBinding skill.Binding
	if json.Unmarshal([]byte(bindingPayload), &currentBinding) != nil || currentBinding.Disabled ||
		currentBinding.SkillID != application.Plan.From.ID || currentBinding.SkillVersion != application.Plan.From.Version ||
		currentBinding.SourceIdentity != application.Plan.From.SourceIdentity {
		return ErrSkillReferenceUpgradeConflict
	}
	busy, err := hasSkillRuntimeUsage(ctx, conn, skillBindingUpgradeUsageFilter(application), "action_calls", "agent_runs", false)
	if err != nil {
		return err
	}
	if busy {
		return ErrSkillReferenceUpgradeBusy
	}
	if err := validateProjectSkillUpgradeReferencesSQL(ctx, conn, application, "project_skill_references", false); err != nil {
		return err
	}
	if err := applySkillUpgradeTeamAuthoritySQL(ctx, conn, application.TeamAuthority, "team_definitions", "team_deployments", "team_definition_activations", false); err != nil {
		return err
	}
	if err := validateSkillUpgradeMaintenance(ctx, gate, application, time.Now().UTC()); err != nil {
		return err
	}
	encodedBinding, err := json.Marshal(application.Binding)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE skill_bindings
		SET skill_id=?,skill_version=?,source_identity=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`,
		application.Binding.SkillID, application.Binding.SkillVersion, application.Binding.SourceIdentity,
		application.Binding.Revision, string(encodedBinding), application.Binding.Scope.Kind, application.Binding.Scope.ID,
		application.Binding.DeploymentID, application.Binding.ID, application.Plan.ExpectedBindingRevision)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrSkillReferenceUpgradeConflict
	}
	for _, mutation := range application.Objectives {
		encoded, marshalErr := json.Marshal(mutation.Value)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = conn.ExecContext(ctx, `UPDATE objectives
			SET owner_type=?,owner_id=?,status=?,priority=?,revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			mutation.Value.Owner.Type, mutation.Value.Owner.ID, mutation.Value.Status, mutation.Value.Priority,
			mutation.Value.Revision, mutation.Value.UpdatedAt, string(encoded), mutation.Value.Scope.Kind,
			mutation.Value.Scope.ID, mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
		if _, err = insertSQLiteActivityConn(ctx, conn, mutation.Event); err != nil {
			return err
		}
	}
	for _, mutation := range application.Projects {
		encoded, marshalErr := json.Marshal(mutation.Value)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = conn.ExecContext(ctx, `UPDATE projects
			SET owner_type=?,owner_id=?,status=?,revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			mutation.Value.Owner.Type, mutation.Value.Owner.ID, mutation.Value.Status, mutation.Value.Revision,
			mutation.Value.UpdatedAt, string(encoded), mutation.Value.Scope.Kind, mutation.Value.Scope.ID,
			mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
		if _, err = insertSQLiteActivityConn(ctx, conn, mutation.Event); err != nil {
			return err
		}
	}
	for _, mutation := range application.ConversationEndpoints {
		encoded, marshalErr := json.Marshal(mutation.Value)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = conn.ExecContext(ctx, `UPDATE external_conversation_endpoints
			SET ingress_route=?,provider=?,status=?,revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, mutation.Value.IngressRoute,
			mutation.Value.Provider, mutation.Value.Status, mutation.Value.Revision, mutation.Value.UpdatedAt,
			string(encoded), mutation.Value.Scope.Kind, mutation.Value.Scope.ID, mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	for _, mutation := range application.CallbackRegistrations {
		encoded, marshalErr := json.Marshal(mutation.Value)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = conn.ExecContext(ctx, `UPDATE callback_registrations
			SET ingress_route=?,provider=?,status=?,revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, mutation.Value.IngressRoute,
			mutation.Value.Provider, mutation.Value.Status, mutation.Value.Revision, mutation.Value.UpdatedAt,
			string(encoded), mutation.Value.Scope.Kind, mutation.Value.Scope.ID, mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
