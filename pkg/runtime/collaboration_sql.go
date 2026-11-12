package runtime

import (
	"encoding/json"
	"fmt"
)

func decodeAgentRequest(payload string) (*AgentRequest, error) {
	var request AgentRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return nil, fmt.Errorf("decode agent request: %w", err)
	}
	return &request, nil
}
