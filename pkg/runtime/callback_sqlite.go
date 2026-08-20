package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func migrateCallbackRegistrySQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS callback_registrations (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			ingress_route TEXT NOT NULL UNIQUE, provider TEXT NOT NULL, status TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id), CHECK (revision > 0)
		);
		CREATE INDEX IF NOT EXISTS idx_callback_registrations_scope
			ON callback_registrations(scope_kind,scope_id,provider,status,updated_at DESC);
		CREATE TABLE IF NOT EXISTS callback_events (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			registration_id TEXT NOT NULL, event_source TEXT NOT NULL, event_id TEXT NOT NULL,
			status TEXT NOT NULL, available_at DATETIME NOT NULL, lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at DATETIME, attempts INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id),
			UNIQUE (scope_kind,scope_id,registration_id,event_source,event_id), CHECK (revision > 0)
		);
		CREATE INDEX IF NOT EXISTS idx_callback_events_registration
			ON callback_events(scope_kind,scope_id,registration_id,status,updated_at DESC);
	`)
	if err != nil {
		return err
	}
	columns, err := sqliteTableColumns(db, "callback_events")
	if err != nil {
		return err
	}
	additions := map[string]string{
		"available_at":     `ALTER TABLE callback_events ADD COLUMN available_at DATETIME`,
		"lease_owner":      `ALTER TABLE callback_events ADD COLUMN lease_owner TEXT NOT NULL DEFAULT ''`,
		"lease_expires_at": `ALTER TABLE callback_events ADD COLUMN lease_expires_at DATETIME`,
		"attempts":         `ALTER TABLE callback_events ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`,
	}
	for name, statement := range additions {
		if !columns[name] {
			if _, err := db.Exec(statement); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`UPDATE callback_events SET available_at=updated_at WHERE available_at IS NULL;
		CREATE INDEX IF NOT EXISTS idx_callback_events_dispatch
		ON callback_events(scope_kind,scope_id,status,available_at,lease_expires_at)`)
	return err
}

func (s *SQLiteStore) CreateCallbackRegistration(ctx context.Context, value *CallbackRegistration) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO callback_registrations
		(scope_kind,scope_id,id,ingress_route,provider,status,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?,?)`,
		value.Scope.Kind, value.Scope.ID, value.ID, value.IngressRoute, value.Provider, value.Status,
		value.Revision, value.UpdatedAt, string(payload))
	if sqliteUniqueViolation(err) {
		return ErrCallbackRegistrationConflict
	}
	return err
}

