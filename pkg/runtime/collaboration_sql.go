package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeAgentRequest(payload string) (*models.AgentRequest, error) {
	var request models.AgentRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return nil, fmt.Errorf("decode agent request: %w", err)
	}
	return &request, nil
}
