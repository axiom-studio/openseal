package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func (s *SQLiteStore) ListDueConversationTaskRuns(ctx context.Context, filter ConversationTaskDueFilter) ([]*AgentRun, error) {
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
	runs := make([]*AgentRun, 0, filter.Limit)
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

func (s *SQLiteStore) PromoteConversationTask(ctx context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error) {
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
		return nil, ErrRunNotFound
	}
	existing, err := getSQLiteConversationTaskByWorkRun(ctx, conn, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return replayConversationTaskContinuation(existing, run)
	}
	if !conversationTaskContinuationCandidate(run) {
		return nil, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	messageID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := getSQLiteConversation(ctx, conn, req.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	message, err := getSQLiteChannelMessage(ctx, conn, req.Scope, conversationID, messageID)
	if err != nil {
		return nil, err
	}
	for _, responseKey := range conversationTaskFinalResponseKeys(run) {
		final, err := getSQLiteChannelMessageByKey(ctx, conn, req.Scope, conversationID, responseKey)
		if err != nil {
			return nil, err
		}
		if final != nil {
			return nil, nil
		}
	}
	if run.Owner.Type == OwnerTypeTeam {
		round, err := getSQLiteParticipationRoundByKey(ctx, conn, req.Scope, conversationID, conversationTaskParticipationRoundKey(run))
		if err != nil {
			return nil, err
		}
		if conversationTaskFinishedTeamRound(run, round) {
			return nil, nil
		}
	}
	var turnPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_turns WHERE scope_kind=? AND scope_id=? AND run_id=? AND sequence=?`, req.Scope.Kind, req.Scope.ID, req.RunID, run.LastAppliedTurn+1).Scan(&turnPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		turn, err := decodeAgentTurnRecord(turnPayload)
		if err != nil {
			return nil, err
		}
		if conversationTaskFinishedBeforePromotion(turn) {
			return nil, nil
		}
	}
	task, updated, event, err := buildConversationTaskContinuation(req, run, conversation, message)
	if err != nil || task == nil {
		return nil, err
	}
	if task.ThreadRootID != task.SourceMessageID {
		root, err := getSQLiteChannelMessage(ctx, conn, req.Scope, conversationID, task.ThreadRootID)
		if err != nil {
			return nil, err
		}
		if root == nil || root.ConversationID != conversationID {
			return nil, ErrInvalidConversationTask
		}
	}
	ack, err := buildConversationTaskAcknowledgment(task, conversation, message)
	if err != nil {
		return nil, err
	}
	if err := updateSQLiteAgentRunConn(ctx, conn, updated, run.Revision); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO conversation_tasks
		(scope_kind,scope_id,id,source_run_id,task_key,request_digest,owner_type,owner_id,actor_type,actor_id,
		conversation_id,thread_root_id,work_run_id,work_status,is_active,created_at,payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, task.Scope.Kind, task.Scope.ID, task.ID, task.SourceRunID, task.TaskKey, task.RequestDigest,
		task.Owner.Type, task.Owner.ID, task.AuthenticatedActor.Type, task.AuthenticatedActor.ID, task.ConversationID, task.ThreadRootID,
		task.WorkRunID, updated.Status, 1, task.CreatedAt.UTC(), string(payload))
	if err != nil {
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
	return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(updated), SourceRun: cloneAgentRun(updated), AcknowledgmentMessageID: ack.Message.ID}, nil
}

func getSQLiteConversationTaskByWorkRun(ctx context.Context, queryer sqliteDependencyQueryer, scope Scope, runID string) (*ConversationTask, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks WHERE scope_kind=? AND scope_id=? AND work_run_id=?`, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationTaskSQL(payload)
}
