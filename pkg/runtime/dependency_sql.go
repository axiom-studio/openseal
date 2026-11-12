package runtime

import (
	"encoding/json"
	"fmt"
)

func decodeRunDependencyGroup(payload string) (*RunDependencyGroup, error) {
	var group RunDependencyGroup
	if err := json.Unmarshal([]byte(payload), &group); err != nil {
		return nil, fmt.Errorf("decode run dependency group: %w", err)
	}
	return &group, nil
}

func decodeRunDependency(payload string) (*RunDependency, error) {
	var edge RunDependency
	if err := json.Unmarshal([]byte(payload), &edge); err != nil {
		return nil, fmt.Errorf("decode run dependency: %w", err)
	}
	return &edge, nil
}
