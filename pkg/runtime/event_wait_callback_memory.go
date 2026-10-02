package runtime

import (
	"bytes"
	"context"
	"encoding/json"
)

func (s *MemoryStore) ReceiveCallbackEventWithRunEvent(ctx context.Context, value *CallbackEventReceipt, observation *RunEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	if err := validateRunEventReceipt(observation); err != nil {
		return nil, false, err
	}
	if value.Scope != observation.Event.Scope || value.Event.ID != observation.Event.ID ||
		value.Event.Type != observation.Event.Type || value.Event.Subject != observation.Event.Subject ||
		!value.Event.OccurredAt.Equal(observation.Event.OccurredAt) || !sameVerifiedCallbackObservation(value.Event, observation.Event) {
		return nil, false, ErrCallbackRegistrationConflict
	}
	callback, err := cloneMemoryCallbackEventReceipt(value)
	if err != nil {
		return nil, false, err
	}
	inbox, err := cloneMemoryRunEventReceipt(observation)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	replayed := false
	if current := s.callbackEvents[value.ID]; current != nil {
		if current.Scope != value.Scope || current.RegistrationID != value.RegistrationID ||
			current.Event.Source != value.Event.Source || current.Event.ID != value.Event.ID ||
			!sameCallbackProviderObservation(current.Event, value.Event) {
			return nil, false, ErrCallbackRegistrationConflict
		}
		callback = current
		replayed = true
	}
	// The original verified intake time decides deadline eligibility, even if
	// a provider retries later or an older receipt needs its inbox projection.
	inbox.ReceivedAt = callback.CreatedAt
	if err := validateRunEventReceipt(inbox); err != nil {
		return nil, false, err
	}
	result, err := cloneMemoryCallbackEventReceipt(callback)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if _, err := s.publishMemoryRunEventLocked(inbox); err != nil {
		return nil, false, err
	}
	if !replayed {
		s.callbackEvents[callback.ID] = callback
	}
	return result, replayed, nil
}

func cloneMemoryCallbackEventReceipt(value *CallbackEventReceipt) (*CallbackEventReceipt, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result CallbackEventReceipt
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

var _ CallbackRunEventIntakeStore = (*MemoryStore)(nil)
