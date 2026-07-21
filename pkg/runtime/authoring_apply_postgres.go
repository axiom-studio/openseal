package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
)

func (s *PostgresStore) ApplyChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	a, err := materializeWorkforceApplication(value)
	if err != nil {
		return nil, err
	}
	value.ApplyReceipt.Activation = a.activation
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definitions")+`(id,version,digest,created_at,payload) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT (id,version) DO NOTHING`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(p)); err != nil {
			return nil, err
		}
		var storedDefinitionDigest, storedDefinitionPayload string
		if err = tx.QueryRowContext(ctx, `SELECT digest,payload FROM `+s.table("agent_definitions")+` WHERE id=$1 AND version=$2`, definition.ID, definition.Version).Scan(&storedDefinitionDigest, &storedDefinitionPayload); err != nil {
			return nil, authoring.ErrChangeSetRevision
		}
		if storedDefinitionDigest != definition.Digest {
			var stored agent.AgentDefinition
			if json.Unmarshal([]byte(storedDefinitionPayload), &stored) != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			definition.CreatedAt, definition.Digest = stored.CreatedAt, ""
			definition.Digest = portableDigest(definition)
			if definition.Digest != storedDefinitionDigest {
				return nil, authoring.ErrChangeSetRevision
			}
		}
		deployment := a.agentDeployments[i]
		expectedDeploymentRevision := value.Placement.AgentExpectedRevisions[definition.ID]
		if expectedDeploymentRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_deployments")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, deployment.ID, expectedDeploymentRevision).Scan(&existing); err != nil {
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
		if expectedDeploymentRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_deployments")+`(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(dp))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE `+s.table("agent_deployments")+` SET active_version=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(dp), value.Scope.Kind, value.Scope.ID, deployment.ID, expectedDeploymentRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if a.activation == authoring.WorkforceActivationActive {
			if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_definition_activations")+`(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, activation.ID, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.Revision, activation.CreatedAt, string(ap)); err != nil {
				return nil, err
			}
		}
	}
	if err = applyPostgresWorkforceSkillBindings(ctx, tx, s.table("skill_bindings"), s.table("skill_definitions"), value, a.skillBindings); err != nil {
		return nil, err
	}
	if a.teamDefinition != nil {
		tdp, _ := json.Marshal(a.teamDefinition)
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definitions")+`(id,version,digest,created_at,payload) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT (id,version) DO NOTHING`, a.teamDefinition.ID, a.teamDefinition.Version, a.teamDefinition.Digest, a.teamDefinition.CreatedAt, string(tdp)); err != nil {
			return nil, err
		}
		var storedTeamDigest, storedTeamPayload string
		if err = tx.QueryRowContext(ctx, `SELECT digest,payload FROM `+s.table("team_definitions")+` WHERE id=$1 AND version=$2`, a.teamDefinition.ID, a.teamDefinition.Version).Scan(&storedTeamDigest, &storedTeamPayload); err != nil {
			return nil, authoring.ErrChangeSetRevision
		}
		if storedTeamDigest != a.teamDefinition.Digest {
			var stored team.Definition
			if json.Unmarshal([]byte(storedTeamPayload), &stored) != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			a.teamDefinition.CreatedAt, a.teamDefinition.Digest = stored.CreatedAt, ""
			a.teamDefinition.Digest = portableDigest(a.teamDefinition)
			if a.teamDefinition.Digest != storedTeamDigest {
				return nil, authoring.ErrChangeSetRevision
			}
		}
		expectedTeamRevision := value.Placement.TeamExpectedRevision
		if expectedTeamRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("team_deployments")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, expectedTeamRevision).Scan(&existing); err != nil {
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
		if expectedTeamRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_deployments")+`(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, a.teamDeployment.DefinitionID, a.teamDeployment.ActiveVersion, a.teamDeployment.Revision, a.teamDeployment.UpdatedAt, string(tdp2))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE `+s.table("team_deployments")+` SET active_version=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, a.teamDeployment.ActiveVersion, a.teamDeployment.Revision, a.teamDeployment.UpdatedAt, string(tdp2), value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, expectedTeamRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if a.activation == authoring.WorkforceActivationActive {
			if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("team_definition_activations")+`(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, a.teamActivation.ID, value.Scope.Kind, value.Scope.ID, a.teamDeployment.ID, a.teamDeployment.Revision, a.teamActivation.CreatedAt, string(tap)); err != nil {
				return nil, err
			}
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
	if err = applyPostgresWorkforceInitiative(ctx, tx, s.table("initiatives"), a.initiative, a.initiativeExpectedRevision); err != nil {
		return nil, err
	}
	synchronizeWorkforceSkillBindingResources(a)
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

func applyPostgresWorkforceInitiative(ctx context.Context, tx *sql.Tx, table string, initiative *Initiative, expectedRevision int64) error {
	if initiative == nil {
		return nil
	}
	if expectedRevision > 0 {
		var payload []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4 FOR UPDATE`, initiative.Scope.Kind, initiative.Scope.ID, initiative.ID, expectedRevision).Scan(&payload); err != nil {
			return authoring.ErrChangeSetRevision
		}
		var current Initiative
		if json.Unmarshal(payload, &current) != nil {
			return authoring.ErrChangeSetRevision
		}
		initiative.CreatedAt = current.CreatedAt
		initiative.IdempotencyKeyHash = current.IdempotencyKeyHash
		initiative.CreationFingerprint = current.CreationFingerprint
	}
	if err := initiative.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(initiative)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, initiative.ID, initiative.Scope.Kind, initiative.Scope.ID, initiative.Owner.Type, initiative.Owner.ID, initiative.Status, initiative.Revision, initiative.UpdatedAt, initiative.IdempotencyKeyHash, string(payload))
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET owner_type=$1,owner_id=$2,status=$3,revision=$4,updated_at=$5,idempotency_key_hash=$6,payload=$7::jsonb WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`, initiative.Owner.Type, initiative.Owner.ID, initiative.Status, initiative.Revision, initiative.UpdatedAt, initiative.IdempotencyKeyHash, string(payload), initiative.Scope.Kind, initiative.Scope.ID, initiative.ID, expectedRevision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return authoring.ErrChangeSetRevision
	}
	return nil
}

func applyPostgresWorkforceSkillBindings(ctx context.Context, tx *sql.Tx, bindingTable, definitionTable string, value *authoring.ChangeSet, desired []*capability.Binding) error {
	existing := map[string]*capability.Binding{}
	reconciledDeployments := workforceBindingReconciliationDeployments(value)
	if len(reconciledDeployments) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT payload FROM `+bindingTable+` WHERE scope_kind=$1 AND scope_id=$2 FOR UPDATE`, value.Scope.Kind, value.Scope.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			var binding capability.Binding
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			if json.Unmarshal([]byte(payload), &binding) == nil && strings.HasPrefix(binding.ID, "workforce:") && reconciledDeployments[binding.DeploymentID] {
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
		definitionPayload, err := resolvePostgresWorkforceSkillDefinition(ctx, tx, definitionTable, binding)
		if err != nil {
			return fmt.Errorf("resolve Skill %s@%s for workforce binding: %w", binding.SkillID, binding.SkillVersion, err)
		}
		var definition capability.Definition
		if json.Unmarshal([]byte(definitionPayload), &definition) != nil {
			return fmt.Errorf("Skill %s@%s cannot satisfy workforce binding", binding.SkillID, binding.SkillVersion)
		}
		if binding.SourceIdentity == "" && definition.Source != nil {
			binding.SourceIdentity = strings.TrimSpace(definition.Source.Identity)
		}
		if validateWorkforceBindingDefinition(binding, &definition) != nil {
			return fmt.Errorf("Skill %s@%s cannot satisfy workforce binding", binding.SkillID, binding.SkillVersion)
		}
		current := existing[binding.ID]
		if current != nil {
			binding.Revision = current.Revision + 1
		}
		payload, _ := json.Marshal(binding)
		if current == nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+bindingTable+`(scope_kind,scope_id,deployment_id,id,skill_id,skill_version,source_identity,revision,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload)); err != nil {
				return err
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE `+bindingTable+` SET skill_id=$1,skill_version=$2,source_identity=$3,revision=$4,payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND deployment_id=$8 AND id=$9 AND revision=$10`, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, current.Revision)
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+bindingTable+` WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND id=$4 AND revision=$5`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.Revision); err != nil {
			return err
		}
	}
	return nil
}

func resolvePostgresWorkforceSkillDefinition(ctx context.Context, tx *sql.Tx, table string, binding *capability.Binding) (string, error) {
	query := `SELECT source_identity,payload FROM ` + table + ` WHERE id=$1 AND version=$2`
	arguments := []interface{}{binding.SkillID, binding.SkillVersion}
	if binding.SourceIdentity != "" {
		query += ` AND source_identity=$3`
		arguments = append(arguments, binding.SourceIdentity)
	}
	query += ` ORDER BY source_identity FOR SHARE`
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var payload string
	count := 0
	for rows.Next() {
		var sourceIdentity, candidate string
		if err := rows.Scan(&sourceIdentity, &candidate); err != nil {
			return "", err
		}
		if sourceIdentity != binding.SourceIdentity && binding.SourceIdentity != "" {
			return "", errors.New("selected Skill source does not match persisted provenance")
		}
		payload, count = candidate, count+1
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "", sql.ErrNoRows
	}
	if count > 1 {
		return "", skill.ErrDefinitionAmbiguous
	}
	return payload, nil
}

var _ authoring.AtomicChangeSetStore = (*PostgresStore)(nil)
