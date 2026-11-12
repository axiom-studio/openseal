package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeObjective(payload string) (*models.Objective, error) {
	var objective models.Objective
	if err := json.Unmarshal([]byte(payload), &objective); err != nil {
		return nil, fmt.Errorf("decode objective: %w", err)
	}
	return &objective, nil
}

func decodeAgentRun(payload string) (*models.AgentRun, error) {
	var run models.AgentRun
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
	run.Kind = models.NormalizeRunKind(run.Kind)
	return &run, nil
}
