package runtime

import (
	"context"
	"database/sql"
)

const naturalTeamCoordinationMigrationVersion int64 = 24

// migrateNaturalTeamCoordination removes the retired coordination-mode label
// from every durable artifact that can carry a Team definition. Team channels
// now have one relevance-arbitrated model governed by explicit participation
// controls, so retaining dynamic/peer/leader labels would misrepresent runtime
// behavior.
func (s *PostgresStore) migrateNaturalTeamCoordination(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`UPDATE ` + s.table("team_definitions") + `
		 SET payload = payload #- '{coordination,mode}'
		 WHERE payload #> '{coordination,mode}' IS NOT NULL`,
		`UPDATE ` + s.table("team_definition_amendments") + `
		 SET payload = payload #- '{candidate,coordination,mode}'
		 WHERE payload #> '{candidate,coordination,mode}' IS NOT NULL`,
		`UPDATE ` + s.table("workforce_change_sets") + `
		 SET payload = payload #- '{result,candidate,team,coordination,mode}'
		 WHERE payload #> '{result,candidate,team,coordination,mode}' IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'single natural Team coordination model')
		ON CONFLICT (version) DO NOTHING`, naturalTeamCoordinationMigrationVersion)
	return err
}

func migrateNaturalTeamCoordinationSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`UPDATE team_definitions
		 SET payload = json_remove(payload, '$.coordination.mode')
		 WHERE json_type(payload, '$.coordination.mode') IS NOT NULL`,
		`UPDATE team_definition_amendments
		 SET payload = json_remove(payload, '$.candidate.coordination.mode')
		 WHERE json_type(payload, '$.candidate.coordination.mode') IS NOT NULL`,
		`UPDATE workforce_change_sets
		 SET payload = json_remove(payload, '$.result.candidate.team.coordination.mode')
		 WHERE json_type(payload, '$.result.candidate.team.coordination.mode') IS NOT NULL`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
