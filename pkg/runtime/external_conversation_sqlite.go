package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func migrateExternalConversations(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS external_conversation_endpoints (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			ingress_route TEXT NOT NULL,
			owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, provider TEXT NOT NULL,
			status TEXT NOT NULL, revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_endpoints_owner
			ON external_conversation_endpoints(scope_kind, scope_id, owner_type, owner_id, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_endpoints_provider
			ON external_conversation_endpoints(scope_kind, scope_id, provider, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_endpoints_gateway
			ON external_conversation_endpoints(provider, status);

		CREATE TABLE IF NOT EXISTS external_conversation_inbox (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			endpoint_id TEXT NOT NULL, endpoint_revision INTEGER NOT NULL, provider_event_id TEXT NOT NULL,
			status TEXT NOT NULL, available_at DATETIME NOT NULL, lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at DATETIME, created_at DATETIME NOT NULL, revision INTEGER NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, endpoint_id, provider_event_id)
		);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_inbox_claim
			ON external_conversation_inbox(scope_kind, scope_id, status, available_at, lease_expires_at, created_at);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_inbox_endpoint
			ON external_conversation_inbox(scope_kind, scope_id, endpoint_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS external_conversation_mappings (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL,
			external_conversation_id TEXT NOT NULL, external_thread_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL, revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, endpoint_id, external_conversation_id, external_thread_id),
			UNIQUE (scope_kind, scope_id, endpoint_id, conversation_id, external_thread_id)
		);
		CREATE TABLE IF NOT EXISTS external_participant_mappings (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL,
			external_participant_id TEXT NOT NULL, participant_type TEXT NOT NULL, participant_id TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, endpoint_id, external_participant_id),
			UNIQUE (scope_kind, scope_id, endpoint_id, participant_type, participant_id)
		);
		CREATE TABLE IF NOT EXISTS external_message_mappings (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL, direction TEXT NOT NULL,
			external_message_id TEXT NOT NULL, conversation_id TEXT NOT NULL, channel_message_id TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, endpoint_id, direction, external_message_id),
			UNIQUE (scope_kind, scope_id, endpoint_id, direction, conversation_id, channel_message_id)
		);

		CREATE TABLE IF NOT EXISTS external_conversation_deliveries (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			endpoint_id TEXT NOT NULL, endpoint_revision INTEGER NOT NULL, conversation_id TEXT NOT NULL,
			channel_message_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, status TEXT NOT NULL,
			available_at DATETIME NOT NULL, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at DATETIME,
			created_at DATETIME NOT NULL, revision INTEGER NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, endpoint_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_deliveries_claim
			ON external_conversation_deliveries(scope_kind, scope_id, status, available_at, lease_expires_at, created_at);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_deliveries_conversation
			ON external_conversation_deliveries(scope_kind, scope_id, endpoint_id, conversation_id, created_at DESC);
	`); err != nil {
		return err
	}
	return migrateExternalConversationIngressRoutesSQLite(db)
}

func migrateExternalConversationIngressRoutesSQLite(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(external_conversation_endpoints)`)
	if err != nil {
		return err
	}
	hasRoute := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		hasRoute = hasRoute || name == "ingress_route"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasRoute {
		if _, err := db.Exec(`ALTER TABLE external_conversation_endpoints ADD COLUMN ingress_route TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	existing, err := db.Query(`SELECT scope_kind,scope_id,id,payload FROM external_conversation_endpoints WHERE ingress_route=''`)
	if err != nil {
		return err
	}
	type backfill struct{ scopeKind, scopeID, id, route, payload string }
	updates := make([]backfill, 0)
	for existing.Next() {
		var item backfill
		if err := existing.Scan(&item.scopeKind, &item.scopeID, &item.id, &item.payload); err != nil {
			existing.Close()
			return err
		}
		var endpoint ExternalConversationEndpoint
		if err := json.Unmarshal([]byte(item.payload), &endpoint); err != nil {
			existing.Close()
			return err
		}
		item.route = uuid.NewString()
		endpoint.IngressRoute = item.route
		encoded, err := json.Marshal(&endpoint)
		if err != nil {
			existing.Close()
			return err
		}
		item.payload = string(encoded)
		updates = append(updates, item)
	}
	if err := existing.Close(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range updates {
		if _, err := tx.Exec(`UPDATE external_conversation_endpoints SET ingress_route=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND ingress_route=''`,
			item.route, item.payload, item.scopeKind, item.scopeID, item.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_external_conversation_endpoints_ingress_route
		ON external_conversation_endpoints(ingress_route)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) CreateExternalConversationEndpoint(ctx context.Context, endpoint *ExternalConversationEndpoint) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO external_conversation_endpoints
		(scope_kind,scope_id,id,ingress_route,owner_type,owner_id,provider,status,revision,updated_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute,
		endpoint.Owner.Type, endpoint.Owner.ID, endpoint.Provider, endpoint.Status,
		endpoint.Revision, endpoint.UpdatedAt, string(payload))
	if sqliteUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalConversationEndpointByIngressRoute(ctx context.Context, route string) (*ExternalConversationEndpoint, error) {
	route = strings.TrimSpace(route)
	if !validOpaqueIdentifier(route, 128) {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE ingress_route=?`, route))
}

