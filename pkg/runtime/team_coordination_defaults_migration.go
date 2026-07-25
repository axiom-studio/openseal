package runtime

import (
	"context"
	"database/sql"
)

const teamCoordinationDefaultsMigrationVersion int64 = 25

const teamCoordinationDefaultsJSON = `{
	"maximumSpeakersPerRound": 2,
	"quietByDefault": true,
	"requireRoleRelevance": true,
	"suppressDuplicateContent": true
}`

// migrateTeamCoordinationDefaults makes the natural Team participation policy
// explicit in legacy durable definitions. Defaults are merged before existing
// values so authored false values and custom speaker limits remain authoritative.
func (s *PostgresStore) migrateTeamCoordinationDefaults(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`UPDATE ` + s.table("team_definitions") + `
		 SET payload = jsonb_set(
			payload,
			'{coordination}',
			$1::jsonb || COALESCE(payload->'coordination', '{}'::jsonb),
			true
		 )`,
		`UPDATE ` + s.table("team_definition_amendments") + `
		 SET payload = jsonb_set(
			payload,
			'{candidate,coordination}',
			$1::jsonb || COALESCE(payload #> '{candidate,coordination}', '{}'::jsonb),
			true
		 )
		 WHERE payload->'candidate' IS NOT NULL`,
		`UPDATE ` + s.table("workforce_change_sets") + `
		 SET payload = jsonb_set(
			payload,
			'{result,candidate,team,coordination}',
			$1::jsonb || COALESCE(payload #> '{result,candidate,team,coordination}', '{}'::jsonb),
			true
		 )
		 WHERE payload #> '{result,candidate,team}' IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement, teamCoordinationDefaultsJSON); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'explicit natural Team coordination defaults')
		ON CONFLICT (version) DO NOTHING`, teamCoordinationDefaultsMigrationVersion)
	return err
}

func migrateTeamCoordinationDefaultsSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`UPDATE team_definitions
		 SET payload = json_set(
			payload,
			'$.coordination',
			json_patch(json(?), COALESCE(json_extract(payload, '$.coordination'), json('{}')))
		 )`,
		`UPDATE team_definition_amendments
		 SET payload = json_set(
			payload,
			'$.candidate.coordination',
			json_patch(json(?), COALESCE(json_extract(payload, '$.candidate.coordination'), json('{}')))
		 )
		 WHERE json_type(payload, '$.candidate') IS NOT NULL`,
		`UPDATE workforce_change_sets
		 SET payload = json_set(
			payload,
			'$.result.candidate.team.coordination',
			json_patch(json(?), COALESCE(json_extract(payload, '$.result.candidate.team.coordination'), json('{}')))
		 )
		 WHERE json_type(payload, '$.result.candidate.team') IS NOT NULL`,
	} {
		if _, err = tx.Exec(statement, teamCoordinationDefaultsJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}
