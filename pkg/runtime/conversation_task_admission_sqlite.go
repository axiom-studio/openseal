package runtime

import (
	"context"
	"database/sql"
	"errors"
)

func (s *SQLiteStore) prepareConversationTaskAdmissionConn(ctx context.Context, conn *sql.Conn, claim *AgentRunClaim, runs []*AgentRun) error {
	return prepareConversationTaskAdmission(ctx, claim, runs, conversationTaskAdmissionLookup{
		Task: func(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
			var payload string
			err := conn.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ? AND work_run_id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return decodeConversationTaskSQL(payload)
		},
		Run: func(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
			return getSQLiteAgentRun(ctx, conn, scope, id)
		},
		Conversation: func(ctx context.Context, scope Scope, id string) (*Conversation, error) {
			return getSQLiteConversation(ctx, conn, scope, id)
		},
		Message: func(ctx context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
			return getSQLiteChannelMessage(ctx, conn, scope, conversationID, id)
		},
		Request: func(ctx context.Context, scope Scope, id string) (*AgentRequest, error) {
			var payload string
			err := conn.QueryRowContext(ctx, `SELECT payload FROM agent_requests WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return decodeAgentRequest(payload)
		},
		Round: func(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRound, error) {
			return getSQLiteParticipationRoundByKey(ctx, conn, scope, conversationID, key)
		},
	})
}