func (s *SQLiteStore) GetExternalConversationEndpoint(ctx context.Context, scope Scope, id string) (*ExternalConversationEndpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationEndpoints(ctx context.Context, filter ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		query += ` AND owner_type=? AND owner_id=?`
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
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
	result := make([]*ExternalConversationEndpoint, 0)
	for rows.Next() {
		endpoint, scanErr := scanSQLiteExternalConversationEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, endpoint)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ListExternalConversationEndpointsByVerifiedRoute(
	ctx context.Context,
	route ExternalConversationVerifiedRoute,
) ([]*ExternalConversationEndpoint, error) {
	if err := route.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM external_conversation_endpoints
		WHERE provider=? AND status=? ORDER BY scope_kind,scope_id,id`,
		route.Provider, ExternalConversationEndpointActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ExternalConversationEndpoint, 0)
	for rows.Next() {
		endpoint, scanErr := scanSQLiteExternalConversationEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if externalConversationEndpointMatchesVerifiedRoute(endpoint, route) {
			result = append(result, endpoint)
		}
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateExternalConversationEndpoint(ctx context.Context, endpoint *ExternalConversationEndpoint, expectedRevision int64) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_conversation_endpoints
		SET status=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND ingress_route=? AND revision=?`,
		endpoint.Status, endpoint.Revision, endpoint.UpdatedAt, string(payload),
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute, expectedRevision)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrExternalConversationConflict
	}
	return nil
}

