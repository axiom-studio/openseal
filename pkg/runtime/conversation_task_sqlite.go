package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func migrateConversationTasksSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversation_tasks (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			task_key TEXT NOT NULL,
			request_digest TEXT NOT NULL,
			owner_type TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			thread_root_id TEXT NOT NULL DEFAULT '',
			work_run_id TEXT NOT NULL,
			work_status TEXT NOT NULL,
			is_active INTEGER NOT NULL CHECK (is_active IN (0, 1)),
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_conversation_tasks_request
			ON conversation_tasks(scope_kind, scope_id, source_run_id, task_key);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_conversation_tasks_work_run
			ON conversation_tasks(scope_kind, scope_id, work_run_id);
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_conversation_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, created_at, id);
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_thread_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, thread_root_id, created_at, id);
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_actor_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, created_at, id);
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_actor_thread_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, thread_root_id, created_at, id);
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_active_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, created_at, id) WHERE is_active = 1;
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_active_thread_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, thread_root_id, created_at, id) WHERE is_active = 1;
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_active_actor_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, created_at, id) WHERE is_active = 1;
		CREATE INDEX IF NOT EXISTS idx_conversation_tasks_active_actor_thread_page
			ON conversation_tasks(scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, thread_root_id, created_at, id) WHERE is_active = 1;
		CREATE INDEX IF NOT EXISTS idx_agent_runs_conversation_roots
			ON agent_runs(scope_kind, scope_id, json_extract(payload, '$.owner.type'), json_extract(payload, '$.owner.id'), json_extract(payload, '$.context.conversationId'), id)
			WHERE json_extract(payload, '$.kind') = 'conversation';
		CREATE INDEX IF NOT EXISTS idx_agent_runs_conversation_active_children
			ON agent_runs(scope_kind, scope_id, root_run_id, json_extract(payload, '$.owner.type'), json_extract(payload, '$.owner.id'), created_at DESC, id DESC)
			WHERE COALESCE(json_extract(payload, '$.kind'), 'agent_work') <> 'conversation' AND status NOT IN ('completed', 'failed', 'canceled');
		CREATE INDEX IF NOT EXISTS idx_agent_runs_conversation_foreground_key
			ON agent_runs(scope_kind,scope_id,json_extract(payload,'$.owner.type'),json_extract(payload,'$.owner.id'),json_extract(payload,'$.concurrencyKey'),created_at DESC,id DESC)
			WHERE json_extract(payload,'$.kind')='conversation';
		CREATE INDEX IF NOT EXISTS idx_agent_runs_conversation_foreground_thread
			ON agent_runs(scope_kind,scope_id,json_extract(payload,'$.owner.type'),json_extract(payload,'$.owner.id'),json_extract(payload,'$.context.conversationId'),created_at DESC,id DESC)
			WHERE json_extract(payload,'$.kind')='conversation' AND json_type(payload,'$.context.threadRootMessageId')='text'
			AND json_extract(payload,'$.context.threadRootMessageId')<>''
			AND json_extract(payload,'$.concurrencyKey')=json_extract(payload,'$.context.conversationId')||':thread:'||json_extract(payload,'$.context.threadRootMessageId');
		CREATE TRIGGER IF NOT EXISTS project_conversation_task_status
		AFTER UPDATE OF status ON agent_runs
		WHEN NEW.status <> OLD.status
		BEGIN
			UPDATE conversation_tasks SET work_status = NEW.status,
				is_active = CASE WHEN NEW.status IN ('completed', 'failed', 'canceled') THEN 0 ELSE 1 END
			WHERE scope_kind = NEW.scope_kind AND scope_id = NEW.scope_id AND work_run_id = NEW.id;
		END;
	`)
	if err != nil {
		return err
	}
	if err := migrateConversationActiveRunsSQLite(db); err != nil {
		return err
	}
	return migrateConversationTaskContinuationsSQLite(db)
}

func (s *SQLiteStore) CreateConversationTask(ctx context.Context, record ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	if err := validateConversationTaskCreateRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	existing, err := getSQLiteConversationTask(ctx, conn, record.Task.Scope, record.Task.ID, "", "")
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing, err = getSQLiteConversationTask(ctx, conn, record.Task.Scope, "", record.Task.SourceRunID, record.Task.TaskKey)
		if err != nil {
			return nil, err
		}
	}
	if existing != nil {
		if !sameConversationTask(existing, record.Task) {
			return nil, ErrConversationTaskConflict
		}
		result, err := sqliteConversationTaskResult(ctx, conn, existing)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		result.Replayed = true
		return result, nil
	}
	current, err := getSQLiteAgentRun(ctx, conn, record.Task.Scope, record.Task.SourceRunID)
	if err != nil {
		return nil, err
	}
	continuation, err := getSQLiteConversationTaskByWorkRun(ctx, conn, record.Task.Scope, record.Task.SourceRunID)
	if err != nil {
		return nil, err
	}
	if continuation != nil && continuation.Mode == ConversationTaskModeContinuation {
		return replayConversationTaskContinuation(continuation, current)
	}
	if err := validateConversationTaskSourceUpdate(current, record); err != nil {
		return nil, err
	}
	if err := updateSQLiteAgentRunConn(ctx, conn, record.SourceRun, record.ExpectedSourceRevision); err != nil {
		return nil, err
	}
	if err := insertSQLiteAgentRunConn(ctx, conn, record.WorkRun); err != nil {
		return nil, normalizeSQLiteConversationTaskConflict(err)
	}
	payload, err := json.Marshal(record.Task)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO conversation_tasks
		(scope_kind, scope_id, id, source_run_id, task_key, request_digest, owner_type, owner_id, actor_type, actor_id,
		 conversation_id, thread_root_id, work_run_id, work_status, is_active, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Task.Scope.Kind, record.Task.Scope.ID,
		record.Task.ID, record.Task.SourceRunID, record.Task.TaskKey, record.Task.RequestDigest,
		record.Task.Owner.Type, record.Task.Owner.ID, record.Task.AuthenticatedActor.Type, record.Task.AuthenticatedActor.ID,
		record.Task.ConversationID, record.Task.ThreadRootID,
		record.Task.WorkRunID, record.WorkRun.Status, 1, record.Task.CreatedAt.UTC(), string(payload))
	if err != nil {
		return nil, normalizeSQLiteConversationTaskConflict(err)
	}
	if _, err := insertSQLiteActivityConn(ctx, conn, record.Event); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &ConversationTaskResult{Task: cloneConversationTask(record.Task), WorkRun: cloneAgentRun(record.WorkRun), SourceRun: cloneAgentRun(record.SourceRun)}, nil
}

func (s *SQLiteStore) GetConversationTask(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	return getSQLiteConversationTask(ctx, s.db, scope, strings.TrimSpace(id), "", "")
}

func (s *SQLiteStore) FindConversationTaskByWorkRunID(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks
		WHERE scope_kind = ? AND scope_id = ? AND work_run_id = ?`, scope.Kind, scope.ID, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationTaskSQL(payload)
}

