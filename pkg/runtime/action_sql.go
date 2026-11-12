package runtime

import (
	"encoding/json"
)

func decodeActionCall(payload string) (*ActionCall, error) {
	var call ActionCall
	if err := json.Unmarshal([]byte(payload), &call); err != nil {
		return nil, err
	}
	return &call, nil
}

func decodeApproval(payload string) (*ApprovalCheckpoint, error) {
	var approval ApprovalCheckpoint
	if err := json.Unmarshal([]byte(payload), &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}
