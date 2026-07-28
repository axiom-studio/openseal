package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
)

const objectiveRunTemplateExclusivityMigrationVersion int64 = 34

type objectivePayloadMigrationRow struct {
	scopeKind string
	scopeID   string
	id        string
	payload   []byte
}

// migrateObjectiveRunTemplateExclusivity forward-ports Objective templates
// written before direct Skill actions and Agent Runbook entrypoints became
// mutually exclusive. A direct capability is already the complete execution
// contract, so the old decorative entrypoint carried no runtime authority and
// is removed without changing the Objective revision.
func (s *PostgresStore) migrateObjectiveRunTemplateExclusivity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT scope_kind,scope_id,id,payload FROM `+s.table("objectives"))
	if err != nil {
		return err
	}
	values := []objectivePayloadMigrationRow{}
	for rows.Next() {
		var value objectivePayloadMigrationRow
		if err = rows.Scan(&value.scopeKind, &value.scopeID, &value.id, &value.payload); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, value := range values {
		payload, changed, rewriteErr := forwardPortObjectiveRunTemplates(value.payload)
		if rewriteErr != nil {
			return rewriteErr
		}
		if !changed {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE `+s.table("objectives")+` SET payload=$1::jsonb WHERE scope_kind=$2 AND scope_id=$3 AND id=$4`,
			string(payload), value.scopeKind, value.scopeID, value.id); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES($1,'exclusive Objective Runbook entrypoints and Skill capabilities')
		ON CONFLICT(version) DO NOTHING`, objectiveRunTemplateExclusivityMigrationVersion)
	return err
}

func migrateObjectiveRunTemplateExclusivitySQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT scope_kind,scope_id,id,payload FROM objectives`)
	if err != nil {
		return err
	}
	values := []objectivePayloadMigrationRow{}
	for rows.Next() {
		var value objectivePayloadMigrationRow
		if err = rows.Scan(&value.scopeKind, &value.scopeID, &value.id, &value.payload); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, value := range values {
		payload, changed, rewriteErr := forwardPortObjectiveRunTemplates(value.payload)
		if rewriteErr != nil {
			return rewriteErr
		}
		if !changed {
			continue
		}
		if _, err = tx.Exec(`UPDATE objectives SET payload=? WHERE scope_kind=? AND scope_id=? AND id=?`,
			string(payload), value.scopeKind, value.scopeID, value.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func forwardPortObjectiveRunTemplates(payload []byte) ([]byte, bool, error) {
	var objective map[string]interface{}
	if err := json.Unmarshal(payload, &objective); err != nil {
		return nil, false, err
	}
	changed := removeRedundantRunTemplateEntrypoint(objective["cadence"])
	if eventRules, ok := objective["eventRules"].(map[string]interface{}); ok {
		if rules, ok := eventRules["rules"].([]interface{}); ok {
			for _, rule := range rules {
				changed = removeRedundantRunTemplateEntrypoint(rule) || changed
			}
		}
	}
	if !changed {
		return payload, false, nil
	}
	encoded, err := json.Marshal(objective)
	return encoded, true, err
}

func removeRedundantRunTemplateEntrypoint(parent interface{}) bool {
	container, ok := parent.(map[string]interface{})
	if !ok {
		return false
	}
	template, ok := container["runTemplate"].(map[string]interface{})
	if !ok || template["capability"] == nil {
		return false
	}
	if _, exists := template["entrypoint"]; !exists {
		return false
	}
	delete(template, "entrypoint")
	return true
}
