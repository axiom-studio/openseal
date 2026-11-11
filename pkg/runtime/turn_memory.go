package runtime

import (
	"encoding/json"
)

func cloneAgentTurn(in *AgentTurn) *AgentTurn {
	if in == nil {
		return nil
	}
	var out AgentTurn
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}
