package runtime

import (
	"context"
	"database/sql"
)

const conversationActiveRunProjectionColumns = `(scope_kind,scope_id,id,owner_type,owner_id,conversation_id,root_run_id,created_at)`

const sqliteConversationActiveRunProjection = `SELECT child.scope_kind, child.scope_id, child.id,
	json_extract(child.payload, '$.owner.type'), json_extract(child.payload, '$.owner.id'),
	json_extract(root.payload, '$.context.conversationId'), child.root_run_id, child.created_at
	FROM agent_runs child JOIN agent_runs root
	ON root.scope_kind = child.scope_kind AND root.scope_id = child.scope_id AND root.id = child.root_run_id
	WHERE child.id <> root.id AND json_extract(root.payload, '$.kind') = 'conversation'
	AND json_type(root.payload, '$.context.conversationId') = 'text'
	AND json_extract(root.payload, '$.context.conversationId') <> ''
	AND json_extract(child.payload, '$.owner.type') = json_extract(root.payload, '$.owner.type')
	AND json_extract(child.payload, '$.owner.id') = json_extract(root.payload, '$.owner.id')
	AND COALESCE(json_extract(child.payload, '$.kind'), 'agent_work') <> 'conversation'
	AND child.status NOT IN ('completed', 'failed', 'canceled')`

func migrateConversationActiveRunsSQLite(db *sql.DB) error {
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='conversation_active_runs')`).Scan(&exists); err != nil {
		return err
	}
	insert := `INSERT OR REPLACE INTO conversation_active_runs ` + conversationActiveRunProjectionColumns + ` ` + sqliteConversationActiveRunProjection
	refreshChild := `
		DELETE FROM conversation_active_runs WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND id=NEW.id;
		` + insert + ` AND child.scope_kind=NEW.scope_kind AND child.scope_id=NEW.scope_id AND child.id=NEW.id;`
	refreshRoot := `
		DELETE FROM conversation_active_runs WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND root_run_id=NEW.id;
		` + insert + ` AND child.scope_kind=NEW.scope_kind AND child.scope_id=NEW.scope_id AND child.root_run_id=NEW.id;`
	identityChanged := `NEW.scope_kind IS NOT OLD.scope_kind OR NEW.scope_id IS NOT OLD.scope_id OR NEW.id IS NOT OLD.id
		OR json_extract(NEW.payload,'$.kind') IS NOT json_extract(OLD.payload,'$.kind')
		OR json_extract(NEW.payload,'$.owner.type') IS NOT json_extract(OLD.payload,'$.owner.type')
		OR json_extract(NEW.payload,'$.owner.id') IS NOT json_extract(OLD.payload,'$.owner.id')`
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversation_active_runs (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			root_run_id TEXT NOT NULL, created_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id)
		);
		CREATE INDEX IF NOT EXISTS idx_conversation_active_runs_page
			ON conversation_active_runs(scope_kind,scope_id,owner_type,owner_id,conversation_id,created_at DESC,id DESC);
		CREATE INDEX IF NOT EXISTS idx_conversation_active_runs_root
			ON conversation_active_runs(scope_kind,scope_id,root_run_id);
		CREATE TRIGGER IF NOT EXISTS project_conversation_active_runs_insert AFTER INSERT ON agent_runs BEGIN ` + refreshChild + refreshRoot + ` END;
		DROP TRIGGER IF EXISTS project_conversation_active_runs_update;
		CREATE TRIGGER IF NOT EXISTS project_conversation_active_runs_update_child AFTER UPDATE ON agent_runs
		WHEN ` + identityChanged + ` OR NEW.root_run_id IS NOT OLD.root_run_id OR NEW.created_at IS NOT OLD.created_at
			OR (NEW.status IN ('completed','failed','canceled')) <> (OLD.status IN ('completed','failed','canceled'))
		BEGIN
			DELETE FROM conversation_active_runs WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND id=OLD.id;
			` + refreshChild + ` END;
		CREATE TRIGGER IF NOT EXISTS project_conversation_active_runs_update_root AFTER UPDATE ON agent_runs
		WHEN ` + identityChanged + ` OR json_extract(NEW.payload,'$.context.conversationId') IS NOT json_extract(OLD.payload,'$.context.conversationId')
			OR json_type(NEW.payload,'$.context.conversationId') IS NOT json_type(OLD.payload,'$.context.conversationId')
		BEGIN
			DELETE FROM conversation_active_runs WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND root_run_id=OLD.id;
			` + refreshRoot + ` END;
		CREATE TRIGGER IF NOT EXISTS project_conversation_active_runs_delete AFTER DELETE ON agent_runs BEGIN
			DELETE FROM conversation_active_runs WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND id=OLD.id;
			DELETE FROM conversation_active_runs WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND root_run_id=OLD.id;
		END;
	`)
	if err != nil || exists {
		return err
	}
	_, err = db.Exec(insert)
	return err
}

func (s *SQLiteStore) ListConversationActiveRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit int) ([]*AgentRun, error) {
	if err := validateConversationActiveRunsQuery(scope, owner, conversationID, limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT child.payload FROM conversation_active_runs linked
		JOIN agent_runs child INDEXED BY sqlite_autoindex_agent_runs_1 ON child.scope_kind=linked.scope_kind AND child.scope_id=linked.scope_id AND child.id=linked.id
		JOIN agent_runs root ON root.scope_kind=linked.scope_kind AND root.scope_id=linked.scope_id AND root.id=linked.root_run_id
		WHERE linked.scope_kind = ? AND linked.scope_id = ? AND linked.owner_type = ? AND linked.owner_id = ? AND linked.conversation_id = ?
			AND child.root_run_id=root.id AND child.id<>root.id
			AND json_extract(root.payload, '$.owner.type')=linked.owner_type AND json_extract(root.payload, '$.owner.id')=linked.owner_id
			AND json_extract(root.payload, '$.context.conversationId')=linked.conversation_id
			AND json_extract(root.payload, '$.kind') = 'conversation'
			AND json_extract(child.payload, '$.owner.type')=linked.owner_type AND json_extract(child.payload, '$.owner.id')=linked.owner_id
			AND COALESCE(json_extract(child.payload, '$.kind'), 'agent_work') <> 'conversation'
			AND child.status NOT IN ('completed', 'failed', 'canceled')
		ORDER BY linked.created_at DESC, linked.id DESC LIMIT ?`, scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationActiveRunsSQL(rows)
}
