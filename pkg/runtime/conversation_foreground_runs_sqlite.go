package runtime

import "context"

func (s *SQLiteStore) ListConversationForegroundRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) ([]*AgentRun, error) {
	if err := validateConversationForegroundRunsQuery(scope, owner, conversationID, limit, offset); err != nil {
		return nil, err
	}
	pageSize := conversationForegroundSQLPageSize(limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM agent_runs
		WHERE scope_kind=? AND scope_id=? AND json_extract(payload,'$.owner.type')=? AND json_extract(payload,'$.owner.id')=?
		AND json_extract(payload,'$.kind')='conversation' AND json_extract(payload,'$.concurrencyKey')=?
		ORDER BY created_at DESC,id DESC LIMIT ?) legacy
		UNION ALL
		SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM agent_runs
		WHERE scope_kind=? AND scope_id=? AND json_extract(payload,'$.owner.type')=? AND json_extract(payload,'$.owner.id')=?
		AND json_extract(payload,'$.kind')='conversation' AND json_extract(payload,'$.context.conversationId')=?
		AND json_type(payload,'$.context.threadRootMessageId')='text' AND json_extract(payload,'$.context.threadRootMessageId')<>''
		AND json_extract(payload,'$.concurrencyKey')=json_extract(payload,'$.context.conversationId')||':thread:'||json_extract(payload,'$.context.threadRootMessageId')
		ORDER BY created_at DESC,id DESC LIMIT ?) thread
		ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, pageSize,
		scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, pageSize, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationForegroundRunsSQL(rows)
}