func (s *SQLiteStore) ReceiveExternalConversationEvent(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationInboxItem, bool, error) {
	if err := item.Validate(); err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	endpoint, err := scanSQLiteExternalConversationEndpoint(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision || endpoint.Adapter != item.Adapter {
		return nil, false, ErrExternalConversationConflict
	}
	existing, err := scanSQLiteExternalConversationInbox(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox
		 WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND provider_event_id=?`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID, item.Event.ID))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !sameExternalConversationInboxIntent(existing, item) {
			return nil, false, ErrExternalConversationConflict
		}
		if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, false, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO external_conversation_inbox
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,provider_event_id,status,available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.Scope.Kind, item.Scope.ID, item.ID, item.EndpointID, item.EndpointRevision, item.Event.ID,
		item.Status, item.AvailableAt, item.LeaseOwner, nullableSQLiteTime(item.LeaseExpiresAt), item.CreatedAt, item.Revision, string(payload))
	if sqliteUniqueViolation(err) {
		return nil, false, ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, false, err
	}
	return cloneExternalConversationInboxItem(item), false, nil
}

func (s *SQLiteStore) GetExternalConversationInboxItem(ctx context.Context, scope Scope, id string) (*ExternalConversationInboxItem, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationInbox(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox WHERE scope_kind=? AND scope_id=? AND id=?`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationInbox(ctx context.Context, filter ExternalConversationInboxFilter) ([]*ExternalConversationInboxItem, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM external_conversation_inbox WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.EndpointID != "" {
		query += ` AND endpoint_id=?`
		args = append(args, filter.EndpointID)
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY created_at DESC,id ASC LIMIT ? OFFSET ?`
	limit, offset := normalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ExternalConversationInboxItem, 0)
	for rows.Next() {
		item, scanErr := scanSQLiteExternalConversationInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ClaimExternalConversationInbox(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationInboxItem, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	item, err := scanSQLiteExternalConversationInbox(conn.QueryRowContext(ctx, `
		SELECT i.payload FROM external_conversation_inbox i
		JOIN external_conversation_endpoints e
		  ON e.scope_kind=i.scope_kind AND e.scope_id=i.scope_id AND e.id=i.endpoint_id
		WHERE i.scope_kind=? AND i.scope_id=? AND i.available_at<=?
		  AND (i.status IN (?,?) OR (i.status=? AND i.lease_expires_at<=?))
		  AND e.status=? AND e.revision=i.endpoint_revision
		  AND NOT EXISTS (
		    SELECT 1 FROM external_conversation_inbox active
		    WHERE active.scope_kind=i.scope_kind AND active.scope_id=i.scope_id
		      AND active.endpoint_id=i.endpoint_id AND active.id<>i.id
		      AND active.status=? AND active.lease_expires_at>?
		      AND json_extract(active.payload,'$.event.orderingKey')=json_extract(i.payload,'$.event.orderingKey')
		  )
		ORDER BY i.available_at ASC,i.created_at ASC,i.id ASC LIMIT 1`,
		scope.Kind, scope.ID, now,
		ExternalConversationInboxPending, ExternalConversationInboxRetry,
		ExternalConversationInboxLeased, now, ExternalConversationEndpointActive,
		ExternalConversationInboxLeased, now))
	if err != nil || item == nil {
		return nil, err
	}
	item.Status = ExternalConversationInboxLeased
	item.Attempt++
	item.LeaseOwner = strings.TrimSpace(worker)
	item.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	item.Revision++
	item.UpdatedAt = now.UTC()
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE external_conversation_inbox
		SET status=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
		item.Status, item.LeaseOwner, item.LeaseExpiresAt, item.Revision, string(payload),
		item.Scope.Kind, item.Scope.ID, item.ID, item.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrExternalConversationConflict
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *SQLiteStore) SaveExternalConversationInbox(ctx context.Context, item *ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) error {
	if err := item.Validate(); err != nil {
		return err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	current, err := scanSQLiteExternalConversationInbox(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox WHERE scope_kind=? AND scope_id=? AND id=?`,
		item.Scope.Kind, item.Scope.ID, item.ID))
	if err != nil {
		return err
	}
	if current == nil {
		return ErrExternalConversationInboxNotFound
	}
	if !validExternalConversationInboxSave(current, item, expectedRevision, leaseOwner) {
		return ErrExternalConversationLeaseLost
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE external_conversation_inbox
		SET status=?,available_at=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=? AND lease_owner=?`,
		item.Status, item.AvailableAt, item.LeaseOwner, nullableSQLiteTime(item.LeaseExpiresAt), item.Revision, string(payload),
		item.Scope.Kind, item.Scope.ID, item.ID, expectedRevision, ExternalConversationInboxLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrExternalConversationLeaseLost
	}
	return commitSQLiteConn(ctx, conn, &committed)
}

func (s *SQLiteStore) GetExternalConversationMapping(ctx context.Context, scope Scope, endpointID, externalConversationID, externalThreadID string) (*ExternalConversationMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalConversationMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_conversation_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_conversation_id=? AND external_thread_id=?`,
		scope.Kind, scope.ID, endpointID, externalConversationID, externalThreadID))
}

func (s *SQLiteStore) SaveExternalConversationMapping(ctx context.Context, mapping *ExternalConversationMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO external_conversation_mappings
			(scope_kind,scope_id,endpoint_id,external_conversation_id,external_thread_id,conversation_id,revision,updated_at,payload)
			VALUES(?,?,?,?,?,?,?,?,?)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalConversationID, mapping.ExternalThreadID, mapping.ConversationID, mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE external_conversation_mappings SET revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_conversation_id=? AND external_thread_id=? AND conversation_id=? AND revision=?`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalConversationID, mapping.ExternalThreadID, mapping.ConversationID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalParticipantMapping(ctx context.Context, scope Scope, endpointID, externalParticipantID string) (*ExternalParticipantMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalParticipantMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_participant_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_participant_id=?`,
		scope.Kind, scope.ID, endpointID, externalParticipantID))
}

func (s *SQLiteStore) SaveExternalParticipantMapping(ctx context.Context, mapping *ExternalParticipantMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO external_participant_mappings
			(scope_kind,scope_id,endpoint_id,external_participant_id,participant_type,participant_id,revision,updated_at,payload)
			VALUES(?,?,?,?,?,?,?,?,?)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalParticipantID, mapping.Participant.Type, mapping.Participant.ID, mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE external_participant_mappings SET revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_participant_id=?
			  AND participant_type=? AND participant_id=? AND revision=?`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalParticipantID, mapping.Participant.Type, mapping.Participant.ID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalMessageMapping(ctx context.Context, scope Scope, endpointID string, direction ExternalMessageDirection, externalMessageID string) (*ExternalMessageMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalMessageMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_message_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND direction=? AND external_message_id=?`,
		scope.Kind, scope.ID, endpointID, direction, externalMessageID))
}

