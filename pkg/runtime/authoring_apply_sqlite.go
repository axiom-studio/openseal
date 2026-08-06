package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/mattn/go-sqlite3"
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
	value.ApplyReceipt.Activation = application.activation
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
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_definitions(id,version,digest,created_at,payload) VALUES(?,?,?,?,?)`, definition.ID, definition.Version, definition.Digest, definition.CreatedAt, string(payload)); err != nil {
			return nil, err
		}
		var storedDefinitionDigest, storedDefinitionPayload string
		if err = tx.QueryRowContext(ctx, `SELECT digest,payload FROM agent_definitions WHERE id=? AND version=?`, definition.ID, definition.Version).Scan(&storedDefinitionDigest, &storedDefinitionPayload); err != nil {
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
		deployment := application.agentDeployments[index]
		expectedDeploymentRevision := value.Placement.AgentExpectedRevisions[definition.ID]
		if expectedDeploymentRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM agent_deployments WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID, deployment.ID, expectedDeploymentRevision).Scan(&existing); err != nil {
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
		if expectedDeploymentRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO agent_deployments(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.DefinitionID, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(deploymentPayload))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE agent_deployments SET active_version=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt, string(deploymentPayload), value.Scope.Kind, value.Scope.ID, deployment.ID, expectedDeploymentRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			if expectedDeploymentRevision == 0 && sqliteUniqueConstraint(err) {
				return nil, workforceAgentDeploymentIdentityConflict(deployment.ID)
			}
			return nil, err
		}
		if application.activation == authoring.WorkforceActivationActive {
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_definition_activations(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES(?,?,?,?,?,?,?)`, activation.ID, value.Scope.Kind, value.Scope.ID, deployment.ID, deployment.Revision, activation.CreatedAt, string(activationPayload)); err != nil {
				return nil, err
			}
		}
	}
	if err = applySQLiteWorkforceSkillBindings(ctx, tx, value, application.skillBindings); err != nil {
		return nil, err
	}
	if application.teamDefinition != nil {
		teamDefinitionPayload, _ := json.Marshal(application.teamDefinition)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO team_definitions(id,version,digest,created_at,payload) VALUES(?,?,?,?,?)`, application.teamDefinition.ID, application.teamDefinition.Version, application.teamDefinition.Digest, application.teamDefinition.CreatedAt, string(teamDefinitionPayload)); err != nil {
			return nil, err
		}
		var storedTeamDigest, storedTeamPayload string
		if err = tx.QueryRowContext(ctx, `SELECT digest,payload FROM team_definitions WHERE id=? AND version=?`, application.teamDefinition.ID, application.teamDefinition.Version).Scan(&storedTeamDigest, &storedTeamPayload); err != nil {
			return nil, authoring.ErrChangeSetRevision
		}
		if storedTeamDigest != application.teamDefinition.Digest {
			var stored team.Definition
			if json.Unmarshal([]byte(storedTeamPayload), &stored) != nil {
				return nil, authoring.ErrChangeSetRevision
			}
			application.teamDefinition.CreatedAt, application.teamDefinition.Digest = stored.CreatedAt, ""
			application.teamDefinition.Digest = portableDigest(application.teamDefinition)
			if application.teamDefinition.Digest != storedTeamDigest {
				return nil, authoring.ErrChangeSetRevision
			}
		}
		expectedTeamRevision := value.Placement.TeamExpectedRevision
		if expectedTeamRevision > 0 {
			var existing string
			if err = tx.QueryRowContext(ctx, `SELECT payload FROM team_deployments WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, expectedTeamRevision).Scan(&existing); err != nil {
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
		if expectedTeamRevision == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO team_deployments(scope_kind,scope_id,id,definition_id,active_version,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, application.teamDeployment.DefinitionID, application.teamDeployment.ActiveVersion, application.teamDeployment.Revision, application.teamDeployment.UpdatedAt, string(teamDeploymentPayload))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE team_deployments SET active_version=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, application.teamDeployment.ActiveVersion, application.teamDeployment.Revision, application.teamDeployment.UpdatedAt, string(teamDeploymentPayload), value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, expectedTeamRevision)
			if err == nil {
				if rows, _ := result.RowsAffected(); rows != 1 {
					return nil, authoring.ErrChangeSetRevision
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if application.activation == authoring.WorkforceActivationActive {
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_definition_activations(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES(?,?,?,?,?,?,?)`, application.teamActivation.ID, value.Scope.Kind, value.Scope.ID, application.teamDeployment.ID, application.teamDeployment.Revision, application.teamActivation.CreatedAt, string(teamActivationPayload)); err != nil {
				return nil, err
			}
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
			if item.UpdatedAt.Before(item.CreatedAt) {
				item.UpdatedAt = item.CreatedAt
			}
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
	if err = applySQLiteWorkforceRunbookActivations(ctx, tx, value, application); err != nil {
		return nil, err
	}
	if err = applySQLiteWorkforceProject(ctx, tx, application.project, application.projectExpectedRevision); err != nil {
		return nil, err
	}
	if err = synchronizeWorkforceConversationEndpointBindings(application); err != nil {
		return nil, err
	}
	if err = applySQLiteWorkforceConversationEndpoints(ctx, tx, value, application); err != nil {
		return nil, err
	}
	if err = applySQLiteWorkforceCallbackRegistrations(ctx, tx, value, application); err != nil {
		return nil, err
	}
	synchronizeWorkforceSkillBindingResources(application)
	sortWorkforceApplicationResources(application)
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

func applySQLiteWorkforceRunbookActivations(ctx context.Context, tx *sql.Tx, value *authoring.ChangeSet, application *workforceApplication) error {
	desiredByAgent := map[string]map[string]bool{}
	for _, desired := range application.runbookActivations {
		item := desired.value
		if desiredByAgent[item.AssignedAgentID] == nil {
			desiredByAgent[item.AssignedAgentID] = map[string]bool{}
		}
		desiredByAgent[item.AssignedAgentID][item.ID] = true
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT payload FROM runbook_activations WHERE scope_kind=? AND scope_id=? AND id=?`, value.Scope.Kind, value.Scope.ID, item.ID).Scan(&payload)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			item.Revision = 1
		case err != nil:
			return err
		default:
			var current RunbookActivation
			if json.Unmarshal([]byte(payload), &current) != nil || current.AssignedAgentID != item.AssignedAgentID {
				return authoring.ErrChangeSetRevision
			}
			item.CreatedAt = current.CreatedAt
			if item.UpdatedAt.Before(item.CreatedAt) {
				item.UpdatedAt = item.CreatedAt
			}
			if reflect.DeepEqual(current.Trigger.Schedule, item.Trigger.Schedule) {
				item.NextOccurrenceBase, item.NextRunAt = current.NextOccurrenceBase, current.NextRunAt
			}
			item.Revision = current.Revision + 1
		}
		if err := item.Validate(); err != nil {
			return fmt.Errorf("materialized Runbook activation %s revision=%d createdAt=%s updatedAt=%s: %w", item.ID, item.Revision, item.CreatedAt, item.UpdatedAt, err)
		}
		encoded, _ := json.Marshal(item)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO runbook_activations (id,scope_kind,scope_id,owner_type,owner_id,objective_id,assigned_agent_id,status,next_run_at,revision,updated_at,idempotency_key_hash,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.ID, item.Scope.Kind, item.Scope.ID, item.Owner.Type, item.Owner.ID, item.ObjectiveID, item.AssignedAgentID, item.Status, item.NextRunAt, item.Revision, item.UpdatedAt, item.IdempotencyKeyHash, string(encoded))
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE runbook_activations SET owner_type=?,owner_id=?,objective_id=?,assigned_agent_id=?,status=?,next_run_at=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, item.Owner.Type, item.Owner.ID, item.ObjectiveID, item.AssignedAgentID, item.Status, item.NextRunAt, item.Revision, item.UpdatedAt, string(encoded), item.Scope.Kind, item.Scope.ID, item.ID)
		}
		if err != nil {
			return err
		}
		synchronizeWorkforceRunbookResource(application, item)
	}
	for _, deploymentID := range value.Placement.AgentDeploymentIDs {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM runbook_activations WHERE scope_kind=? AND scope_id=? AND assigned_agent_id=?`, value.Scope.Kind, value.Scope.ID, deploymentID)
		if err != nil {
			return err
		}
		var obsolete []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !desiredByAgent[deploymentID][id] {
				obsolete = append(obsolete, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range obsolete {
			if _, err := tx.ExecContext(ctx, `DELETE FROM runbook_activations WHERE scope_kind=? AND scope_id=? AND id=?`, value.Scope.Kind, value.Scope.ID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func applySQLiteWorkforceConversationEndpoints(
	ctx context.Context,
	tx *sql.Tx,
	value *authoring.ChangeSet,
	application *workforceApplication,
) error {
	desiredIDs := make(map[string]bool, len(application.conversationEndpoints))
	for index := range application.conversationEndpoints {
		desired := application.conversationEndpoints[index]
		endpoint := desired.value
		desiredIDs[endpoint.ID] = true
		if desired.expectedRevision > 0 {
			var payload string
			if err := tx.QueryRowContext(
				ctx,
				`SELECT payload FROM external_conversation_endpoints
				 WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
				value.Scope.Kind, value.Scope.ID, endpoint.ID, desired.expectedRevision,
			).Scan(&payload); err != nil {
				return authoring.ErrChangeSetRevision
			}
			var current ExternalConversationEndpoint
			if json.Unmarshal([]byte(payload), &current) != nil ||
				current.Owner != endpoint.Owner || current.DeploymentID != endpoint.DeploymentID {
				return authoring.ErrChangeSetRevision
			}
			endpoint.CreatedAt = current.CreatedAt
			endpoint.IngressRoute = current.IngressRoute
		}
		payload, _ := json.Marshal(endpoint)
		if desired.expectedRevision == 0 {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO external_conversation_endpoints
				 (scope_kind,scope_id,id,ingress_route,owner_type,owner_id,provider,status,revision,updated_at,payload)
				 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute,
				endpoint.Owner.Type, endpoint.Owner.ID, endpoint.Provider, endpoint.Status,
				endpoint.Revision, endpoint.UpdatedAt, string(payload),
			); err != nil {
				if sqliteUniqueConstraint(err) {
					return authoring.ErrChangeSetRevision
				}
				return err
			}
		} else {
			result, err := tx.ExecContext(
				ctx,
				`UPDATE external_conversation_endpoints
				 SET provider=?,status=?,revision=?,updated_at=?,payload=?
				 WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
				endpoint.Provider, endpoint.Status, endpoint.Revision, endpoint.UpdatedAt, string(payload),
				endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, desired.expectedRevision,
			)
			if err != nil {
				return err
			}
			if rows, _ := result.RowsAffected(); rows != 1 {
				return authoring.ErrChangeSetRevision
			}
		}
	}
	reconciled := workforceBindingReconciliationDeployments(value)
	if len(reconciled) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(
		ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=?`,
		value.Scope.Kind, value.Scope.ID,
	)
	if err != nil {
		return err
	}
	current := make([]*ExternalConversationEndpoint, 0)
	for rows.Next() {
		var payload string
		var endpoint ExternalConversationEndpoint
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return err
		}
		if json.Unmarshal([]byte(payload), &endpoint) == nil &&
			reconciled[endpoint.DeploymentID] && !desiredIDs[endpoint.ID] &&
			endpoint.Status != ExternalConversationEndpointRetired {
			current = append(current, &endpoint)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, endpoint := range current {
		expectedRevision := endpoint.Revision
		now := value.ApplyReceipt.AppliedAt
		endpoint.Status, endpoint.Revision, endpoint.UpdatedAt, endpoint.RetiredAt =
			ExternalConversationEndpointRetired, expectedRevision+1, now, &now
		if err := endpoint.Validate(); err != nil {
			return err
		}
		payload, _ := json.Marshal(endpoint)
		result, err := tx.ExecContext(
			ctx,
			`UPDATE external_conversation_endpoints
			 SET status=?,revision=?,updated_at=?,payload=?
			 WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			endpoint.Status, endpoint.Revision, endpoint.UpdatedAt, string(payload),
			endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, expectedRevision,
		)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return authoring.ErrChangeSetRevision
		}
		application.resources = append(application.resources, authoring.AppliedResourceReference{
			Kind: "conversation_endpoint", ID: endpoint.ID, Revision: endpoint.Revision,
		})
	}
	return nil
}

func applySQLiteWorkforceCallbackRegistrations(
	ctx context.Context,
	tx *sql.Tx,
	value *authoring.ChangeSet,
	application *workforceApplication,
) error {
	desiredIDs := make(map[string]bool, len(application.callbackRegistrations))
	for index := range application.callbackRegistrations {
		desired := application.callbackRegistrations[index]
		registration := desired.value
		desiredIDs[registration.ID] = true
		var current *CallbackRegistration
		if desired.expectedRevision > 0 {
			var payload string
			if err := tx.QueryRowContext(ctx, `SELECT payload FROM callback_registrations
				WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Scope.Kind, value.Scope.ID,
				registration.ID, desired.expectedRevision).Scan(&payload); err != nil {
				return authoring.ErrChangeSetRevision
			}
			current = &CallbackRegistration{}
			if json.Unmarshal([]byte(payload), current) != nil {
				return authoring.ErrChangeSetRevision
			}
		}
		if err := prepareWorkforceCallbackRegistration(registration, current, value.Actor, value.ApplyReceipt.AppliedAt); err != nil {
			return err
		}
		payload, _ := json.Marshal(registration)
		if desired.expectedRevision == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO callback_registrations
				(scope_kind,scope_id,id,ingress_route,provider,status,revision,updated_at,payload)
				VALUES(?,?,?,?,?,?,?,?,?)`, registration.Scope.Kind, registration.Scope.ID, registration.ID,
				registration.IngressRoute, registration.Provider, registration.Status, registration.Revision,
				registration.UpdatedAt, string(payload)); err != nil {
				if sqliteUniqueConstraint(err) {
					return authoring.ErrChangeSetRevision
				}
				return err
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE callback_registrations SET ingress_route=?,provider=?,status=?,revision=?,updated_at=?,payload=?
				WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, registration.IngressRoute,
				registration.Provider, registration.Status, registration.Revision, registration.UpdatedAt, string(payload),
				registration.Scope.Kind, registration.Scope.ID, registration.ID, desired.expectedRevision)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return authoring.ErrChangeSetRevision
			}
		}
		application.resources = append(application.resources, authoring.AppliedResourceReference{
			Kind: "callback_registration", ID: registration.ID, Revision: registration.Revision,
		})
	}
	return retireSQLiteWorkforceCallbacks(ctx, tx, value, application, desiredIDs)
}

func retireSQLiteWorkforceCallbacks(ctx context.Context, tx *sql.Tx, value *authoring.ChangeSet, application *workforceApplication, desiredIDs map[string]bool) error {
	reconciled := workforceBindingReconciliationDeployments(value)
	if len(reconciled) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM callback_registrations WHERE scope_kind=? AND scope_id=?`, value.Scope.Kind, value.Scope.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var registration CallbackRegistration
		if json.Unmarshal([]byte(payload), &registration) != nil || !reconciled[registration.DeploymentID] ||
			desiredIDs[registration.ID] || registration.Status == CallbackRegistrationRetired {
			continue
		}
		expected := registration.Revision
		now := value.ApplyReceipt.AppliedAt
		registration.Status, registration.Revision, registration.UpdatedAt, registration.RetiredAt =
			CallbackRegistrationRetired, expected+1, now, &now
		registration.Lifecycle = append(registration.Lifecycle, CallbackRegistrationLifecycleEntry{
			Revision: registration.Revision, Action: CallbackRegistrationRetiredAction,
			Actor: ActivityActor{Type: value.Actor.Type, ID: value.Actor.ID}, Reason: "retire removed workflow callback", At: now,
		})
		if err := registration.Validate(); err != nil {
			return err
		}
		encoded, _ := json.Marshal(&registration)
		result, err := tx.ExecContext(ctx, `UPDATE callback_registrations SET status=?,revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, registration.Status, registration.Revision,
			registration.UpdatedAt, string(encoded), registration.Scope.Kind, registration.Scope.ID, registration.ID, expected)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return authoring.ErrChangeSetRevision
		}
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "callback_registration", ID: registration.ID, Revision: registration.Revision})
	}
	return rows.Err()
}

func sqliteUniqueConstraint(err error) bool {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey ||
		sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}

func applySQLiteWorkforceProject(ctx context.Context, tx *sql.Tx, project *Project, expectedRevision int64) error {
	if project == nil {
		return nil
	}
	if expectedRevision > 0 {
		var payload string
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM projects WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, project.Scope.Kind, project.Scope.ID, project.ID, expectedRevision).Scan(&payload); err != nil {
			return authoring.ErrChangeSetRevision
		}
		var current Project
		if json.Unmarshal([]byte(payload), &current) != nil {
			return authoring.ErrChangeSetRevision
		}
		project.CreatedAt = current.CreatedAt
		project.IdempotencyKeyHash = current.IdempotencyKeyHash
		project.CreationFingerprint = current.CreationFingerprint
	}
	if err := project.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(project)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO projects(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload) VALUES(?,?,?,?,?,?,?,?,?,?)`, project.ID, project.Scope.Kind, project.Scope.ID, project.Owner.Type, project.Owner.ID, project.Status, project.Revision, project.UpdatedAt, project.IdempotencyKeyHash, string(payload))
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE projects SET owner_type=?,owner_id=?,status=?,revision=?,updated_at=?,idempotency_key_hash=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, project.Owner.Type, project.Owner.ID, project.Status, project.Revision, project.UpdatedAt, project.IdempotencyKeyHash, string(payload), project.Scope.Kind, project.Scope.ID, project.ID, expectedRevision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return authoring.ErrChangeSetRevision
	}
	return nil
}