func (s *SQLiteStore) GetCallbackRegistration(ctx context.Context, scope Scope, id string) (*CallbackRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteCallbackRegistration(s.db.QueryRowContext(ctx, `SELECT payload FROM callback_registrations
		WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) GetCallbackRegistrationByIngressRoute(ctx context.Context, route string) (*CallbackRegistration, error) {
	if !validOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, ErrInvalidCallbackRegistration
	}
	return scanSQLiteCallbackRegistration(s.db.QueryRowContext(ctx,
		`SELECT payload FROM callback_registrations WHERE ingress_route=?`, strings.TrimSpace(route)))
}

func (s *SQLiteStore) ListCallbackRegistrations(ctx context.Context, filter CallbackRegistrationFilter) ([]*CallbackRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM callback_registrations WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Provider != "" {
		query += ` AND provider=?`
		args = append(args, filter.Provider)
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY updated_at DESC,id ASC LIMIT ? OFFSET ?`
	limit, offset := normalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*CallbackRegistration, 0)
	for rows.Next() {
		value, err := scanSQLiteCallbackRegistration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateCallbackRegistration(ctx context.Context, value *CallbackRegistration, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE callback_registrations SET ingress_route=?,provider=?,status=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.IngressRoute, value.Provider, value.Status,
		value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if sqliteUniqueViolation(err) {
		return ErrCallbackRegistrationConflict
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		current, getErr := s.GetCallbackRegistration(ctx, value.Scope, value.ID)
		if getErr != nil {
			return getErr
		}
		if current == nil {
			return ErrCallbackRegistrationNotFound
		}
		return ErrCallbackRegistrationConflict
	}
	return nil
}

func (s *SQLiteStore) ReceiveCallbackEvent(ctx context.Context, value *CallbackEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO callback_events
		(scope_kind,scope_id,id,registration_id,event_source,event_id,status,available_at,lease_owner,lease_expires_at,attempts,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		value.Scope.Kind, value.Scope.ID, value.ID, value.RegistrationID, value.Event.Source, value.Event.ID,
		value.Status, value.AvailableAt, value.LeaseOwner, nullableSQLiteTime(value.LeaseExpiresAt), value.Attempts,
		value.Revision, value.UpdatedAt, string(payload))
	if err != nil {
		return nil, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if rows == 1 {
		return cloneCallbackEventReceipt(value), false, nil
	}
	current, err := scanSQLiteCallbackEvent(s.db.QueryRowContext(ctx, `SELECT payload FROM callback_events
		WHERE scope_kind=? AND scope_id=? AND registration_id=? AND event_source=? AND event_id=?`,
		value.Scope.Kind, value.Scope.ID, value.RegistrationID, value.Event.Source, value.Event.ID))
	return current, true, err
}

func (s *SQLiteStore) UpdateCallbackEvent(ctx context.Context, value *CallbackEventReceipt, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE callback_events SET status=?,available_at=?,lease_owner=?,lease_expires_at=?,attempts=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Status, value.AvailableAt, value.LeaseOwner,
		nullableSQLiteTime(value.LeaseExpiresAt), value.Attempts, value.Revision, value.UpdatedAt,
		string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrCallbackRegistrationConflict
	}
	return nil
}

func (s *SQLiteStore) ClaimCallbackEvent(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*CallbackEventReceipt, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidCallbackRegistration
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	receipt, err := scanSQLiteCallbackEvent(conn.QueryRowContext(ctx, `SELECT payload FROM callback_events
		WHERE scope_kind=? AND scope_id=? AND available_at<=?
		  AND (status=? OR (status=? AND lease_expires_at<=?))
		ORDER BY available_at ASC,updated_at ASC,id ASC LIMIT 1`, scope.Kind, scope.ID, now,
		CallbackEventPending, CallbackEventLeased, now))
	if err != nil || receipt == nil {
		return nil, err
	}
	receipt.Status, receipt.LeaseOwner = CallbackEventLeased, strings.TrimSpace(worker)
	receipt.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	receipt.Attempts++
	receipt.Revision++
	receipt.UpdatedAt = now.UTC()
	payload, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE callback_events
		SET status=?,lease_owner=?,lease_expires_at=?,attempts=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, receipt.Status, receipt.LeaseOwner,
		receipt.LeaseExpiresAt, receipt.Attempts, receipt.Revision, receipt.UpdatedAt, string(payload),
		receipt.Scope.Kind, receipt.Scope.ID, receipt.ID, receipt.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrCallbackRegistrationConflict
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (s *SQLiteStore) SaveClaimedCallbackEvent(ctx context.Context, receipt *CallbackEventReceipt, expectedRevision int64, leaseOwner string) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE callback_events
		SET status=?,available_at=?,lease_owner=?,lease_expires_at=?,attempts=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=? AND lease_owner=?`, receipt.Status,
		receipt.AvailableAt, receipt.LeaseOwner, nullableSQLiteTime(receipt.LeaseExpiresAt), receipt.Attempts,
		receipt.Revision, receipt.UpdatedAt, string(payload), receipt.Scope.Kind, receipt.Scope.ID, receipt.ID,
		expectedRevision, CallbackEventLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrCallbackRegistrationConflict
	}
	return nil
}

func scanSQLiteCallbackRegistration(scanner sqliteExternalConversationScanner) (*CallbackRegistration, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value CallbackRegistration
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteCallbackEvent(scanner sqliteExternalConversationScanner) (*CallbackEventReceipt, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value CallbackEventReceipt
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	normalizeStoredCallbackReceipt(&value)
	return &value, value.Validate()
}