func (s *SQLiteStore) SaveExternalMessageMapping(ctx context.Context, mapping *ExternalMessageMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO external_message_mappings
			(scope_kind,scope_id,endpoint_id,direction,external_message_id,conversation_id,channel_message_id,revision,updated_at,payload)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID, mapping.Direction,
			mapping.ExternalMessageID, mapping.ConversationID, mapping.ChannelMessageID, mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE external_message_mappings SET revision=?,updated_at=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND direction=? AND external_message_id=?
			  AND conversation_id=? AND channel_message_id=? AND revision=?`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.Direction, mapping.ExternalMessageID, mapping.ConversationID, mapping.ChannelMessageID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) EnqueueExternalConversationDelivery(ctx context.Context, delivery *ExternalConversationDelivery) (*ExternalConversationDelivery, bool, error) {
	if err := delivery.Validate(); err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	endpoint, err := scanSQLiteExternalConversationEndpoint(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != delivery.EndpointRevision || endpoint.Adapter != delivery.Adapter {
		return nil, false, ErrExternalConversationConflict
	}
	existing, err := scanSQLiteExternalConversationDelivery(conn.QueryRowContext(ctx, `SELECT payload FROM external_conversation_deliveries
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND idempotency_key=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID, delivery.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !sameExternalConversationDeliveryIntent(existing, delivery) {
			return nil, false, ErrExternalConversationConflict
		}
		if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}
	payload, err := json.Marshal(delivery)
	if err != nil {
		return nil, false, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO external_conversation_deliveries
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,conversation_id,channel_message_id,idempotency_key,status,
		 available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.EndpointID,
		delivery.EndpointRevision, delivery.ConversationID, delivery.ChannelMessageID, delivery.IdempotencyKey, delivery.Status,
		delivery.AvailableAt, delivery.LeaseOwner, nullableSQLiteTime(delivery.LeaseExpiresAt), delivery.CreatedAt, delivery.Revision, string(payload))
	if sqliteUniqueViolation(err) {
		return nil, false, ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, false, err
	}
	return cloneExternalConversationDelivery(delivery), false, nil
}

