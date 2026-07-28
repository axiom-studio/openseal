package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *PostgresStore) ApplySkillReferenceUpgrade(ctx context.Context, application *SkillReferenceUpgradeMutation) error {
	if err := validateSkillReferenceUpgradeMutation(application); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bindingPayload string
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_bindings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND id=$4 AND revision=$5 FOR UPDATE`,
		application.Plan.Scope.Kind, application.Plan.Scope.ID, application.Plan.DeploymentID,
		application.Plan.BindingID, application.Plan.ExpectedBindingRevision).Scan(&bindingPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSkillReferenceUpgradeConflict
		}
		return err
	}
	var currentBinding skill.Binding
	if json.Unmarshal([]byte(bindingPayload), &currentBinding) != nil ||
		currentBinding.SkillID != application.Plan.From.ID || currentBinding.SkillVersion != application.Plan.From.Version ||
		currentBinding.SourceIdentity != application.Plan.From.SourceIdentity {
		return ErrSkillReferenceUpgradeConflict
	}
	encodedBinding, err := json.Marshal(application.Binding)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("skill_bindings")+`
		SET skill_id=$1,skill_version=$2,source_identity=$3,revision=$4,payload=$5::jsonb
		WHERE scope_kind=$6 AND scope_id=$7 AND deployment_id=$8 AND id=$9 AND revision=$10`,
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
		result, err = tx.ExecContext(ctx, `UPDATE `+s.table("objectives")+`
			SET owner_type=$1,owner_id=$2,status=$3,priority=$4,revision=$5,updated_at=$6,payload=$7::jsonb
			WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`,
			mutation.Value.Owner.Type, mutation.Value.Owner.ID, mutation.Value.Status, mutation.Value.Priority,
			mutation.Value.Revision, mutation.Value.UpdatedAt, string(encoded), mutation.Value.Scope.Kind,
			mutation.Value.Scope.ID, mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
		if _, err = s.insertPostgresActivityTx(ctx, tx, mutation.Event); err != nil {
			return err
		}
	}
	for _, mutation := range application.Projects {
		encoded, marshalErr := json.Marshal(mutation.Value)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = tx.ExecContext(ctx, `UPDATE `+s.table("projects")+`
			SET owner_type=$1,owner_id=$2,status=$3,revision=$4,updated_at=$5,payload=$6::jsonb
			WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`,
			mutation.Value.Owner.Type, mutation.Value.Owner.ID, mutation.Value.Status, mutation.Value.Revision,
			mutation.Value.UpdatedAt, string(encoded), mutation.Value.Scope.Kind, mutation.Value.Scope.ID,
			mutation.Value.ID, mutation.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrSkillReferenceUpgradeConflict
		}
		if _, err = s.insertPostgresActivityTx(ctx, tx, mutation.Event); err != nil {
			return err
		}
	}
	return tx.Commit()
}
