package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

const sqliteConversationTaskDuePredicate = `json_extract(payload,'$.kind')='conversation'
	AND json_extract(payload,'$.source')='chat' AND parent_run_id='' AND root_run_id=id
	AND status NOT IN ('completed','failed','canceled')
	AND json_extract(payload,'$.context."openseal.conversationTaskId"') IS NULL`

func migrateConversationTaskContinuationsSQLite(db *sql.DB) error {
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_runs_conversation_task_due
		ON agent_runs(scope_kind,scope_id,created_at,id) WHERE ` + sqliteConversationTaskDuePredicate)
	return err
}

func (s *SQLiteStore) ListDueConversationTaskRuns(ctx context.Context, filter models.ConversationTaskDueFilter) ([]*models.AgentRun, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM agent_runs WHERE scope_kind=? AND scope_id=? AND ` + sqliteConversationTaskDuePredicate + ` AND created_at<=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
	if filter.AfterCreatedAt != nil {
		query += ` AND (created_at,id)>(?,?)`
		args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID)
	}
	query += ` ORDER BY created_at,id LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]*models.AgentRun, 0, filter.Limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		run, err := sqlstore.DecodeAgentRun(payload)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *SQLiteStore) PromoteConversationTask(ctx context.Context, req models.ConversationTaskPromotionRequest) (*models.ConversationTaskResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	run, err := getSQLiteAgentRun(ctx, conn, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, models.ErrRunNotFound
	}
	existing, err := getSQLiteConversationTaskByWorkRun(ctx, conn, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return models.ReplayConversationTaskContinuation(existing, run)
	}
	if !models.ConversationTaskContinuationCandidate(run) {
		return nil, nil
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	messageID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := getSQLiteConversation(ctx, conn, req.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	message, err := getSQLiteChannelMessage(ctx, conn, req.Scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	for _, responseKey := range models.ConversationTaskFinalResponseKeys(run) {
		final, err := getSQLiteChannelMessageByKey(ctx, conn, req.Scope, conversationID, responseKey)
		if err != nil {
			return nil, err
		}
		if final != nil {
			return nil, nil
		}
	}
	var turnPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_turns WHERE scope_kind=? AND scope_id=? AND run_id=? ORDER BY sequence DESC LIMIT 1`, req.Scope.Kind, req.Scope.ID, req.RunID).Scan(&turnPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		step, err := sqlstore.DecodeAgentTurnRecord(turnPayload)
		if err != nil {
			return nil, err
		}
		if models.ConversationTaskFinishedBeforePromotion(step) {
			return nil, nil
		}
	}
	var chat *models.AgentSession
	if run.SessionID != "" {
		chat, err = getSQLiteAgentSession(ctx, conn, req.Scope, run.SessionID)
		if errors.Is(err, models.ErrAgentSessionNotFound) {
			chat, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	task, updated, event, fork, err := models.BuildConversationTaskContinuation(req, run, conversation, message, chat)
	if err != nil || task == nil {
		return nil, err
	}
	if task.ThreadRootID != task.SourceMessageID {
		root, err := getSQLiteChannelMessage(ctx, conn, req.Scope, conversationID, task.ThreadRootID)
		if err != nil {
			return nil, err
		}
		if root == nil || root.ConversationID != conversationID {
			return nil, models.ErrInvalidConversationTask
		}
	}
	ack, err := models.BuildConversationTaskAcknowledgment(task, conversation, message)
	if err != nil {
		return nil, err
	}
	if err := updateSQLiteAgentRunConn(ctx, conn, updated, run.Revision); err != nil {
		return nil, err
	}
	// updated already carries SessionID = child, so the fork leaves the Run row alone.
	if fork != nil {
		if _, err := s.createRunAgentSessionTx(ctx, conn, fork.Child); err != nil {
			return nil, err
		}
		if fork.Note != nil {
			if _, err := s.appendAgentSessionEntriesTx(ctx, conn, *fork.Note); err != nil {
				return nil, err
			}
		}
	}
	if err := insertSQLiteConversationTask(ctx, conn, task, updated.Status); err != nil {
		return nil, normalizeSQLiteConversationTaskConflict(err)
	}
	if _, err := insertSQLiteActivityConn(ctx, conn, event); err != nil {
		return nil, err
	}
	if err := updateSQLiteConversation(ctx, conn, ack.Conversation, ack.ExpectedRevision); err != nil {
		return nil, err
	}
	if err := insertSQLiteChannelMessage(ctx, conn, ack.Message); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &models.ConversationTaskResult{Task: models.CloneConversationTask(task), WorkRun: models.CloneAgentRun(updated), SourceRun: models.CloneAgentRun(updated), AcknowledgmentMessageID: ack.Message.ID}, nil
}

func getSQLiteConversationTaskByWorkRun(ctx context.Context, queryer sqliteDependencyQueryer, scope models.Scope, runID string) (*models.ConversationTask, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks WHERE scope_kind=? AND scope_id=? AND work_run_id=?`, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversationTask(payload)
}