func applySQLiteWorkforceSkillBindings(ctx context.Context, tx *sql.Tx, value *authoring.ChangeSet, desired []*capability.Binding) error {
	existing := map[string]*capability.Binding{}
	desiredIDs := make(map[string]bool, len(desired))
	for _, binding := range desired {
		if binding != nil {
			desiredIDs[binding.ID] = true
		}
	}
	reconciledDeployments := workforceBindingReconciliationDeployments(value)
	if len(reconciledDeployments) > 0 {
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
			if json.Unmarshal([]byte(payload), &binding) == nil && reconciledDeployments[binding.DeploymentID] &&
				(strings.HasPrefix(binding.ID, "workforce:") || desiredIDs[binding.ID]) {
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
		definitionPayload, err := resolveSQLiteWorkforceSkillDefinition(ctx, tx, binding)
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO skill_bindings(scope_kind,scope_id,deployment_id,id,skill_id,skill_version,source_identity,revision,payload) VALUES(?,?,?,?,?,?,?,?,?)`, binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload)); err != nil {
				return err
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE skill_bindings SET skill_id=?,skill_version=?,source_identity=?,revision=?,payload=? WHERE scope_kind=? AND scope_id=? AND deployment_id=? AND id=? AND revision=?`, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision, string(payload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, current.Revision)
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

func resolveSQLiteWorkforceSkillDefinition(ctx context.Context, tx *sql.Tx, binding *capability.Binding) (string, error) {
	query := `SELECT source_identity,payload FROM skill_definitions WHERE id=? AND version=?`
	arguments := []interface{}{binding.SkillID, binding.SkillVersion}
	if binding.SourceIdentity != "" {
		query += ` AND source_identity=?`
		arguments = append(arguments, binding.SourceIdentity)
	}
	query += ` ORDER BY source_identity`
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

var _ authoring.AtomicChangeSetStore = (*SQLiteStore)(nil)
