package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
)

func (s *SQLiteStore) ApplyChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	for attempt := 0; attempt < 8; attempt++ {
		result, err := s.applyChangeSetOnce(ctx, value, expectedRevision)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "database is locked") {
			return result, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return nil, errors.New("atomic workforce apply exhausted SQLite lock retries")
}

func (s *SQLiteStore) applyChangeSetOnce(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	application, err := materializeWorkforceApplication(value)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	var revision int64
	var currentPayload string
	if err = tx.QueryRowContext(ctx, `SELECT status, revision, payload FROM workforce_change_sets WHERE scope_kind=? AND scope_id=? AND id=?`, value.Scope.Kind, value.Scope.ID, value.ID).Scan(&status, &revision, &currentPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, authoring.ErrChangeSetNotFound
		}
		return nil, err
	}
	if status == string(authoring.ChangeSetApplied) {
		var current authoring.ChangeSet
		if json.Unmarshal([]byte(currentPayload), &current) == nil && current.ApplyReceipt != nil && current.ApplyReceipt.IdempotencyKey == value.ApplyReceipt.IdempotencyKey {
			return &current, nil
		}
	}
	if status != string(authoring.ChangeSetReady) || revision != expectedRevision {
		return nil, authoring.ErrChangeSetRevision
	}
	for index, definition := range application.agentDefinitions {
		payload, _ := json.Marshal(definition)
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_definitions(id,version,digest,created_at,payload) VALUES(?,?,?,?,?)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload)); err != nil {
			return nil, err
		}
		deployment := application.agentDeployments[index]
		if value.Mode == authoring.ModeAmend {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM agent_deployments WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID, deployment.ID, value.Placement.AgentExpectedRevisions[definition.ID]).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current agent.AgentDeployment
			if json.Unmarshal([]byte(existing), &current) != nil || current.DefinitionID != definition.ID {
				return nil, authoring.ErrChangeSetRevision
			}
			deployment.PreviousVersion = current.ActiveVersion
			deployment.CreatedAt = current.CreatedAt
		}
		deploymentPayload, _ := json.Marshal(deployment)
		activation := application.agentActivations[index]
		activation.FromVersion = deployment.PreviousVersion
		activationPayload, _ := json.Marshal(activation)
		if value.Mode == authoring.ModeCreate {
			_, err = tx.ExecContext(ctx, `INSERT INTO agent_deployments(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(deploymentPayload))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE agent_deployments SET active_version=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(deploymentPayload), value.Scope.Kind, value.Scope.ID, deployment.ID, value.Placement.AgentExpectedRevisions[definition.ID])
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_definition_activations(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES(?,?,?,?,?,?,?)`, activation.ID, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.Revision, activation.CreatedAt, string(activationPayload)); err != nil {
			return nil, err
		}
	}
	if err = applySQLiteWorkforceSkillBindings(ctx, tx, value, application.skillBindings); err != nil {
		return nil, err
	}
	if application.teamDefinition != nil {
		teamDefinitionPayload, _ := json.Marshal(application.teamDefinition)
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_definitions(id,version,digest,created_at,payload) VALUES(?,?,?,?,?)`, application.teamDefinition.ID, application.teamDefinition.Version, application.teamDefinition.Digest, application.teamDefinition.CreatedAt, string(teamDefinitionPayload)); err != nil {
			return nil, err
		}
		if value.Mode == authoring.ModeAmend {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM team_deployments WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, value.Placement.TeamExpectedRevision).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current team.Deployment
			if json.Unmarshal([]byte(existing), &current) != nil || current.DefinitionID != application.teamDefinition.ID {
				return nil, authoring.ErrChangeSetRevision
			}
			application.teamDeployment.CreatedAt = current.CreatedAt
			application.teamActivation.FromVersion = current.ActiveVersion
		}
		teamDeploymentPayload, _ := json.Marshal(application.teamDeployment)
		teamActivationPayload, _ := json.Marshal(application.teamActivation)
		if value.Mode == authoring.ModeCreate {
			_, err = tx.ExecContext(ctx, `INSERT INTO team_deployments(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, application.teamDeployment.DefinitionID, application.teamDeployment.ActiveVersion, application.teamDeployment.Revision, application.teamDeployment.UpdatedAt, string(teamDeploymentPayload))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE team_deployments SET active_version=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, application.teamDeployment.ActiveVersion, application.teamDeployment.Revision, application.teamDeployment.UpdatedAt, string(teamDeploymentPayload), value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, value.Placement.TeamExpectedRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO team_definition_activations(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES(?,?,?,?,?,?,?)`, application.teamActivation.ID, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, application.teamDeployment.Revision, application.teamActivation.CreatedAt, string(teamActivationPayload)); err != nil {
			return nil, err
		}
	}
	for _, objective := range application.objectives {
		item := objective.value
		if objective.expectedRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM objectives WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID, item.ID, objective.expectedRevision).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current Objective
			if json.Unmarshal([]byte(existing), &current) != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			item.CreatedAt = current.CreatedAt
		}
		payload, _ := json.Marshal(item)
		if objective.expectedRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO objectives(id,scope_kind,scope_id,owner_type,owner_id,status,priority,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?,?,?)`, item.ID, value.Scope.Kind, value.Scope.ID, item.Owner.Type, item.Owner.ID, item.Status, item.Priority, item.Revision, item.UpdatedAt, string(payload))
		} else {
			var update sql.Result
			update, err = tx.ExecContext(ctx, `UPDATE objectives SET owner_type=?,owner_id=?,status=?,priority=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, item.Owner.Type, item.Owner.ID, item.Status, item.Priority, item.Revision, item.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, item.ID, objective.expectedRevision)
			if err == nil {
				if rows, _ := update.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	synchronizeWorkforceSkillBindingResources(application)
	value.ApplyReceipt.Resources = application.resources
	payload, _ := json.Marshal(value)
	result, err := tx.ExecContext(ctx, `UPDATE workforce_change_sets SET status=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=? AND candidate_digest=?`, value.Status, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision, authoring.ChangeSetReady, value.CandidateDigest)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, authoring.ErrChangeSetRevision
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return decodeChangeSet(string(payload))
}

func applySQLiteWorkforceSkillBindings(ctx context.Context, tx *sql.Tx, value *authoring.ChangeSet, desired []*capability.Binding) error {
	existing := map[string]*capability.Binding{}
	if value.Mode == authoring.ModeAmend {
		rows, err := tx.QueryContext(ctx, `SELECT payload FROM skill_bindings WHERE scope_kind=? AND scope_id=?`, value.Scope.Kind, value.Scope.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			var binding capability.Binding
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			if json.Unmarshal([]byte(payload), &binding) == nil && strings.HasPrefix(binding.ID, "workforce:") {
				existing[binding.ID] = &binding
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for _, binding := range desired {
		var definitionPayload string
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM skill_definitions WHERE id=? AND version=?`, binding.SkillID, binding.SkillVersion).Scan(&definitionPayload); err != nil {
			return fmt.Errorf("resolve Skill %s@%s for workforce binding: %w", binding.SkillID, binding.SkillVersion, err)
		}
		var definition capability.Definition
		if json.Unmarshal([]byte(definitionPayload), &definition) != nil || validateWorkforceBindingDefinition(binding, &definition) != nil {
			return fmt.Errorf("Skill %s@%s cannot satisfy workforce binding", binding.SkillID, binding.SkillVersion)
		}
		current := existing[binding.ID]
		if current != nil {
			binding.Revision = current.Revision + 1
		}
		payload, _ := json.Marshal(binding)
		if current == nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO skill_bindings(scope_kind,scope_id,deployment_id,id,skill_id,skill_version,revision,payload) VALUES(?,?,?,?,?,?,?,?)`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.Revision, string(payload)); err != nil {
				return err
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE skill_bindings SET skill_id=?,skill_version=?,revision=?,payload=? WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`, binding.SkillID, binding.SkillVersion, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, current.Revision)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return authoring.ErrChangeSetRevision
			}
			delete(existing, binding.ID)
		}
	}
	for _, binding := range existing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM skill_bindings WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.Revision); err != nil {
			return err
		}
	}
	return nil
}

var _ authoring.AtomicChangeSetStore = (*SQLiteStore)(nil)
