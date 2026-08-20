package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const callbackRegistryMigrationVersion int64 = 39

func (s *PostgresStore) migrateCallbackRegistry(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("callback_registrations")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			ingress_route TEXT NOT NULL UNIQUE, provider TEXT NOT NULL, status TEXT NOT NULL,
			revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id), CHECK (revision > 0)
		);
		CREATE INDEX IF NOT EXISTS callback_registrations_scope_idx
			ON `+s.table("callback_registrations")+` (scope_kind,scope_id,provider,status,updated_at DESC);
		CREATE TABLE IF NOT EXISTS `+s.table("callback_events")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			registration_id TEXT NOT NULL, event_source TEXT NOT NULL, event_id TEXT NOT NULL,
			status TEXT NOT NULL, available_at TIMESTAMPTZ NOT NULL, lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ, attempts INTEGER NOT NULL DEFAULT 0,
			revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id),
			UNIQUE (scope_kind,scope_id,registration_id,event_source,event_id),
			CHECK (revision > 0)
		);
		CREATE INDEX IF NOT EXISTS callback_events_registration_idx
			ON `+s.table("callback_events")+` (scope_kind,scope_id,registration_id,status,updated_at DESC)
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'portable signed callback registry')
		ON CONFLICT(version) DO NOTHING`, callbackRegistryMigrationVersion)
	return err
}

func (s *PostgresStore) CreateCallbackRegistration(ctx context.Context, value *CallbackRegistration) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("callback_registrations")+`
		(scope_kind,scope_id,id,ingress_route,provider,status,revision,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, value.Scope.Kind, value.Scope.ID, value.ID,
		value.IngressRoute, value.Provider, value.Status, value.Revision, value.UpdatedAt, string(payload))
	if postgresUniqueViolation(err) {
		return ErrCallbackRegistrationConflict
	}
	return err
}

func (s *PostgresStore) GetCallbackRegistration(ctx context.Context, scope Scope, id string) (*CallbackRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanPostgresCallbackRegistration(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("callback_registrations")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) GetCallbackRegistrationByIngressRoute(ctx context.Context, route string) (*CallbackRegistration, error) {
	if !validOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, ErrInvalidCallbackRegistration
	}
	return scanPostgresCallbackRegistration(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("callback_registrations")+`
		WHERE ingress_route=$1`, strings.TrimSpace(route)))
}

func (s *PostgresStore) ListCallbackRegistrations(ctx context.Context, filter CallbackRegistrationFilter) ([]*CallbackRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("callback_registrations") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	if filter.Provider != "" {
		query += fmt.Sprintf(` AND provider=$%d`, placeholder)
		args = append(args, filter.Provider)
		placeholder++
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "status", statuses)
	query += fmt.Sprintf(` ORDER BY updated_at DESC,id ASC LIMIT $%d OFFSET $%d`, placeholder, placeholder+1)
	limit, offset := normalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*CallbackRegistration, 0)
	for rows.Next() {
		value, scanErr := scanPostgresCallbackRegistration(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateCallbackRegistration(ctx context.Context, value *CallbackRegistration, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("callback_registrations")+`
		SET ingress_route=$1,provider=$2,status=$3,revision=$4,updated_at=$5,payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`, value.IngressRoute, value.Provider,
		value.Status, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if postgresUniqueViolation(err) {
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

func (s *PostgresStore) ReceiveCallbackEvent(ctx context.Context, value *CallbackEventReceipt) (*CallbackEventReceipt, bool, error) {
	if err := value.Validate(); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("callback_events")+`
		(scope_kind,scope_id,id,registration_id,event_source,event_id,status,available_at,lease_owner,lease_expires_at,attempts,revision,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb) ON CONFLICT DO NOTHING`, value.Scope.Kind, value.Scope.ID,
		value.ID, value.RegistrationID, value.Event.Source, value.Event.ID, value.Status, value.AvailableAt, value.LeaseOwner,
		nullablePostgresTime(value.LeaseExpiresAt), value.Attempts, value.Revision, value.UpdatedAt, string(payload))
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
	current, err := scanPostgresCallbackEvent(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("callback_events")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND registration_id=$3 AND event_source=$4 AND event_id=$5`,
		value.Scope.Kind, value.Scope.ID, value.RegistrationID, value.Event.Source, value.Event.ID))
	return current, true, err
}

func (s *PostgresStore) UpdateCallbackEvent(ctx context.Context, value *CallbackEventReceipt, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("callback_events")+`
		SET status=$1,available_at=$2,lease_owner=$3,lease_expires_at=$4,attempts=$5,revision=$6,updated_at=$7,payload=$8::jsonb
		WHERE scope_kind=$9 AND scope_id=$10 AND id=$11 AND revision=$12`, value.Status, value.AvailableAt, value.LeaseOwner,
		nullablePostgresTime(value.LeaseExpiresAt), value.Attempts, value.Revision, value.UpdatedAt, string(payload),
		value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
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

func (s *PostgresStore) ClaimCallbackEvent(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*CallbackEventReceipt, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidCallbackRegistration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	receipt, err := scanPostgresCallbackEvent(tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("callback_events")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND available_at<=$3
		  AND (status=$4 OR (status=$5 AND lease_expires_at<=$3))
		ORDER BY available_at ASC,updated_at ASC,id ASC FOR UPDATE SKIP LOCKED LIMIT 1`,
		scope.Kind, scope.ID, now, CallbackEventPending, CallbackEventLeased))
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("callback_events")+`
		SET status=$1,lease_owner=$2,lease_expires_at=$3,attempts=$4,revision=$5,updated_at=$6,payload=$7::jsonb
		WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`, receipt.Status, receipt.LeaseOwner,
		receipt.LeaseExpiresAt, receipt.Attempts, receipt.Revision, receipt.UpdatedAt, string(payload),
		receipt.Scope.Kind, receipt.Scope.ID, receipt.ID, receipt.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrCallbackRegistrationConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (s *PostgresStore) SaveClaimedCallbackEvent(ctx context.Context, receipt *CallbackEventReceipt, expectedRevision int64, leaseOwner string) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("callback_events")+`
		SET status=$1,available_at=$2,lease_owner=$3,lease_expires_at=$4,attempts=$5,revision=$6,updated_at=$7,payload=$8::jsonb
		WHERE scope_kind=$9 AND scope_id=$10 AND id=$11 AND revision=$12 AND status=$13 AND lease_owner=$14`,
		receipt.Status, receipt.AvailableAt, receipt.LeaseOwner, nullablePostgresTime(receipt.LeaseExpiresAt), receipt.Attempts,
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

func scanPostgresCallbackRegistration(scanner sqliteExternalConversationScanner) (*CallbackRegistration, error) {
	var payload []byte
	if err := scanner.Scan(&payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value CallbackRegistration
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanPostgresCallbackEvent(scanner sqliteExternalConversationScanner) (*CallbackEventReceipt, error) {
	var payload []byte
	if err := scanner.Scan(&payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value CallbackEventReceipt
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	normalizeStoredCallbackReceipt(&value)
	return &value, value.Validate()
}
