package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

func migrateEventSourceSubscriptions(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS event_source_subscriptions (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, source TEXT NOT NULL,
			status TEXT NOT NULL, revision INTEGER NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS event_source_subscriptions_scope_status_idx ON event_source_subscriptions(scope_kind, scope_id, status, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS event_source_health (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, subscription_id TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, subscription_id)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) CreateEventSourceSubscription(ctx context.Context, value *EventSourceSubscription) error {
	if value == nil || value.Validate() != nil {
		return ErrInvalidEventSourceSubscription
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO event_source_subscriptions(scope_kind,scope_id,id,source,status,revision,created_at,updated_at,payload) VALUES(?,?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, value.ID, value.Source, value.Status, value.Revision, value.CreatedAt, value.UpdatedAt, string(payload))
	if err != nil {
		return err
	}
	return expectEventSourceSubscriptionRow(result)
}

func (s *SQLiteStore) GetEventSourceSubscription(ctx context.Context, scope Scope, id string) (*EventSourceSubscription, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM event_source_subscriptions WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEventSourceSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	var value EventSourceSubscription
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) ListEventSourceSubscriptions(ctx context.Context, filter EventSourceSubscriptionFilter) ([]*EventSourceSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM event_source_subscriptions WHERE scope_kind=? AND scope_id=?`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*EventSourceSubscription, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var item EventSourceSubscription
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
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

func (s *SQLiteStore) UpdateEventSourceSubscription(ctx context.Context, value *EventSourceSubscription, expectedRevision int64) error {
	if value == nil || value.Validate() != nil || value.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceSubscription
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE event_source_subscriptions SET source=?,status=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, value.Source, value.Status, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if err != nil {
		return err
	}
	return expectEventSourceSubscriptionRow(result)
}

func (s *SQLiteStore) GetEventSourceHealth(ctx context.Context, scope Scope, subscriptionID string) (*EventSourceHealth, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM event_source_health WHERE scope_kind=? AND scope_id=? AND subscription_id=?`, scope.Kind, scope.ID, subscriptionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value EventSourceHealth
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) SaveEventSourceHealth(ctx context.Context, value *EventSourceHealth, expectedRevision int64) error {
	if value == nil || value.Validate() != nil || value.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceHealth
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var result sql.Result
	if expectedRevision == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO event_source_health(scope_kind,scope_id,subscription_id,revision,updated_at,payload) VALUES(?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, value.SubscriptionID, value.Revision, value.UpdatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE event_source_health SET revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND subscription_id=? AND revision=?`, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.SubscriptionID, expectedRevision)
	}
	if err != nil {
		return err
	}
	return expectEventSourceSubscriptionRow(result)
}

func expectEventSourceSubscriptionRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrEventSourceSubscriptionConflict
	}
	return nil
}

func paginateEventSourceSubscriptions(items []*EventSourceSubscription, filter EventSourceSubscriptionFilter) []*EventSourceSubscription {
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	start := filter.Offset
	if start > len(items) {
		start = len(items)
	}
	end := len(items)
	if filter.Limit > 0 && start+filter.Limit < end {
		end = start + filter.Limit
	}
	return items[start:end]
}

var _ EventSourceSubscriptionStore = (*SQLiteStore)(nil)
