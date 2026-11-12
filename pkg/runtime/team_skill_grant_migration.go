package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// Team role Skill grants are gone: every roster member uses every Team
// binding. These are the payload locations that can carry a Team definition,
// with the JSON keys leading from the payload to that definition.
var teamSkillGrantDefinitionPaths = []struct {
	table string
	keys  []string
	// columns are the primary key columns, in order.
	columns []string
}{
	{"team_definitions", nil, []string{"id", "version"}},
	{"team_definition_amendments", []string{"candidate"}, []string{"scope_kind", "scope_id", "id"}},
}

type teamSkillGrantMigrationDB interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// stripTeamSkillGrants removes roles[].skillGrants from every stored Team
// definition and recomputes the digest of each stored definition version.
// table maps a bare table name to its qualified name; placeholder renders the
// n-th bind parameter, and "?" selects SQLite.
func stripTeamSkillGrants(ctx context.Context, db teamSkillGrantMigrationDB, table func(string) string, placeholder func(int) string) error {
	sqlite := placeholder(1) == "?"
	for _, location := range teamSkillGrantDefinitionPaths {
		payloadText, payloadValue := "payload::text", placeholder(1)+"::jsonb"
		if sqlite {
			payloadText, payloadValue = "payload", placeholder(1)
		}
		rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(location.columns, ", ")+`, `+payloadText+` FROM `+table(location.table)+` WHERE `+payloadText+` LIKE '%"skillGrants"%'`)
		if err != nil {
			return err
		}
		type rewrite struct {
			keys    []interface{}
			payload string
			digest  string
		}
		var rewrites []rewrite
		for rows.Next() {
			keys := make([]string, len(location.columns))
			targets := make([]interface{}, 0, len(keys)+1)
			for index := range keys {
				targets = append(targets, &keys[index])
			}
			var payload string
			targets = append(targets, &payload)
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				return err
			}
			var document map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &document); err != nil {
				rows.Close()
				return fmt.Errorf("%s %s: %w", location.table, strings.Join(keys, "/"), err)
			}
			definition := document
			for _, key := range location.keys {
				next, _ := definition[key].(map[string]interface{})
				definition = next
			}
			if definition == nil {
				continue
			}
			roles, _ := definition["roles"].([]interface{})
			changed := false
			for _, role := range roles {
				if fields, ok := role.(map[string]interface{}); ok {
					if _, present := fields["skillGrants"]; present {
						delete(fields, "skillGrants")
						changed = true
					}
				}
			}
			if !changed {
				continue
			}
			item := rewrite{}
			for _, key := range keys {
				item.keys = append(item.keys, key)
			}
			if location.keys == nil {
				encoded, _ := json.Marshal(definition)
				var typed kernelteam.Definition
				if err := json.Unmarshal(encoded, &typed); err != nil {
					rows.Close()
					return fmt.Errorf("%s %s: %w", location.table, strings.Join(keys, "/"), err)
				}
				item.digest = kernelteam.DefinitionDigest(&typed)
				definition["digest"] = item.digest
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				rows.Close()
				return err
			}
			item.payload = string(encoded)
			rewrites = append(rewrites, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, item := range rewrites {
			set := `payload = ` + payloadValue
			args := []interface{}{item.payload}
			if item.digest != "" {
				set += `, digest = ` + placeholder(2)
				args = append(args, item.digest)
			}
			where := make([]string, len(location.columns))
			for index, column := range location.columns {
				args = append(args, item.keys[index])
				where[index] = column + ` = ` + placeholder(len(args))
			}
			if _, err := db.ExecContext(ctx, `UPDATE `+table(location.table)+` SET `+set+` WHERE `+strings.Join(where, ` AND `), args...); err != nil {
				return err
			}
		}
	}
	return nil
}