func (s *SQLiteStore) GetExternalConversationDelivery(ctx context.Context, scope Scope, id string) (*ExternalConversationDelivery, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationDelivery(s.db.QueryRowContext(ctx, `SELECT payload FROM external_conversation_deliveries
		WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationDeliveries(ctx context.Context, filter ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM external_conversation_deliveries WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.EndpointID != "" {
		query += ` AND endpoint_id=?`
		args = append(args, filter.EndpointID)
	}
	if filter.ConversationID != "" {
		query += ` AND conversation_id=?`
		args = append(args, filter.ConversationID)
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY created_at DESC,id ASC LIMIT ? OFFSET ?`
	limit, offset := normalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ExternalConversationDelivery, 0)
	for rows.Next() {
		delivery, scanErr := scanSQLiteExternalConversationDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, delivery)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ClaimExternalConversationDelivery(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationDelivery, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	delivery, err := scanSQLiteExternalConversationDelivery(conn.QueryRowContext(ctx, `
		SELECT d.payload FROM external_conversation_deliveries d
		JOIN external_conversation_endpoints e
		  ON e.scope_kind=d.scope_kind AND e.scope_id=d.scope_id AND e.id=d.endpoint_id
		WHERE d.scope_kind=? AND d.scope_id=? AND d.available_at<=?
		  AND (d.status IN (?,?) OR (d.status=? AND d.lease_expires_at<=?))
		  AND e.status=? AND e.revision=d.endpoint_revision
		  AND NOT EXISTS (
		    SELECT 1 FROM external_conversation_deliveries active
		    WHERE active.scope_kind=d.scope_kind AND active.scope_id=d.scope_id
		      AND active.endpoint_id=d.endpoint_id AND active.id<>d.id
		      AND active.status=? AND active.lease_expires_at>?
		      AND json_extract(active.payload,'$.orderingKey')=json_extract(d.payload,'$.orderingKey')
		  )
		ORDER BY d.available_at ASC,d.created_at ASC,d.id ASC LIMIT 1`,
		scope.Kind, scope.ID, now,
		ExternalConversationDeliveryPending, ExternalConversationDeliveryRetry,
		ExternalConversationDeliveryLeased, now, ExternalConversationEndpointActive,
		ExternalConversationDeliveryLeased, now))
	if err != nil || delivery == nil {
		return nil, err
	}
	delivery.Status = ExternalConversationDeliveryLeased
	delivery.Attempt++
	delivery.LeaseOwner = strings.TrimSpace(worker)
	delivery.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	delivery.Revision++
	delivery.UpdatedAt = now.UTC()
	payload, err := json.Marshal(delivery)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE external_conversation_deliveries
		SET status=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
		delivery.Status, delivery.LeaseOwner, delivery.LeaseExpiresAt, delivery.Revision, string(payload),
		delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrExternalConversationConflict
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, err
	}
	return delivery, nil
}

func (s *SQLiteStore) SaveExternalConversationDelivery(ctx context.Context, delivery *ExternalConversationDelivery, expectedRevision int64, leaseOwner string) error {
	if err := delivery.Validate(); err != nil {
		return err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	current, err := scanSQLiteExternalConversationDelivery(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_deliveries WHERE scope_kind=? AND scope_id=? AND id=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.ID))
	if err != nil {
		return err
	}
	if current == nil {
		return ErrExternalConversationDeliveryNotFound
	}
	if !validExternalConversationDeliverySave(current, delivery, expectedRevision, leaseOwner) {
		return ErrExternalConversationLeaseLost
	}
	payload, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE external_conversation_deliveries
		SET status=?,available_at=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=? AND lease_owner=?`,
		delivery.Status, delivery.AvailableAt, delivery.LeaseOwner, nullableSQLiteTime(delivery.LeaseExpiresAt),
		delivery.Revision, string(payload), delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, expectedRevision,
		ExternalConversationDeliveryLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrExternalConversationLeaseLost
	}
	return commitSQLiteConn(ctx, conn, &committed)
}

type sqliteExternalConversationScanner interface {
	Scan(...interface{}) error
}

func scanSQLiteExternalConversationEndpoint(scanner sqliteExternalConversationScanner) (*ExternalConversationEndpoint, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationEndpoint
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationInbox(scanner sqliteExternalConversationScanner) (*ExternalConversationInboxItem, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationInboxItem
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationMapping(scanner sqliteExternalConversationScanner) (*ExternalConversationMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalParticipantMapping(scanner sqliteExternalConversationScanner) (*ExternalParticipantMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalParticipantMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalMessageMapping(scanner sqliteExternalConversationScanner) (*ExternalMessageMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalMessageMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationDelivery(scanner sqliteExternalConversationScanner) (*ExternalConversationDelivery, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationDelivery
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func commitSQLiteConn(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	*committed = true
	return nil
}

func nullableSQLiteTime(value time.Time) interface{} {
	if value.IsZero() {
		return nil
	}
	return value
}

func sqliteUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func normalizeExternalConversationPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func validExternalConversationInboxSave(current, next *ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) bool {
	return current != nil && next != nil && current.Revision == expectedRevision && next.Revision == expectedRevision+1 &&
		current.Status == ExternalConversationInboxLeased && current.LeaseOwner == strings.TrimSpace(leaseOwner) &&
		!next.UpdatedAt.Before(current.UpdatedAt) && !next.UpdatedAt.After(current.LeaseExpiresAt) &&
		current.EndpointID == next.EndpointID && current.EndpointRevision == next.EndpointRevision &&
		current.Adapter == next.Adapter && sameExternalConversationInboxIntent(current, next) &&
		current.CreatedAt.Equal(next.CreatedAt)
}

func validExternalConversationDeliverySave(current, next *ExternalConversationDelivery, expectedRevision int64, leaseOwner string) bool {
	return current != nil && next != nil && current.Revision == expectedRevision && next.Revision == expectedRevision+1 &&
		current.Status == ExternalConversationDeliveryLeased && current.LeaseOwner == strings.TrimSpace(leaseOwner) &&
		!next.UpdatedAt.Before(current.UpdatedAt) && !next.UpdatedAt.After(current.LeaseExpiresAt) &&
		sameExternalConversationDeliveryIntent(current, next) && current.CreatedAt.Equal(next.CreatedAt)
}
