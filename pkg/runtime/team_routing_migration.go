package runtime

import (
	"context"
	"database/sql"
)

// Migrations 24 and 25 rewrote the retired Team coordination policy. Fresh
// databases still record them so the ledger stays complete; their payload
// rewrites are gone because migration 69 removes the whole policy.
const (
	naturalTeamCoordinationMigrationVersion  int64 = 24
	teamCoordinationDefaultsMigrationVersion int64 = 25
	teamRoutingMigrationVersion              int64 = 69
)

func (s *PostgresStore) migrateTeamRouting(ctx context.Context, tx *sql.Tx) error {
	for _, path := range teamCoordinationPaths {
		if _, err := tx.ExecContext(ctx, `UPDATE `+s.table(path.table)+`
			SET payload = payload #- '`+path.postgres+`'
			WHERE payload #> '`+path.postgres+`' IS NOT NULL`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES
		($1, 'single natural Team coordination model'),
		($2, 'explicit natural Team coordination defaults'),
		($3, 'Team channel routing replaces coordination policy')
		ON CONFLICT (version) DO NOTHING`,
		naturalTeamCoordinationMigrationVersion, teamCoordinationDefaultsMigrationVersion, teamRoutingMigrationVersion)
	return err
}

// rollbackTeamRouting removes the routing fields an older kernel does not know.
func (s *PostgresStore) rollbackTeamRouting(ctx context.Context, tx *sql.Tx) error {
	for _, path := range teamCoordinationPaths {
		routing := path.postgres[:len(path.postgres)-len("coordination}")] + "routing}"
		if _, err := tx.ExecContext(ctx, `UPDATE `+s.table(path.table)+`
			SET payload = payload #- '`+routing+`'
			WHERE payload #> '`+routing+`' IS NOT NULL`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE `+s.table("team_deployments")+`
		SET payload = jsonb_set(payload, '{roster}', (
			SELECT jsonb_agg(member - 'owns' ORDER BY position)
			FROM jsonb_array_elements(payload->'roster') WITH ORDINALITY AS roster(member, position)))
		WHERE jsonb_typeof(payload->'roster') = 'array' AND jsonb_array_length(payload->'roster') > 0`)
	return err
}
