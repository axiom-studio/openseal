package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *SQLiteStore) ListConversationForegroundRuns(ctx context.Context, scope models.Scope, owner models.ObjectiveOwner, conversationID string, limit, offset int) ([]*models.AgentRun, error) {
	if err := models.ValidateConversationForegroundRunsQuery(scope, owner, conversationID, limit, offset); err != nil {
		return nil, err
	}
	pageSize := sqlstore.ConversationForegroundPageSize(limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM agent_runs
		WHERE scope_kind=? AND scope_id=? AND json_extract(payload,'$.owner.type')=? AND json_extract(payload,'$.owner.id')=?
		AND json_extract(payload,'$.kind')='conversation' AND json_extract(payload,'$.concurrencyKey')=?
		ORDER BY created_at DESC,id DESC LIMIT ?) legacy
		UNION ALL
		SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM agent_runs
		WHERE scope_kind=? AND scope_id=? AND json_extract(payload,'$.owner.type')=? AND json_extract(payload,'$.owner.id')=?
		AND json_extract(payload,'$.kind')='conversation' AND json_extract(payload,'$.context.conversationId')=?
		AND json_extract(payload,'$.concurrencyKey')<>?
		AND json_extract(payload,'$.concurrencyKey')=json_extract(payload,'$.context.conversationId')
			||CASE WHEN json_type(payload,'$.context.threadRootMessageId')='text' AND json_extract(payload,'$.context.threadRootMessageId')<>''
				THEN ':thread:'||json_extract(payload,'$.context.threadRootMessageId') ELSE '' END
			||CASE WHEN COALESCE(json_extract(payload,'$.assignedAgentId'),'')<>''
				THEN ':agent:'||json_extract(payload,'$.assignedAgentId') ELSE '' END
		ORDER BY created_at DESC,id DESC LIMIT ?) lanes
		ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, pageSize,
		scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, conversationID, pageSize, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return sqlstore.ScanConversationForegroundRuns(rows)
}
