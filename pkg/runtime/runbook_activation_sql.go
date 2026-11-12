package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeRunbookActivation(payload string) (*models.RunbookActivation, error) {
	var value models.RunbookActivation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return &value, nil
}
