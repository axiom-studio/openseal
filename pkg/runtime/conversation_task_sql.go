package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeConversationTaskSQL(payload string) (*models.ConversationTask, error) {
	var task models.ConversationTask
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return nil, fmt.Errorf("decode conversation task: %w", err)
	}
	return &task, nil
}

func scanConversationTaskResults(rows *sql.Rows) ([]*models.ConversationTaskResult, error) {
	results := make([]*models.ConversationTaskResult, 0)
	for rows.Next() {
		var taskPayload, runPayload string
		if err := rows.Scan(&taskPayload, &runPayload); err != nil {
			return nil, err
		}
		task, err := decodeConversationTaskSQL(taskPayload)
		if err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(runPayload)
		if err != nil {
			return nil, err
		}
		results = append(results, &models.ConversationTaskResult{Task: task, WorkRun: run})
	}
	return results, rows.Err()
}
