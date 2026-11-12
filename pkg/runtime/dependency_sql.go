package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeRunDependencyGroup(payload string) (*models.RunDependencyGroup, error) {
	var group models.RunDependencyGroup
	if err := json.Unmarshal([]byte(payload), &group); err != nil {
		return nil, fmt.Errorf("decode run dependency group: %w", err)
	}
	return &group, nil
}

func decodeRunDependency(payload string) (*models.RunDependency, error) {
	var edge models.RunDependency
	if err := json.Unmarshal([]byte(payload), &edge); err != nil {
		return nil, fmt.Errorf("decode run dependency: %w", err)
	}
	return &edge, nil
}
