package runtime

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

func scanConversationActiveRunsSQL(rows *sql.Rows) ([]*models.AgentRun, error) {
	runs := make([]*models.AgentRun, 0)
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

const conversationActiveRunProjectionColumns = `(scope_kind,scope_id,id,owner_type,owner_id,conversation_id,root_run_id,created_at)`
