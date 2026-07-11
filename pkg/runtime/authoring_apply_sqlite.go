package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/team"
)

func (s *SQLiteStore) ApplyChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
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

var _ authoring.AtomicChangeSetStore = (*SQLiteStore)(nil)
