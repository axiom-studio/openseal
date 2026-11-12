package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *SQLiteStore) prepareConversationTaskAdmissionConn(ctx context.Context, conn *sql.Conn, claim *models.AgentRunClaim, runs []*models.AgentRun) error {
	return models.PrepareConversationTaskAdmission(ctx, claim, runs, models.ConversationTaskAdmissionLookup{
		Task: func(ctx context.Context, scope models.Scope, id string) (*models.ConversationTask, error) {
			var payload string
			err := conn.QueryRowContext(ctx, `SELECT payload FROM conversation_tasks WHERE scope_kind = ? AND scope_id = ? AND work_run_id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return sqlstore.DecodeConversationTask(payload)
		},
		Run: func(ctx context.Context, scope models.Scope, id string) (*models.AgentRun, error) {
			return getSQLiteAgentRun(ctx, conn, scope, id)
		},
		Conversation: func(ctx context.Context, scope models.Scope, id string) (*models.Conversation, error) {
			return getSQLiteConversation(ctx, conn, scope, id)
		},
		Message: func(ctx context.Context, scope models.Scope, conversationID, id string) (*models.ChannelMessage, error) {
			return getSQLiteChannelMessage(ctx, conn, scope, conversationID, id)
		},
		Request: func(ctx context.Context, scope models.Scope, id string) (*models.AgentRequest, error) {
			var payload string
			err := conn.QueryRowContext(ctx, `SELECT payload FROM agent_requests WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return sqlstore.DecodeAgentRequest(payload)
		},
		Round: func(ctx context.Context, scope models.Scope, conversationID, key string) (*models.ParticipationRound, error) {
			return getSQLiteParticipationRoundByKey(ctx, conn, scope, conversationID, key)
		},
	})
}
