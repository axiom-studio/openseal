package runtime

import (
	"database/sql"
)

// migrateAgentBuilderDraftsSQLite mirrors PostgreSQL migration 82. SQLite has
// no ledger: the column is added and filled only while it is missing, so a
// later open does nothing.
func migrateAgentBuilderDraftsSQLite(db *sql.DB) error {
	exists, err := sqliteColumnExists(db, "conversations", "builder_draft_requester")
	if err != nil || exists {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE conversations ADD COLUMN builder_draft_requester TEXT`,
		`UPDATE conversations SET builder_draft_requester = (
				SELECT m.sender_id FROM channel_messages m
				WHERE m.scope_kind = conversations.scope_kind AND m.scope_id = conversations.scope_id AND m.conversation_id = conversations.id
					AND m.sequence = 1 AND m.sender_type = 'user')
			WHERE json_extract(payload, '$.origin.kind') = 'agent_builder' AND NOT EXISTS (
				SELECT 1 FROM approval_checkpoints a
				JOIN action_calls k ON k.scope_kind = a.scope_kind AND k.scope_id = a.scope_id AND k.id = a.action_call_id
				JOIN agent_runs r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = a.run_id
				LEFT JOIN agent_runs root ON root.scope_kind = r.scope_kind AND root.scope_id = r.scope_id AND root.id = r.root_run_id
				WHERE a.scope_kind = conversations.scope_kind AND a.scope_id = conversations.scope_id
					AND a.status = 'approved' AND k.status = 'succeeded' AND (
						json_extract(a.payload, '$.proposedAction.resourceType') = 'agent_profile' AND json_extract(a.payload, '$.proposedAction.operation') = 'create_agent'
						OR json_extract(a.payload, '$.proposedAction.resourceType') = 'team' AND json_extract(a.payload, '$.proposedAction.operation') = 'create_team')
					AND COALESCE(NULLIF(json_extract(r.payload, '$.context.conversationId'), ''), json_extract(root.payload, '$.context.conversationId'), '') = conversations.id)`,
		`CREATE INDEX IF NOT EXISTS idx_conversations_builder_draft ON conversations (scope_kind, scope_id, builder_draft_requester, updated_at DESC, id)
			WHERE builder_draft_requester IS NOT NULL`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
