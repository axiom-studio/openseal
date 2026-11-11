package runtime

import (
	"encoding/json"
)

func cloneCallbackEventReceipt(value *CallbackEventReceipt) *CallbackEventReceipt {
	if value == nil {
		return nil
	}
	var result CallbackEventReceipt
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
