package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const conversationTaskContinuationMigrationVersion int64 = 58

const postgresConversationTaskDuePredicate = `payload->>'kind'='conversation'
	AND payload->>'source'='chat' AND parent_run_id='' AND root_run_id=id
	AND status NOT IN ('completed','failed','canceled')
	AND payload->'context'->>'openseal.conversationTaskId' IS NULL`

func (s *PostgresStore) migrateConversationTaskContinuations(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, conversationTaskContinuationMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS agent_runs_conversation_task_due_idx ON `+s.table("agent_runs")+
		` (scope_kind,scope_id,created_at,id) WHERE `+postgresConversationTaskDuePredicate); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version,name) VALUES ($1,'indexed conversation continuation handoff') ON CONFLICT (version) DO NOTHING`, conversationTaskContinuationMigrationVersion)
	return err
}

func (s *PostgresStore) ListDueConversationTaskRuns(ctx context.Context, filter ConversationTaskDueFilter) ([]*AgentRun, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("agent_runs") + ` WHERE scope_kind=$1 AND scope_id=$2 AND ` + postgresConversationTaskDuePredicate + ` AND created_at<=$3`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
	if filter.AfterCreatedAt != nil {
		query += ` AND (created_at,id)>($4,$5) ORDER BY created_at,id LIMIT $6`
		args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID, filter.Limit)
	} else {
		query += ` ORDER BY created_at,id LIMIT $4`
		args = append(args, filter.Limit)
	}
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

func (s *PostgresStore) PromoteConversationTask(ctx context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	run, err := s.getPostgresAgentRunTx(ctx, tx, req.Scope, req.RunID, true)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	existing, err := s.getPostgresConversationTaskByWorkRun(ctx, tx, req.Scope, req.RunID)
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
	// Final response publication uses this same conversation row lock. This
	// serializes answer-first versus handoff-first without touching the runner.
	conversation, err := s.getPostgresConversation(ctx, tx, req.Scope, conversationID, true)
	if err != nil {
		return nil, err
	}
	message, err := s.getPostgresChannelMessage(ctx, tx, req.Scope, conversationID, messageID, false)
	if err != nil {
		return nil, err
	}
	for _, responseKey := range conversationTaskFinalResponseKeys(run) {
		final, err := s.getPostgresChannelMessageByKey(ctx, tx, req.Scope, conversationID, responseKey, false)
		if err != nil {
			return nil, err
		}
		if final != nil {
			return nil, nil
		}
	}
	if run.Owner.Type == OwnerTypeTeam {
		round, err := s.getPostgresParticipationRoundByKey(ctx, tx, req.Scope, conversationID, conversationTaskParticipationRoundKey(run), false)
		if err != nil {
			return nil, err
		}
		if conversationTaskFinishedTeamRound(run, round) {
			return nil, nil
		}
	}
	var turnPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_turns")+
		` WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3 AND sequence=$4 FOR UPDATE`, req.Scope.Kind, req.Scope.ID, req.RunID, run.LastAppliedTurn+1).Scan(&turnPayload)
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
		root, err := s.getPostgresChannelMessage(ctx, tx, req.Scope, conversationID, task.ThreadRootID, false)
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
	if err := s.updatePostgresAgentRunTx(ctx, tx, updated, run.Revision); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_tasks")+`
		(scope_kind,scope_id,id,source_run_id,task_key,request_digest,owner_type,owner_id,actor_type,actor_id,
		conversation_id,thread_root_id,work_run_id,work_status,is_active,created_at,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::jsonb)`, task.Scope.Kind, task.Scope.ID, task.ID,
		task.SourceRunID, task.TaskKey, task.RequestDigest, task.Owner.Type, task.Owner.ID, task.AuthenticatedActor.Type, task.AuthenticatedActor.ID,
		task.ConversationID, task.ThreadRootID, task.WorkRunID, updated.Status, true, task.CreatedAt.UTC(), string(payload))
	if err != nil {
		return nil, normalizePostgresConversationTaskConflict(err)
	}
	if _, err := s.insertPostgresActivityTx(ctx, tx, event); err != nil {
		return nil, err
	}
	if err := s.updatePostgresConversationTx(ctx, tx, ack.Conversation, ack.ExpectedRevision); err != nil {
		return nil, err
	}
	if err := s.insertPostgresChannelMessageTx(ctx, tx, ack.Message); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(updated), SourceRun: cloneAgentRun(updated), AcknowledgmentMessageID: ack.Message.ID}, nil
}

func (s *PostgresStore) getPostgresConversationTaskByWorkRun(ctx context.Context, queryer postgresDependencyQueryer, scope Scope, runID string) (*ConversationTask, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM `+s.table("conversation_tasks")+` WHERE scope_kind=$1 AND scope_id=$2 AND work_run_id=$3`, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationTaskSQL(payload)
}
