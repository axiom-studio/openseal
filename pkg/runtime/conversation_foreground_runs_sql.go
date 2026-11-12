package runtime

import (
	"database/sql"
	"math"
)

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
