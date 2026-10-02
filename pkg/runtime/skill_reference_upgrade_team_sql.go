package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type skillUpgradeTeamSQLTransaction interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func applySkillUpgradeTeamAuthoritySQL(ctx context.Context, tx skillUpgradeTeamSQLTransaction, mutation *SkillReferenceTeamAuthorityMutation,
	definitions, deployments, activations string, postgres bool,
) error {
	if mutation == nil {
		return nil
	}
	previous := mutation.PreviousDeployment
	lock := ""
	if postgres {
		lock = " FOR UPDATE"
	}
	var payload string
	query := skillUpgradeTeamSQLQuery(`SELECT payload FROM `+deployments+` WHERE scope_kind=? AND scope_id=? AND id=?`+lock, postgres)
	if err := tx.QueryRowContext(ctx, query, previous.Scope.Kind, previous.Scope.ID, previous.ID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSkillReferenceUpgradeConflict
		}
		return err
	}
	var current kernelteam.Deployment
	if err := json.Unmarshal([]byte(payload), &current); err != nil {
		return err
	}
	query = skillUpgradeTeamSQLQuery(`SELECT payload FROM `+definitions+` WHERE id=? AND version=?`, postgres)
	if err := tx.QueryRowContext(ctx, query, current.DefinitionID, current.ActiveVersion).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSkillReferenceUpgradeConflict
		}
		return err
	}
	var definition kernelteam.Definition
	if err := json.Unmarshal([]byte(payload), &definition); err != nil {
		return err
	}
	if err := validateCurrentSkillUpgradeTeamAuthority(&current, &definition, mutation); err != nil {
		return err
	}
	if mutation.Definition == nil {
		return nil
	}
	encodedDefinition, err := json.Marshal(mutation.Definition)
	if err != nil {
		return err
	}
	cast := ""
	if postgres {
		cast = "::jsonb"
	}
	query = skillUpgradeTeamSQLQuery(`INSERT INTO `+definitions+`(id,version,digest,created_at,payload) VALUES(?,?,?,?,?`+cast+`) ON CONFLICT(id,version) DO NOTHING`, postgres)
	if _, err := tx.ExecContext(ctx, query, mutation.Definition.ID, mutation.Definition.Version, mutation.Definition.Digest,
		mutation.Definition.CreatedAt, string(encodedDefinition)); err != nil {
		return err
	}
	// Another deployment can derive identical immutable behavior. Reuse that
	// record only when its complete content matches, independently of timestamps.
	query = skillUpgradeTeamSQLQuery(`SELECT payload FROM `+definitions+` WHERE id=? AND version=?`, postgres)
	if err := tx.QueryRowContext(ctx, query, mutation.Definition.ID, mutation.Definition.Version).Scan(&payload); err != nil {
		return err
	}
	var persisted kernelteam.Definition
	if json.Unmarshal([]byte(payload), &persisted) != nil || !skillUpgradeTeamDefinitionEqual(&persisted, mutation.Definition) {
		return ErrSkillReferenceUpgradeConflict
	}
	deploymentPayload, activationPayload, err := teamRegistryPayloads(mutation.Deployment, *mutation.Activation)
	if err != nil {
		return err
	}
	query = skillUpgradeTeamSQLQuery(`UPDATE `+deployments+` SET active_version=?,revision=?,updated_at=?,payload=?`+cast+`
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND definition_id=? AND active_version=?`, postgres)
	result, err := tx.ExecContext(ctx, query, mutation.Deployment.ActiveVersion, mutation.Deployment.Revision, mutation.Deployment.UpdatedAt, deploymentPayload,
		previous.Scope.Kind, previous.Scope.ID, previous.ID, previous.Revision, previous.DefinitionID, previous.ActiveVersion)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return ErrSkillReferenceUpgradeConflict
	}
	activation := mutation.Activation
	query = skillUpgradeTeamSQLQuery(`INSERT INTO `+activations+`(id,scope_kind,scope_id,deployment_id,deployment_revision,created_at,payload) VALUES(?,?,?,?,?,?,?`+cast+`)`, postgres)
	_, err = tx.ExecContext(ctx, query, activation.ID, activation.Scope.Kind, activation.Scope.ID, activation.DeploymentID,
		activation.DeploymentRevision, activation.CreatedAt, activationPayload)
	return err
}

// All callers pass fixed SQL templates. Table names come from the canonical
// store; values remain bound parameters in both SQL implementations.
func skillUpgradeTeamSQLQuery(query string, postgres bool) string {
	if !postgres {
		return query
	}
	var result strings.Builder
	index := 0
	for _, char := range query {
		if char == '?' {
			index++
			fmt.Fprintf(&result, "$%d", index)
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}
