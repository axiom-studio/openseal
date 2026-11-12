package postgres

import (
	"context"
	"database/sql"
)

// agentBuilderDraftMigrationVersion adds conversations.builder_draft_requester:
// the person who started a builder conversation, until it produces its Agent
// or Team. The drafts list selects on it instead of reading every builder
// conversation of the scope.
const agentBuilderDraftMigrationVersion int64 = 82

// migrateAgentBuilderDrafts is PostgreSQL migration 82. It fills the column
// for the builder conversations written before it by the rule the drafts
// list applied on read: a person sent the first message, and no approved
// create card of the conversation has a succeeded call.
func (s *PostgresStore) migrateAgentBuilderDrafts(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, agentBuilderDraftMigrationVersion)
	if err != nil || applied {
		return err
	}
	conversations := s.table("conversations")
	statements := []string{
		`ALTER TABLE ` + conversations + ` ADD COLUMN IF NOT EXISTS builder_draft_requester TEXT`,
		`WITH created AS (
			SELECT a.scope_kind, a.scope_id,
				COALESCE(NULLIF(r.payload->'context'->>'conversationId', ''), root.payload->'context'->>'conversationId', '') AS conversation_id
			FROM ` + s.table("approval_checkpoints") + ` a
			JOIN ` + s.table("action_calls") + ` k ON k.scope_kind = a.scope_kind AND k.scope_id = a.scope_id AND k.id = a.action_call_id
			JOIN ` + s.table("agent_runs") + ` r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = a.run_id
			LEFT JOIN ` + s.table("agent_runs") + ` root ON root.scope_kind = r.scope_kind AND root.scope_id = r.scope_id AND root.id = r.root_run_id
			WHERE a.status = 'approved' AND k.status = 'succeeded' AND (
				a.payload->'proposedAction'->>'resourceType' = 'agent_profile' AND a.payload->'proposedAction'->>'operation' = 'create_agent'
				OR a.payload->'proposedAction'->>'resourceType' = 'team' AND a.payload->'proposedAction'->>'operation' = 'create_team')
		)
		UPDATE ` + conversations + ` c SET builder_draft_requester = m.sender_id
		FROM ` + s.table("channel_messages") + ` m
		WHERE m.scope_kind = c.scope_kind AND m.scope_id = c.scope_id AND m.conversation_id = c.id
			AND m.sequence = 1 AND m.sender_type = 'user' AND c.payload->'origin'->>'kind' = 'agent_builder'
			AND NOT EXISTS (SELECT 1 FROM created d
				WHERE d.scope_kind = c.scope_kind AND d.scope_id = c.scope_id AND d.conversation_id = c.id)`,
		`CREATE INDEX IF NOT EXISTS conversations_builder_draft_idx ON ` + conversations + ` (scope_kind, scope_id, builder_draft_requester, updated_at DESC, id)
			WHERE builder_draft_requester IS NOT NULL`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'agent builder draft requester') ON CONFLICT (version) DO NOTHING`, agentBuilderDraftMigrationVersion)
	return err
}