func (s *SQLiteStore) ListConversationTasks(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT t.payload, r.payload FROM conversation_tasks t
		JOIN agent_runs r ON r.scope_kind = t.scope_kind AND r.scope_id = t.scope_id AND r.id = t.work_run_id
		WHERE t.scope_kind = ? AND t.scope_id = ? AND t.conversation_id = ? AND t.owner_type = ? AND t.owner_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.Owner.Type, filter.Owner.ID}
	if filter.AuthenticatedActor.Type != "" {
		query += ` AND t.actor_type = ? AND t.actor_id = ?`
		args = append(args, filter.AuthenticatedActor.Type, filter.AuthenticatedActor.ID)
	}
	if filter.ThreadRootID != "" {
		query += ` AND t.thread_root_id = ?`
		args = append(args, filter.ThreadRootID)
	}
	if filter.ActiveOnly {
		query += ` AND t.is_active = 1`
	}
	if filter.BeforeCreatedAt != nil {
		query += ` AND (t.created_at < ? OR (t.created_at = ? AND t.id < ?))`
		args = append(args, filter.BeforeCreatedAt.UTC(), filter.BeforeCreatedAt.UTC(), filter.BeforeID)
	}
	query += ` ORDER BY t.created_at DESC, t.id DESC LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationTaskResults(rows)
}

func getSQLiteConversationTask(ctx context.Context, queryer sqliteDependencyQueryer, scope Scope, id, sourceRunID, taskKey string) (*ConversationTask, error) {
	query := `SELECT payload FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{scope.Kind, scope.ID}
	if id != "" {
		query += ` AND id = ?`
		args = append(args, id)
	} else {
		query += ` AND source_run_id = ? AND task_key = ?`
		args = append(args, sourceRunID, taskKey)
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationTaskSQL(payload)
}

func sqliteConversationTaskResult(ctx context.Context, queryer sqliteDependencyQueryer, task *ConversationTask) (*ConversationTaskResult, error) {
	work, err := getSQLiteAgentRun(ctx, queryer, task.Scope, task.WorkRunID)
	if err != nil {
		return nil, err
	}
	source, err := getSQLiteAgentRun(ctx, queryer, task.Scope, task.SourceRunID)
	if err != nil {
		return nil, err
	}
	return &ConversationTaskResult{Task: task, WorkRun: work, SourceRun: source}, nil
}

func normalizeSQLiteConversationTaskConflict(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrConversationTaskConflict
	}
	return err
}

func decodeConversationTaskSQL(payload string) (*ConversationTask, error) {
	var task ConversationTask
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return nil, fmt.Errorf("decode conversation task: %w", err)
	}
	return &task, nil
}

func scanConversationTaskResults(rows *sql.Rows) ([]*ConversationTaskResult, error) {
	results := make([]*ConversationTaskResult, 0)
	for rows.Next() {
		var taskPayload, runPayload string
		if err := rows.Scan(&taskPayload, &runPayload); err != nil {
			return nil, err
		}
		task, err := decodeConversationTaskSQL(taskPayload)
		if err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(runPayload)
		if err != nil {
			return nil, err
		}
		results = append(results, &ConversationTaskResult{Task: task, WorkRun: run})
	}
	return results, rows.Err()
}
