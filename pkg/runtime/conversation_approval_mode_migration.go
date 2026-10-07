package runtime

import (
	"context"
	"database/sql"
)

const conversationApprovalModeMigrationVersion int64 = 61

// migrateConversationApprovalMode makes every stored conversation's approval
// mode explicit. Conversations created before the mode existed get the
// default, auto.
func (s *PostgresStore) migrateConversationApprovalMode(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, conversationApprovalModeMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("conversations")+`
		SET payload = jsonb_set(payload, '{approvalMode}', to_jsonb($1::text), true)
		WHERE COALESCE(payload->>'approvalMode', '') = ''`, string(DefaultConversationApprovalMode)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version,name) VALUES ($1,'conversation approval mode') ON CONFLICT (version) DO NOTHING`, conversationApprovalModeMigrationVersion)
	return err
}

func (s *PostgresStore) rollbackConversationApprovalMode(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+s.table("conversations")+` SET payload = payload - 'approvalMode' WHERE payload ? 'approvalMode'`)
	return err
}

func migrateSQLiteConversationApprovalMode(db *sql.DB) error {
	_, err := db.Exec(`UPDATE conversations SET payload = json_set(payload, '$.approvalMode', ?)
		WHERE COALESCE(json_extract(payload, '$.approvalMode'), '') = ''`, string(DefaultConversationApprovalMode))
	return err
}
