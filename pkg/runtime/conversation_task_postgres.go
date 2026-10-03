package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

const conversationTaskMigrationVersion int64 = 57

func (s *PostgresStore) migrateConversationTasks(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, conversationTaskMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("conversation_tasks")+` (
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
		is_active BOOLEAN NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		payload JSONB NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id)
	)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS conversation_tasks_request_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, source_run_id, task_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS conversation_tasks_work_run_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, work_run_id)`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_conversation_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_thread_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, thread_root_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_actor_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_actor_thread_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, thread_root_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_active_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, created_at, id) WHERE is_active`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_active_thread_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, thread_root_id, created_at, id) WHERE is_active`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_active_actor_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, created_at, id) WHERE is_active`,
		`CREATE INDEX IF NOT EXISTS conversation_tasks_active_actor_thread_page_idx ON ` + s.table("conversation_tasks") + ` (scope_kind, scope_id, conversation_id, owner_type, owner_id, actor_type, actor_id, thread_root_id, created_at, id) WHERE is_active`,
		`CREATE INDEX IF NOT EXISTS agent_runs_conversation_roots_idx ON ` + s.table("agent_runs") + ` (scope_kind, scope_id, (payload->'owner'->>'type'), (payload->'owner'->>'id'), (payload->'context'->>'conversationId'), id) WHERE payload->>'kind' = 'conversation'`,
		`CREATE INDEX IF NOT EXISTS agent_runs_conversation_active_children_idx ON ` + s.table("agent_runs") + ` (scope_kind, scope_id, root_run_id, (payload->'owner'->>'type'), (payload->'owner'->>'id'), created_at DESC, id DESC) WHERE COALESCE(payload->>'kind', 'agent_work') <> 'conversation' AND status NOT IN ('completed', 'failed', 'canceled')`,
		`CREATE INDEX IF NOT EXISTS agent_runs_conversation_foreground_key_idx ON ` + s.table("agent_runs") + ` (scope_kind,scope_id,(payload->'owner'->>'type'),(payload->'owner'->>'id'),(payload->>'concurrencyKey'),created_at DESC,id DESC) WHERE payload->>'kind'='conversation'`,
		`CREATE INDEX IF NOT EXISTS agent_runs_conversation_foreground_thread_idx ON ` + s.table("agent_runs") + ` (scope_kind,scope_id,(payload->'owner'->>'type'),(payload->'owner'->>'id'),(payload->'context'->>'conversationId'),created_at DESC,id DESC) WHERE payload->>'kind'='conversation' AND jsonb_typeof(payload->'context'->'threadRootMessageId')='string' AND payload->'context'->>'threadRootMessageId'<>'' AND payload->>'concurrencyKey'=(payload->'context'->>'conversationId')||':thread:'||(payload->'context'->>'threadRootMessageId')`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION `+s.table("project_conversation_task_status")+`() RETURNS TRIGGER AS $$
		BEGIN
			UPDATE `+s.table("conversation_tasks")+` SET work_status = NEW.status,
				is_active = NEW.status NOT IN ('completed', 'failed', 'canceled')
			WHERE scope_kind = NEW.scope_kind AND scope_id = NEW.scope_id AND work_run_id = NEW.id;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS project_conversation_task_status ON `+s.table("agent_runs")+`;
		CREATE TRIGGER project_conversation_task_status AFTER UPDATE OF status ON `+s.table("agent_runs")+`
		FOR EACH ROW WHEN (NEW.status IS DISTINCT FROM OLD.status)
		EXECUTE FUNCTION `+s.table("project_conversation_task_status")+`()`); err != nil {
		return err
	}
	if err := s.migrateConversationActiveRuns(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name)
		VALUES ($1, 'durable conversation tasks') ON CONFLICT (version) DO NOTHING`, conversationTaskMigrationVersion)
	return err
}

func (s *PostgresStore) CreateConversationTask(ctx context.Context, record ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	if err := validateConversationTaskCreateRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		"openseal:conversation-task:"+record.Task.Scope.key()+":"+record.Task.SourceRunID+":"+record.Task.TaskKey); err != nil {
		return nil, err
	}
	existing, err := s.getPostgresConversationTask(ctx, tx, record.Task.Scope, record.Task.ID, "", "", true)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing, err = s.getPostgresConversationTask(ctx, tx, record.Task.Scope, "", record.Task.SourceRunID, record.Task.TaskKey, true)
		if err != nil {
			return nil, err
		}
	}
	if existing != nil {
		if !sameConversationTask(existing, record.Task) {
			return nil, ErrConversationTaskConflict
		}
		result, err := s.postgresConversationTaskResult(ctx, tx, existing)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		result.Replayed = true
		return result, nil
	}
	current, err := s.getPostgresAgentRunTx(ctx, tx, record.Task.Scope, record.Task.SourceRunID, true)
	if err != nil {
		return nil, err
	}
	if err := validateConversationTaskSourceUpdate(current, record); err != nil {
		return nil, err
	}
	if err := s.updatePostgresAgentRunTx(ctx, tx, record.SourceRun, record.ExpectedSourceRevision); err != nil {
		return nil, err
	}
	if err := s.insertPostgresAgentRunTx(ctx, tx, record.WorkRun); err != nil {
		return nil, normalizePostgresConversationTaskConflict(err)
	}
	payload, err := json.Marshal(record.Task)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_tasks")+`
		(scope_kind, scope_id, id, source_run_id, task_key, request_digest, owner_type, owner_id, actor_type, actor_id,
		 conversation_id, thread_root_id, work_run_id, work_status, is_active, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::jsonb)`, record.Task.Scope.Kind,
		record.Task.Scope.ID, record.Task.ID, record.Task.SourceRunID, record.Task.TaskKey, record.Task.RequestDigest,
		record.Task.Owner.Type, record.Task.Owner.ID, record.Task.AuthenticatedActor.Type, record.Task.AuthenticatedActor.ID,
		record.Task.ConversationID, record.Task.ThreadRootID,
		record.Task.WorkRunID, record.WorkRun.Status, true, record.Task.CreatedAt.UTC(), string(payload))
	if err != nil {
		return nil, normalizePostgresConversationTaskConflict(err)
	}
	if _, err := s.insertPostgresActivityTx(ctx, tx, record.Event); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ConversationTaskResult{Task: cloneConversationTask(record.Task), WorkRun: cloneAgentRun(record.WorkRun), SourceRun: cloneAgentRun(record.SourceRun)}, nil
}

func (s *PostgresStore) GetConversationTask(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	return s.getPostgresConversationTask(ctx, s.db, scope, strings.TrimSpace(id), "", "", false)
}

func (s *PostgresStore) FindConversationTaskByWorkRunID(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("conversation_tasks")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND work_run_id = $3`, scope.Kind, scope.ID, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationTaskSQL(payload)
}

