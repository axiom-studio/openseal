package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeAgentTurnRecord(payload string) (*models.AgentTurn, error) {
	var turn models.AgentTurn
	if err := json.Unmarshal([]byte(payload), &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}
