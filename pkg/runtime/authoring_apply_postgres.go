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

func (s *PostgresStore) ApplyChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	a, err := materializeWorkforceApplication(value)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	var revision int64
	var currentPayload string
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,payload FROM `+s.table("workforce_change_sets")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, value.ID).Scan(&status, &revision, &currentPayload); err != nil {
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
	for i, definition := range a.agentDefinitions {
		p, _ := json.Marshal(definition)
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definitions")+`(id,version,digest,created_at,payload) VALUES($1,$2,$3,$4,$5::jsonb)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(p)); err != nil {
			return nil, err
		}
		deployment := a.agentDeployments[i]
		if value.Mode == authoring.ModeAmend {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_deployments")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, deployment.ID, value.Placement.AgentExpectedRevisions[definition.ID]).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current agent.AgentDeployment
			if json.Unmarshal([]byte(existing), &current) != nil || current.DefinitionID != definition.ID {
				return nil, authoring.ErrChangeSetRevision
			}
			deployment.PreviousVersion = current.ActiveVersion
			deployment.CreatedAt = current.CreatedAt
		}
		dp, _ := json.Marshal(deployment)
		activation := a.agentActivations[i]
		activation.FromVersion = deployment.PreviousVersion
		ap, _ := json.Marshal(activation)
		if value.Mode == authoring.ModeCreate {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_deployments")+`(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(dp))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE `+s.table("agent_deployments")+` SET active_version=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(dp), value.Scope.Kind, value.Scope.ID, deployment.ID, value.Placement.AgentExpectedRevisions[definition.ID])
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definition_activations")+`(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, activation.ID, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.Revision, activation.CreatedAt, string(ap)); err != nil {
			return nil, err
		}
	}
	if a.teamDefinition != nil {
		tdp, _ := json.Marshal(a.teamDefinition)
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definitions")+`(id,version,digest,created_at,payload) VALUES($1,$2,$3,$4,$5::jsonb)`, a.teamDefinition.ID, a.teamDefinition.Version, a.teamDefinition.Digest, a.teamDefinition.CreatedAt, string(tdp)); err != nil {
			return nil, err
		}
		if value.Mode == authoring.ModeAmend {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("team_deployments")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, value.Placement.TeamExpectedRevision).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current team.Deployment
			if json.Unmarshal([]byte(existing), &current) != nil || current.DefinitionID != a.teamDefinition.ID {
				return nil, authoring.ErrChangeSetRevision
			}
			a.teamDeployment.CreatedAt = current.CreatedAt
			a.teamActivation.FromVersion = current.ActiveVersion
		}
		tdp2, _ := json.Marshal(a.teamDeployment)
		tap, _ := json.Marshal(a.teamActivation)
		if value.Mode == authoring.ModeCreate {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_deployments")+`(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, a.teamDeployment.DefinitionID, a.teamDeployment.ActiveVersion, a.teamDeployment.Revision, a.teamDeployment.UpdatedAt, string(tdp2))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE `+s.table("team_deployments")+` SET active_version=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, a.teamDeployment.ActiveVersion, a.teamDeployment.Revision, a.teamDeployment.UpdatedAt, string(tdp2), value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, value.Placement.TeamExpectedRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definition_activations")+`(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, a.teamActivation.ID, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, a.teamDeployment.Revision, a.teamActivation.CreatedAt, string(tap)); err != nil {
			return nil, err
		}
	}
	for _, objective := range a.objectives {
		item := objective.value
		if objective.expectedRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("objectives")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, item.ID, objective.expectedRevision).Scan(&existing); err != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			var current Objective
			if json.Unmarshal([]byte(existing), &current) != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			item.CreatedAt = current.CreatedAt
		}
		p, _ := json.Marshal(item)
		if objective.expectedRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("objectives")+`(id,scope_kind,scope_id,owner_type,owner_id,status,priority,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, item.ID, value.Scope.Kind, value.Scope.ID, item.Owner.Type, item.Owner.ID, item.Status, item.Priority, item.Revision, item.UpdatedAt, string(p))
		} else {
			var update sql.Result
			update, err = tx.ExecContext(ctx, `UPDATE `+s.table("objectives")+` SET owner_type=$1,owner_id=$2,status=$3,priority=$4,revision=$5,updated_at=$6,payload=$7::jsonb WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`, item.Owner.Type, item.Owner.ID, item.Status, item.Priority, item.Revision, item.UpdatedAt, string(p), value.Scope.Kind, value.Scope.ID, item.ID, objective.expectedRevision)
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
	value.ApplyReceipt.Resources = a.resources
	p, _ := json.Marshal(value)
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("workforce_change_sets")+` SET status=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8 AND status=$9 AND candidate_digest=$10`, value.Status, value.Revision, value.UpdatedAt, string(p), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision, authoring.ChangeSetReady, value.CandidateDigest)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, authoring.ErrChangeSetRevision
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return decodeChangeSet(string(p))
}

var _ authoring.AtomicChangeSetStore = (*PostgresStore)(nil)
