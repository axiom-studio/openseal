package runtime

import (
	"context"
	"database/sql"
	"math"
)

func (s *PostgresStore) ListConversationForegroundRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) ([]*AgentRun, error) {
	if err := validateConversationForegroundRunsQuery(scope, owner, conversationID, limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM `+s.table("agent_runs")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND payload->'owner'->>'type'=$3 AND payload->'owner'->>'id'=$4
		AND payload->>'kind'='conversation' AND payload->>'concurrencyKey'=$5
		ORDER BY created_at DESC,id DESC LIMIT $8) legacy
		UNION ALL
		SELECT payload,created_at,id FROM (SELECT payload,created_at,id FROM `+s.table("agent_runs")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND payload->'owner'->>'type'=$3 AND payload->'owner'->>'id'=$4
		AND payload->>'kind'='conversation' AND payload->'context'->>'conversationId'=$5
		AND jsonb_typeof(payload->'context'->'threadRootMessageId')='string' AND payload->'context'->>'threadRootMessageId'<>''
		AND payload->>'concurrencyKey'=(payload->'context'->>'conversationId')||':thread:'||(payload->'context'->>'threadRootMessageId')
		ORDER BY created_at DESC,id DESC LIMIT $8) thread
		ORDER BY created_at DESC,id DESC LIMIT $6 OFFSET $7`, scope.Kind, scope.ID, owner.Type, owner.ID, conversationID, limit, offset, conversationForegroundSQLPageSize(limit, offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationForegroundRunsSQL(rows)
}

func conversationForegroundSQLPageSize(limit, offset int) int {
	if offset > math.MaxInt-limit {
		return math.MaxInt
	}
	return offset + limit
}

func scanConversationForegroundRunsSQL(rows *sql.Rows) ([]*AgentRun, error) {
	runs := make([]*AgentRun, 0)
	for rows.Next() {
		var payload, id string
		var createdAt interface{}
		if err := rows.Scan(&payload, &createdAt, &id); err != nil {
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
