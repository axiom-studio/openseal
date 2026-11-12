package runtime

import (
	"encoding/json"
	"fmt"
)

func decodeObjective(payload string) (*Objective, error) {
	var objective Objective
	if err := json.Unmarshal([]byte(payload), &objective); err != nil {
		return nil, fmt.Errorf("decode objective: %w", err)
	}
	return &objective, nil
}

func decodeAgentRun(payload string) (*AgentRun, error) {
	var run AgentRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return nil, fmt.Errorf("decode agent run: %w", err)
	}
	if run.Revision == 0 {
		run.Revision = 1
	}
	if run.AvailableAt.IsZero() {
		run.AvailableAt = run.CreatedAt
	}
	if run.QueueEnteredAt.IsZero() {
		run.QueueEnteredAt = run.CreatedAt
	}
	run.Kind = normalizeRunKind(run.Kind)
	return &run, nil
}
