package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *PostgresStore) migrateEventSourceSubscriptions(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + s.table("event_source_subscriptions") + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, source TEXT NOT NULL,
			status TEXT NOT NULL, revision BIGINT NOT NULL CHECK(revision>0), created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS event_source_subscriptions_scope_status_idx ON ` + s.table("event_source_subscriptions") + `(scope_kind, scope_id, status, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS ` + s.table("event_source_health") + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, subscription_id TEXT NOT NULL,
			revision BIGINT NOT NULL CHECK(revision>0), updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, subscription_id)
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES(22,'durable event source subscriptions and health') ON CONFLICT(version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateEventSourceSubscription(ctx context.Context, value *EventSourceSubscription) error {
	if value == nil || value.Validate() != nil {
		return ErrInvalidEventSourceSubscription
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("event_source_subscriptions")+`(scope_kind,scope_id,id,source,status,revision,created_at,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb) ON CONFLICT DO NOTHING`, value.Scope.Kind, value.Scope.ID, value.ID, value.Source, value.Status, value.Revision, value.CreatedAt, value.UpdatedAt, string(payload))
	if err != nil {
		return err
	}
	return expectPostgresEventSourceSubscriptionRow(result)
}

func (s *PostgresStore) GetEventSourceSubscription(ctx context.Context, scope Scope, id string) (*EventSourceSubscription, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("event_source_subscriptions")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEventSourceSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	var value EventSourceSubscription
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) ListEventSourceSubscriptions(ctx context.Context, filter EventSourceSubscriptionFilter) ([]*EventSourceSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("event_source_subscriptions")+` WHERE scope_kind=$1 AND scope_id=$2`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*EventSourceSubscription, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var item EventSourceSubscription
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, err
		}
		if matchesEventSourceSubscriptionFilter(&item, filter) {
			items = append(items, &item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paginateEventSourceSubscriptions(items, filter), nil
}

func (s *PostgresStore) UpdateEventSourceSubscription(ctx context.Context, value *EventSourceSubscription, expectedRevision int64) error {
	if value == nil || value.Validate() != nil || value.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceSubscription
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("event_source_subscriptions")+` SET source=$1,status=$2,revision=$3,updated_at=$4,payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND id=$8 AND revision=$9`, value.Source, value.Status, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if err != nil {
		return err
	}
	return expectPostgresEventSourceSubscriptionRow(result)
}

func (s *PostgresStore) GetEventSourceHealth(ctx context.Context, scope Scope, subscriptionID string) (*EventSourceHealth, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("event_source_health")+` WHERE scope_kind=$1 AND scope_id=$2 AND subscription_id=$3`, scope.Kind, scope.ID, subscriptionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value EventSourceHealth
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) SaveEventSourceHealth(ctx context.Context, value *EventSourceHealth, expectedRevision int64) error {
	if value == nil || value.Validate() != nil || value.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceHealth
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var result sql.Result
	if expectedRevision == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("event_source_health")+`(scope_kind,scope_id,subscription_id,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`, value.Scope.Kind, value.Scope.ID, value.SubscriptionID, value.Revision, value.UpdatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE `+s.table("event_source_health")+` SET revision=$1,updated_at=$2,payload=$3::jsonb WHERE scope_kind=$4 AND scope_id=$5 AND subscription_id=$6 AND revision=$7`, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.SubscriptionID, expectedRevision)
	}
	if err != nil {
		return err
	}
	return expectPostgresEventSourceSubscriptionRow(result)
}

func expectPostgresEventSourceSubscriptionRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrEventSourceSubscriptionConflict
	}
	return nil
}

var _ EventSourceSubscriptionStore = (*PostgresStore)(nil)
