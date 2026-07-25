package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

const (
	sensitiveAuthoringPromptMigrationVersion       int64 = 29
	legacySensitiveAgentDefinitionMigrationVersion int64 = 30
)

func sanitizeAuthoringChangeSetPayload(payload string) (string, bool, error) {
	var value authoring.ChangeSet
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return "", false, fmt.Errorf("decode workforce ChangeSet for sensitive prompt migration: %w", err)
	}
	if !authoring.RedactSensitiveChangeSetPrompts(&value) {
		return payload, false, nil
	}
	encoded, err := json.Marshal(&value)
	if err != nil {
		return "", false, fmt.Errorf("encode workforce ChangeSet after sensitive prompt migration: %w", err)
	}
	return string(encoded), true, nil
}

type sensitivePromptMigrationRow struct {
	scopeKind, scopeID, id, payload string
}

type sensitiveAgentDefinitionMigrationRow struct {
	id, version, payload string
}

func sanitizeAgentDefinitionPayload(payload string, harden bool) (string, string, bool, error) {
	var value agent.AgentDefinition
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return "", "", false, fmt.Errorf("decode Agent definition for sensitive prompt migration: %w", err)
	}
	var changed bool
	var err error
	if harden {
		changed, err = authoring.HardenLegacySensitiveAgentDefinition(&value)
	} else {
		changed, err = authoring.RedactSensitiveAgentDefinition(&value)
	}
	if err != nil || !changed {
		return payload, value.Digest, changed, err
	}
	encoded, err := json.Marshal(&value)
	if err != nil {
		return "", "", false, fmt.Errorf("encode Agent definition after sensitive prompt migration: %w", err)
	}
	return string(encoded), value.Digest, true, nil
}

func (s *PostgresStore) migrateSensitiveAuthoringPrompts(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT scope_kind,scope_id,id,payload FROM `+s.table("workforce_change_sets"))
	if err != nil {
		return err
	}
	values := make([]sensitivePromptMigrationRow, 0)
	for rows.Next() {
		var value sensitivePromptMigrationRow
		if err := rows.Scan(&value.scopeKind, &value.scopeID, &value.id, &value.payload); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	affectedDefinitions := make(map[string]struct{})
	for _, value := range values {
		var before authoring.ChangeSet
		if err := json.Unmarshal([]byte(value.payload), &before); err != nil {
			return err
		}
		payload, changed, err := sanitizeAuthoringChangeSetPayload(value.payload)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		for _, definition := range before.Result.Candidate.Agents {
			if definition != nil {
				affectedDefinitions[definition.ID] = struct{}{}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("workforce_change_sets")+` SET payload=$1::jsonb WHERE scope_kind=$2 AND scope_id=$3 AND id=$4`, payload, value.scopeKind, value.scopeID, value.id); err != nil {
			return err
		}
	}

	definitionRows, err := tx.QueryContext(ctx, `SELECT id,version,payload FROM `+s.table("agent_definitions"))
	if err != nil {
		return err
	}
	definitions := make([]sensitiveAgentDefinitionMigrationRow, 0)
	for definitionRows.Next() {
		var value sensitiveAgentDefinitionMigrationRow
		if err := definitionRows.Scan(&value.id, &value.version, &value.payload); err != nil {
			definitionRows.Close()
			return err
		}
		definitions = append(definitions, value)
	}
	if err := definitionRows.Err(); err != nil {
		definitionRows.Close()
		return err
	}
	if err := definitionRows.Close(); err != nil {
		return err
	}
	for _, value := range definitions {
		var definition agent.AgentDefinition
		if err := json.Unmarshal([]byte(value.payload), &definition); err != nil {
			return err
		}
		changed, err := authoring.RedactSensitiveAgentDefinition(&definition)
		if _, affected := affectedDefinitions[value.id]; affected && err == nil {
			var hardened bool
			hardened, err = authoring.HardenLegacySensitiveAgentDefinition(&definition)
			changed = changed || hardened
		}
		if err != nil {
			return err
		}
		if changed {
			payload, marshalErr := json.Marshal(&definition)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_definitions")+` SET digest=$1,payload=$2::jsonb WHERE id=$3 AND version=$4`, definition.Digest, string(payload), value.id, value.version); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version,name) VALUES
		($1,'redact sensitive authoring prompts'),
		($2,'harden legacy sensitive Agent definitions')
		ON CONFLICT(version) DO NOTHING`, sensitiveAuthoringPromptMigrationVersion, legacySensitiveAgentDefinitionMigrationVersion)
	return err
}

func migrateSensitiveAuthoringPromptsSQLite(db *sql.DB) error {
	rows, err := db.Query(`SELECT scope_kind,scope_id,id,payload FROM workforce_change_sets`)
	if err != nil {
		return err
	}
	values := make([]sensitivePromptMigrationRow, 0)
	for rows.Next() {
		var value sensitivePromptMigrationRow
		if err := rows.Scan(&value.scopeKind, &value.scopeID, &value.id, &value.payload); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	affectedDefinitions := make(map[string]struct{})
	for _, value := range values {
		var before authoring.ChangeSet
		if err := json.Unmarshal([]byte(value.payload), &before); err != nil {
			return err
		}
		payload, changed, err := sanitizeAuthoringChangeSetPayload(value.payload)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		for _, definition := range before.Result.Candidate.Agents {
			if definition != nil {
				affectedDefinitions[definition.ID] = struct{}{}
			}
		}
		if _, err := db.Exec(`UPDATE workforce_change_sets SET payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, payload, value.scopeKind, value.scopeID, value.id); err != nil {
			return err
		}
	}

	definitionRows, err := db.Query(`SELECT id,version,payload FROM agent_definitions`)
	if err != nil {
		return err
	}
	definitions := make([]sensitiveAgentDefinitionMigrationRow, 0)
	for definitionRows.Next() {
		var value sensitiveAgentDefinitionMigrationRow
		if err := definitionRows.Scan(&value.id, &value.version, &value.payload); err != nil {
			definitionRows.Close()
			return err
		}
		definitions = append(definitions, value)
	}
	if err := definitionRows.Err(); err != nil {
		definitionRows.Close()
		return err
	}
	if err := definitionRows.Close(); err != nil {
		return err
	}
	for _, value := range definitions {
		_, harden := affectedDefinitions[value.id]
		payload, digest, changed, err := sanitizeAgentDefinitionPayload(value.payload, harden)
		if err != nil {
			return err
		}
		if changed {
			if _, err := db.Exec(`UPDATE agent_definitions SET digest=?,payload=? WHERE id=? AND version=?`, digest, payload, value.id, value.version); err != nil {
				return err
			}
		}
	}
	return nil
}
