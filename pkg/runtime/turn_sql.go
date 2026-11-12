package runtime

import (
	"encoding/json"
)

func decodeAgentTurnRecord(payload string) (*AgentTurn, error) {
	var turn AgentTurn
	if err := json.Unmarshal([]byte(payload), &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}
