package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// ReceiveCallbackEventWithRunEvent commits a verified provider delivery and
// its normalized wake observation together. A failed publication rolls back
// callback persistence; a retry retains the original receipt timestamp.
func (s *SQLiteStore) ReceiveCallbackEventWithRunEvent(ctx context.Context, value *CallbackEventReceipt, receipt *RunEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	if err := validateRunEventReceipt(receipt); err != nil {
		return nil, false, err
	}
	if value.Scope != receipt.Event.Scope || value.Event.ID != receipt.Event.ID || value.Event.Type != receipt.Event.Type ||
		value.Event.Subject != receipt.Event.Subject || !value.Event.OccurredAt.Equal(receipt.Event.OccurredAt) {
		return nil, false, ErrInvalidRunEventWait
	}
	if !sameVerifiedCallbackObservation(value.Event, receipt.Event) {
		return nil, false, ErrCallbackRegistrationConflict
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	var existingPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM callback_events
		WHERE scope_kind=? AND scope_id=? AND registration_id=? AND event_source=? AND event_id=?`,
		value.Scope.Kind, value.Scope.ID, value.RegistrationID, value.Event.Source, value.Event.ID).Scan(&existingPayload)
	replayed := err == nil
	stored := value
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if replayed {
		var current CallbackEventReceipt
		decoder := json.NewDecoder(strings.NewReader(existingPayload))
		decoder.UseNumber()
		if err := decoder.Decode(&current); err != nil {
			return nil, false, err
		}
		normalizeStoredCallbackReceipt(&current)
		if err := current.Validate(); err != nil {
			return nil, false, err
		}
		if !sameCallbackProviderObservation(current.Event, value.Event) {
			return nil, false, ErrRunEventConflict
		}
		stored = &current
	} else {
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, false, err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO callback_events
			(scope_kind,scope_id,id,registration_id,event_source,event_id,status,available_at,lease_owner,lease_expires_at,attempts,revision,updated_at,payload)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, value.ID, value.RegistrationID,
			value.Event.Source, value.Event.ID, value.Status, value.AvailableAt.UTC(), value.LeaseOwner,
			nullableSQLiteTime(value.LeaseExpiresAt), value.Attempts, value.Revision, value.UpdatedAt.UTC(), string(payload))
		if sqliteUniqueViolation(err) {
			return nil, false, ErrCallbackRegistrationConflict
		}
		if err != nil {
			return nil, false, err
		}
	}
	observation := *receipt
	observation.ReceivedAt = stored.CreatedAt.UTC()
	if _, err := publishSQLiteRunEventConn(ctx, conn, &observation); err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	return cloneCallbackEventReceipt(stored), replayed, nil
}

var _ CallbackRunEventIntakeStore = (*SQLiteStore)(nil)
