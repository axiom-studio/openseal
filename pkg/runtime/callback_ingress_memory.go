package runtime

import (
	"context"
	"encoding/json"
)

func (s *MemoryStore) ReceiveCallbackEvent(_ context.Context, value *CallbackEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.callbackEvents[value.ID]; current != nil {
		if current.Scope != value.Scope || current.RegistrationID != value.RegistrationID ||
			current.Event.Source != value.Event.Source || current.Event.ID != value.Event.ID {
			return nil, false, ErrCallbackRegistrationConflict
		}
		return cloneCallbackEventReceipt(current), true, nil
	}
	s.callbackEvents[value.ID] = cloneCallbackEventReceipt(value)
	return cloneCallbackEventReceipt(value), false, nil
}

func (s *MemoryStore) UpdateCallbackEvent(_ context.Context, value *CallbackEventReceipt, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.callbackEvents[value.ID]
	if current == nil || current.Revision != expectedRevision {
		return ErrCallbackRegistrationConflict
	}
	s.callbackEvents[value.ID] = cloneCallbackEventReceipt(value)
	return nil
}

func cloneCallbackEventReceipt(value *CallbackEventReceipt) *CallbackEventReceipt {
	if value == nil {
		return nil
	}
	var result CallbackEventReceipt
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
