package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var _ CallbackRunEventIntakeStore = (*PostgresStore)(nil)

// ReceiveCallbackEventWithRunEvent commits the provider receipt and its verified
// workflow observation together. A retry retains the first acquisition time,
// including when repairing a receipt written by an older runtime.
func (s *PostgresStore) ReceiveCallbackEventWithRunEvent(ctx context.Context, value *CallbackEventReceipt, observation *RunEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	if err := validateRunEventReceipt(observation); err != nil {
		return nil, false, err
	}
	if !sameVerifiedCallbackObservation(value.Event, observation.Event) {
		return nil, false, ErrCallbackRegistrationConflict
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("callback_events")+`
		(scope_kind,scope_id,id,registration_id,event_source,event_id,status,available_at,lease_owner,lease_expires_at,attempts,revision,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb) ON CONFLICT DO NOTHING`, value.Scope.Kind, value.Scope.ID,
		value.ID, value.RegistrationID, value.Event.Source, value.Event.ID, value.Status, value.AvailableAt, value.LeaseOwner,
		nullablePostgresTime(value.LeaseExpiresAt), value.Attempts, value.Revision, value.UpdatedAt, string(payload))
	if err != nil {
		return nil, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	stored, replayed := value, count == 0
	if replayed {
		var data []byte
		err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("callback_events")+`
			WHERE scope_kind=$1 AND scope_id=$2 AND registration_id=$3 AND event_source=$4 AND event_id=$5`,
			value.Scope.Kind, value.Scope.ID, value.RegistrationID, value.Event.Source, value.Event.ID).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrCallbackRegistrationConflict
		}
		if err != nil {
			return nil, false, err
		}
		stored = &CallbackEventReceipt{}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(stored); err != nil {
			return nil, false, err
		}
		if !sameCallbackProviderObservation(stored.Event, value.Event) {
			return nil, false, ErrCallbackRegistrationConflict
		}
	}
	copy := *observation
	copy.ReceivedAt = stored.CreatedAt
	if _, err := s.publishPostgresRunEventTx(ctx, tx, &copy); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stored, replayed, nil
}

// Provider JSON numbers may be rendered differently after a JSONB round trip.
// Replay compares exact mathematical scalar values rather than float64 values
// or serialization formatting; host acquisition Actor is separate provenance.
func sameCallbackProviderObservation(a, b EventEnvelope) bool {
	if a.ID != b.ID || a.Scope != b.Scope || a.Source != b.Source || a.Type != b.Type || a.Subject != b.Subject || a.Severity != b.Severity || !a.OccurredAt.Equal(b.OccurredAt) {
		return false
	}
	normalize := func(value interface{}) (interface{}, error) {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var result interface{}
		err = decoder.Decode(&result)
		return result, err
	}
	left, le := normalize(map[string]interface{}{"attributes": a.Attributes, "payload": a.Payload})
	right, re := normalize(map[string]interface{}{"attributes": b.Attributes, "payload": b.Payload})
	return le == nil && re == nil && sameCallbackProviderJSON(left, right)
}

func sameCallbackProviderJSON(a, b interface{}) bool {
	switch a := a.(type) {
	case map[string]interface{}:
		right, ok := b.(map[string]interface{})
		if !ok || len(a) != len(right) {
			return false
		}
		for key, value := range a {
			other, exists := right[key]
			if !exists || !sameCallbackProviderJSON(value, other) {
				return false
			}
		}
		return true
	case []interface{}:
		right, ok := b.([]interface{})
		if !ok || len(a) != len(right) {
			return false
		}
		for index, value := range a {
			if !sameCallbackProviderJSON(value, right[index]) {
				return false
			}
		}
		return true
	default:
		return runEventScalarEqual(a, b)
	}
}
