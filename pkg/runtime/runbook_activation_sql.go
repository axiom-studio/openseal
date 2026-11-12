package runtime

import (
	"encoding/json"
)

func decodeRunbookActivation(payload string) (*RunbookActivation, error) {
	var value RunbookActivation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return &value, nil
}
