package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeActionCall(payload string) (*models.ActionCall, error) {
	var call models.ActionCall
	if err := json.Unmarshal([]byte(payload), &call); err != nil {
		return nil, err
	}
	return &call, nil
}

func decodeApproval(payload string) (*models.ApprovalCheckpoint, error) {
	var approval models.ApprovalCheckpoint
	if err := json.Unmarshal([]byte(payload), &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}
