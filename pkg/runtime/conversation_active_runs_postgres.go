package runtime

import (
	"context"
	"database/sql"
)

func (s *PostgresStore) postgresConversationActiveRunProjection() string {
	return `SELECT child.scope_kind, child.scope_id, child.id, child.payload->'owner'->>'type', child.payload->'owner'->>'id',
		root.payload->'context'->>'conversationId', child.root_run_id, child.created_at
		FROM ` + s.table("agent_runs") + ` child JOIN ` + s.table("agent_runs") + ` root
		ON root.scope_kind=child.scope_kind AND root.scope_id=child.scope_id AND root.id=child.root_run_id
		WHERE child.id<>root.id AND root.payload->>'kind'='conversation'
		AND jsonb_typeof(root.payload->'context'->'conversationId')='string' AND root.payload->'context'->>'conversationId'<>''
		AND child.payload->'owner'->>'type'=root.payload->'owner'->>'type' AND child.payload->'owner'->>'id'=root.payload->'owner'->>'id'
		AND COALESCE(child.payload->>'kind','agent_work')<>'conversation' AND child.status NOT IN ('completed','failed','canceled')`
}

func (s *PostgresStore) migrateConversationActiveRuns(ctx context.Context, tx *sql.Tx) error {
	insert := `INSERT INTO ` + s.table("conversation_active_runs") + ` ` + conversationActiveRunProjectionColumns + ` ` + s.postgresConversationActiveRunProjection()
	upsert := ` ON CONFLICT (scope_kind,scope_id,id) DO UPDATE SET owner_type=EXCLUDED.owner_type,owner_id=EXCLUDED.owner_id,
		conversation_id=EXCLUDED.conversation_id,root_run_id=EXCLUDED.root_run_id,created_at=EXCLUDED.created_at`
	identityChanged := `NEW.scope_kind IS DISTINCT FROM OLD.scope_kind OR NEW.scope_id IS DISTINCT FROM OLD.scope_id OR NEW.id IS DISTINCT FROM OLD.id
		OR NEW.payload->>'kind' IS DISTINCT FROM OLD.payload->>'kind'
		OR NEW.payload->'owner' IS DISTINCT FROM OLD.payload->'owner'`
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("conversation_active_runs")+` (
		scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,id TEXT NOT NULL,
		owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,conversation_id TEXT NOT NULL,
		root_run_id TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(scope_kind,scope_id,id)
	);
	CREATE INDEX IF NOT EXISTS conversation_active_runs_page_idx ON `+s.table("conversation_active_runs")+`
		(scope_kind,scope_id,owner_type,owner_id,conversation_id,created_at DESC,id DESC);
	CREATE INDEX IF NOT EXISTS conversation_active_runs_root_idx ON `+s.table("conversation_active_runs")+` (scope_kind,scope_id,root_run_id);
	CREATE OR REPLACE FUNCTION `+s.table("project_conversation_active_runs")+`() RETURNS TRIGGER AS $$
	BEGIN
		IF TG_OP = 'DELETE' THEN
			DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND id=OLD.id;
			DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND root_run_id=OLD.id;
			RETURN OLD;
		END IF;
		IF TG_OP = 'INSERT' OR `+identityChanged+` OR NEW.root_run_id IS DISTINCT FROM OLD.root_run_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
			OR (NEW.status IN ('completed','failed','canceled')) IS DISTINCT FROM (OLD.status IN ('completed','failed','canceled')) THEN
			IF TG_OP = 'UPDATE' THEN
				DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND id=OLD.id;
			END IF;
			DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND id=NEW.id;
			`+insert+` AND child.scope_kind=NEW.scope_kind AND child.scope_id=NEW.scope_id AND child.id=NEW.id`+upsert+`;
		END IF;
		IF TG_OP = 'INSERT' OR `+identityChanged+` OR NEW.payload->'context'->'conversationId' IS DISTINCT FROM OLD.payload->'context'->'conversationId' THEN
			IF TG_OP = 'UPDATE' THEN
				DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND root_run_id=OLD.id;
			END IF;
			DELETE FROM `+s.table("conversation_active_runs")+` WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND root_run_id=NEW.id;
			`+insert+` AND child.scope_kind=NEW.scope_kind AND child.scope_id=NEW.scope_id AND child.root_run_id=NEW.id`+upsert+`;
		END IF;
		RETURN NEW;
	END;
	$$ LANGUAGE plpgsql;
	DROP TRIGGER IF EXISTS project_conversation_active_runs ON `+s.table("agent_runs")+`;
	CREATE TRIGGER project_conversation_active_runs AFTER INSERT OR UPDATE OR DELETE ON `+s.table("agent_runs")+`
		FOR EACH ROW EXECUTE FUNCTION `+s.table("project_conversation_active_runs")+`();
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, insert+upsert)
	return err
}

func (s *PostgresStore) ListConversationActiveRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit int) ([]*AgentRun, error) {
	if err := validateConversationActiveRunsQuery(scope, owner, conversationID, limit); err != nil {
		return nil, err
	}
	// Materializing the page before canonical point reads prevents the planner
	// from beginning with historical conversation roots. OFFSET 0 preserves
	// each lateral lookup as an exact primary-key probe.
	rows, err := s.db.QueryContext(ctx, `WITH linked AS MATERIALIZED (
		SELECT * FROM `+s.table("conversation_active_runs")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND owner_type=$3 AND owner_id=$4 AND conversation_id=$5
		ORDER BY created_at DESC,id DESC LIMIT $6
	)
	SELECT child.payload FROM linked
		CROSS JOIN LATERAL (SELECT payload,id,root_run_id,status FROM `+s.table("agent_runs")+`
			WHERE scope_kind=linked.scope_kind AND scope_id=linked.scope_id AND id=linked.id OFFSET 0) child
		CROSS JOIN LATERAL (SELECT payload,id FROM `+s.table("agent_runs")+`
			WHERE scope_kind=linked.scope_kind AND scope_id=linked.scope_id AND id=linked.root_run_id OFFSET 0) root
		WHERE child.root_run_id=root.id AND child.id<>root.id AND root.payload->>'kind'='conversation'
			AND root.payload->'owner'->>'type'=linked.owner_type AND root.payload->'owner'->>'id'=linked.owner_id
			AND root.payload->'context'->>'conversationId'=linked.conversation_id
			AND child.payload->'owner'->>'type'=linked.owner_type AND child.payload->'owner'->>'id'=linked.owner_id
			AND COALESCE(child.payload->>'kind', 'agent_work') <> 'conversation'
			AND child.status NOT IN ('completed', 'failed', 'canceled')
		ORDER BY linked.created_at DESC, linked.id DESC LIMIT $6`, scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationActiveRunsSQL(rows)
}

func scanConversationActiveRunsSQL(rows *sql.Rows) ([]*AgentRun, error) {
	runs := make([]*AgentRun, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