func (s *PostgresStore) ListConversationTasks(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT t.payload, r.payload FROM ` + s.table("conversation_tasks") + ` t
		JOIN ` + s.table("agent_runs") + ` r ON r.scope_kind = t.scope_kind AND r.scope_id = t.scope_id AND r.id = t.work_run_id
		WHERE t.scope_kind = $1 AND t.scope_id = $2 AND t.conversation_id = $3 AND t.owner_type = $4 AND t.owner_id = $5`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.Owner.Type, filter.Owner.ID}
	if filter.AuthenticatedActor.Type != "" {
		args = append(args, filter.AuthenticatedActor.Type, filter.AuthenticatedActor.ID)
		query += ` AND t.actor_type = $` + strconv.Itoa(len(args)-1) + ` AND t.actor_id = $` + strconv.Itoa(len(args))
	}
	if filter.ThreadRootID != "" {
		args = append(args, filter.ThreadRootID)
		query += ` AND t.thread_root_id = $` + strconv.Itoa(len(args))
	}
	if filter.ActiveOnly {
		query += ` AND t.is_active`
	}
	if filter.BeforeCreatedAt != nil {
		args = append(args, filter.BeforeCreatedAt.UTC(), filter.BeforeID)
		query += ` AND (t.created_at, t.id) < ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
	}
	args = append(args, filter.Limit)
	query += ` ORDER BY t.created_at DESC, t.id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationTaskResults(rows)
}

func (s *PostgresStore) getPostgresConversationTask(ctx context.Context, queryer postgresDependencyQueryer, scope Scope, id, sourceRunID, taskKey string, lock bool) (*ConversationTask, error) {
	query := `SELECT payload FROM ` + s.table("conversation_tasks") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{scope.Kind, scope.ID}
	if id != "" {
		query += ` AND id = $3`
		args = append(args, id)
	} else {
		query += ` AND source_run_id = $3 AND task_key = $4`
		args = append(args, sourceRunID, taskKey)
	}
	if lock {
		query += ` FOR UPDATE`
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

func (s *PostgresStore) postgresConversationTaskResult(ctx context.Context, queryer postgresDependencyQueryer, task *ConversationTask) (*ConversationTaskResult, error) {
	work, err := s.getPostgresAgentRunTx(ctx, queryer, task.Scope, task.WorkRunID, false)
	if err != nil {
		return nil, err
	}
	source, err := s.getPostgresAgentRunTx(ctx, queryer, task.Scope, task.SourceRunID, false)
	if err != nil {
		return nil, err
	}
	return &ConversationTaskResult{Task: task, WorkRun: work, SourceRun: source}, nil
}

func normalizePostgresConversationTaskConflict(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrConversationTaskConflict
	}
	return err
}
