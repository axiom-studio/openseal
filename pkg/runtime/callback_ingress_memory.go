package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"time"
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

func (s *MemoryStore) ClaimCallbackEvent(_ context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*CallbackEventReceipt, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidCallbackRegistration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *CallbackEventReceipt
	for _, receipt := range s.callbackEvents {
		if receipt.Scope != scope || receipt.AvailableAt.After(now) ||
			(receipt.Status != CallbackEventPending && !(receipt.Status == CallbackEventLeased && !receipt.LeaseExpiresAt.After(now))) {
			continue
		}
		if selected == nil || receipt.AvailableAt.Before(selected.AvailableAt) ||
			(receipt.AvailableAt.Equal(selected.AvailableAt) && receipt.CreatedAt.Before(selected.CreatedAt)) ||
			(receipt.AvailableAt.Equal(selected.AvailableAt) && receipt.CreatedAt.Equal(selected.CreatedAt) && receipt.ID < selected.ID) {
			selected = receipt
		}
	}
	if selected == nil {
		return nil, nil
	}
	selected.Status = CallbackEventLeased
	selected.Attempts++
	selected.LeaseOwner = strings.TrimSpace(worker)
	selected.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	selected.Revision++
	selected.UpdatedAt = now.UTC()
	return cloneCallbackEventReceipt(selected), nil
}

func (s *MemoryStore) SaveClaimedCallbackEvent(_ context.Context, receipt *CallbackEventReceipt, expectedRevision int64, leaseOwner string) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.callbackEvents[receipt.ID]
	if current == nil || current.Revision != expectedRevision || receipt.Revision != expectedRevision+1 ||
		current.Status != CallbackEventLeased || current.LeaseOwner != strings.TrimSpace(leaseOwner) ||
		receipt.UpdatedAt.Before(current.UpdatedAt) || receipt.UpdatedAt.After(current.LeaseExpiresAt) ||
		current.Scope != receipt.Scope || current.RegistrationID != receipt.RegistrationID ||
		current.Event.ID != receipt.Event.ID || current.Event.Source != receipt.Event.Source || !current.CreatedAt.Equal(receipt.CreatedAt) {
		return ErrCallbackRegistrationConflict
	}
	s.callbackEvents[receipt.ID] = cloneCallbackEventReceipt(receipt)
	return nil
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
