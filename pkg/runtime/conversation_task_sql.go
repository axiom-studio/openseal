package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

func decodeConversationTaskSQL(payload string) (*ConversationTask, error) {
	var task ConversationTask
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return nil, fmt.Errorf("decode conversation task: %w", err)
	}
	return &task, nil
}

func scanConversationTaskResults(rows *sql.Rows) ([]*ConversationTaskResult, error) {
	results := make([]*ConversationTaskResult, 0)
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
		results = append(results, &ConversationTaskResult{Task: task, WorkRun: run})
	}
	return results, rows.Err()
}
