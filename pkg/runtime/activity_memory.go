package runtime

import (
	"encoding/json"
)

func cloneActivityEvent(in *ActivityEvent) *ActivityEvent {
	if in == nil {
		return nil
	}
	var out ActivityEvent
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}
