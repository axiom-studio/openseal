package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
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

		CREATE TABLE IF NOT EXISTS external_conversation_gateways (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			ingress_route TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			status TEXT NOT NULL, revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_external_conversation_gateways_scope
			ON external_conversation_gateways(scope_kind, scope_id, provider, status, updated_at DESC);

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
		var endpoint models.ExternalConversationEndpoint
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

func (s *SQLiteStore) CreateExternalConversationEndpoint(ctx context.Context, endpoint *models.ExternalConversationEndpoint) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	err := insertSQLiteExternalConversationEndpoint(ctx, s.db, endpoint)
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalConversationEndpointByIngressRoute(ctx context.Context, route string) (*models.ExternalConversationEndpoint, error) {
	route = strings.TrimSpace(route)
	if !models.IsValidOpaqueIdentifier(route, 128) {
		return nil, models.ErrInvalidExternalConversation
	}
	return sqlstore.ScanExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE ingress_route=?`, route))
}

func (s *SQLiteStore) GetExternalConversationEndpoint(ctx context.Context, scope models.Scope, id string) (*models.ExternalConversationEndpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return sqlstore.ScanExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationEndpoints(ctx context.Context, filter models.ExternalConversationEndpointFilter) ([]*models.ExternalConversationEndpoint, error) {
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
	limit, offset := models.NormalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ExternalConversationEndpoint, 0)
	for rows.Next() {
		endpoint, scanErr := sqlstore.ScanExternalConversationEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, endpoint)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ListExternalConversationEndpointsByVerifiedRoute(
	ctx context.Context,
	route models.ExternalConversationVerifiedRoute,
) ([]*models.ExternalConversationEndpoint, error) {
	if err := route.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM external_conversation_endpoints
		WHERE provider=? AND status=? ORDER BY scope_kind,scope_id,id`,
		route.Provider, models.ExternalConversationEndpointActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ExternalConversationEndpoint, 0)
	for rows.Next() {
		endpoint, scanErr := sqlstore.ScanExternalConversationEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if models.ExternalConversationEndpointMatchesVerifiedRoute(endpoint, route) {
			result = append(result, endpoint)
		}
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateExternalConversationEndpoint(ctx context.Context, endpoint *models.ExternalConversationEndpoint, expectedRevision int64) error {
	return s.updateExternalConversationEndpoint(ctx, s.db, endpoint, expectedRevision)
}

func (s *SQLiteStore) updateExternalConversationEndpoint(ctx context.Context, executor sqlstore.Execer, endpoint *models.ExternalConversationEndpoint, expectedRevision int64) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	result, err := executor.ExecContext(ctx, `UPDATE external_conversation_endpoints
		SET status=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND ingress_route=? AND revision=?`,
		endpoint.Status, endpoint.Revision, endpoint.UpdatedAt, string(payload),
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute, expectedRevision)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return models.ErrExternalConversationConflict
	}
	return nil
}

func (s *SQLiteStore) ReceiveExternalConversationEvent(ctx context.Context, item *models.ExternalConversationInboxItem) (*models.ExternalConversationInboxItem, bool, error) {
	if err := item.Validate(); err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	endpoint, err := sqlstore.ScanExternalConversationEndpoint(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != models.ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!models.ExternalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, false, models.ErrExternalConversationConflict
	}
	existing, err := sqlstore.ScanExternalConversationInbox(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox
		 WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND provider_event_id=?`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID, item.Event.ID))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !models.SameExternalConversationInboxIntent(existing, item) {
			return nil, false, models.ErrExternalConversationConflict
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
		return nil, false, models.ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, false, err
	}
	return models.CloneExternalConversationInboxItem(item), false, nil
}

func (s *SQLiteStore) GetExternalConversationInboxItem(ctx context.Context, scope models.Scope, id string) (*models.ExternalConversationInboxItem, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return sqlstore.ScanExternalConversationInbox(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox WHERE scope_kind=? AND scope_id=? AND id=?`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationInbox(ctx context.Context, filter models.ExternalConversationInboxFilter) ([]*models.ExternalConversationInboxItem, error) {
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
	if filter.ReplyDueAt != nil {
		query += ` AND json_extract(payload,'$.replyState') IS NULL AND (json_extract(payload,'$.replyAvailableAt') IS NULL OR julianday(json_extract(payload,'$.replyAvailableAt')) <= julianday(?))`
		args = append(args, filter.ReplyDueAt.UTC().Format(time.RFC3339Nano))
	}
	query += ` ORDER BY created_at DESC,id ASC LIMIT ? OFFSET ?`
	limit, offset := models.NormalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ExternalConversationInboxItem, 0)
	for rows.Next() {
		item, scanErr := sqlstore.ScanExternalConversationInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ClaimExternalConversationInbox(ctx context.Context, scope models.Scope, worker string, now time.Time, leaseDuration time.Duration) (*models.ExternalConversationInboxItem, error) {
	if scope.Validate() != nil || !models.IsValidOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, models.ErrInvalidExternalConversation
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	item, err := sqlstore.ScanExternalConversationInbox(conn.QueryRowContext(ctx, `
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
		models.ExternalConversationInboxPending, models.ExternalConversationInboxRetry,
		models.ExternalConversationInboxLeased, now, models.ExternalConversationEndpointActive,
		models.ExternalConversationInboxLeased, now))
	if err != nil || item == nil {
		return nil, err
	}
	item.Status = models.ExternalConversationInboxLeased
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
		return nil, models.ErrExternalConversationConflict
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *SQLiteStore) SaveExternalConversationInbox(ctx context.Context, item *models.ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) error {
	if err := item.Validate(); err != nil {
		return err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	current, err := sqlstore.ScanExternalConversationInbox(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_inbox WHERE scope_kind=? AND scope_id=? AND id=?`,
		item.Scope.Kind, item.Scope.ID, item.ID))
	if err != nil {
		return err
	}
	if current == nil {
		return models.ErrExternalConversationInboxNotFound
	}
	if !models.IsValidExternalConversationInboxSave(current, item, expectedRevision, leaseOwner) {
		return models.ErrExternalConversationLeaseLost
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE external_conversation_inbox
		SET status=?,available_at=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=? AND lease_owner=?`,
		item.Status, item.AvailableAt, item.LeaseOwner, nullableSQLiteTime(item.LeaseExpiresAt), item.Revision, string(payload),
		item.Scope.Kind, item.Scope.ID, item.ID, expectedRevision, models.ExternalConversationInboxLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return models.ErrExternalConversationLeaseLost
	}
	return commitSQLiteConn(ctx, conn, &committed)
}

func (s *SQLiteStore) GetExternalConversationMapping(ctx context.Context, scope models.Scope, endpointID, externalConversationID, externalThreadID string) (*models.ExternalConversationMapping, error) {
	if scope.Validate() != nil {
		return nil, models.ErrInvalidExternalConversation
	}
	return sqlstore.ScanExternalConversationMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_conversation_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_conversation_id=? AND external_thread_id=?`,
		scope.Kind, scope.ID, endpointID, externalConversationID, externalThreadID))
}

func (s *SQLiteStore) SaveExternalConversationMapping(ctx context.Context, mapping *models.ExternalConversationMapping, expectedRevision int64) error {
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
				err = models.ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalParticipantMapping(ctx context.Context, scope models.Scope, endpointID, externalParticipantID string) (*models.ExternalParticipantMapping, error) {
	if scope.Validate() != nil {
		return nil, models.ErrInvalidExternalConversation
	}
	return sqlstore.ScanExternalParticipantMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_participant_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND external_participant_id=?`,
		scope.Kind, scope.ID, endpointID, externalParticipantID))
}

func (s *SQLiteStore) SaveExternalParticipantMapping(ctx context.Context, mapping *models.ExternalParticipantMapping, expectedRevision int64) error {
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
				err = models.ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) GetExternalMessageMapping(ctx context.Context, scope models.Scope, endpointID string, direction models.ExternalMessageDirection, externalMessageID string) (*models.ExternalMessageMapping, error) {
	if scope.Validate() != nil {
		return nil, models.ErrInvalidExternalConversation
	}
	return sqlstore.ScanExternalMessageMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM external_message_mappings
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND direction=? AND external_message_id=?`,
		scope.Kind, scope.ID, endpointID, direction, externalMessageID))
}

func (s *SQLiteStore) SaveExternalMessageMapping(ctx context.Context, mapping *models.ExternalMessageMapping, expectedRevision int64) error {
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
				err = models.ErrExternalConversationConflict
			}
		}
	}
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
	}
	return err
}

func (s *SQLiteStore) EnqueueExternalConversationDelivery(ctx context.Context, delivery *models.ExternalConversationDelivery) (*models.ExternalConversationDelivery, bool, error) {
	if err := delivery.Validate(); err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	endpoint, err := sqlstore.ScanExternalConversationEndpoint(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_endpoints WHERE scope_kind=? AND scope_id=? AND id=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != models.ExternalConversationEndpointActive ||
		endpoint.Revision != delivery.EndpointRevision ||
		!models.ExternalConversationAdapterBelongsToEndpoint(endpoint.Adapter, delivery.Adapter) {
		return nil, false, models.ErrExternalConversationConflict
	}
	existing, err := sqlstore.ScanExternalConversationDelivery(conn.QueryRowContext(ctx, `SELECT payload FROM external_conversation_deliveries
		WHERE scope_kind=? AND scope_id=? AND endpoint_id=? AND idempotency_key=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID, delivery.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if models.SameExternalConversationDeliveryIntent(existing, delivery) {
			if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
		if models.SameExternalConversationDeliveryEffect(existing, delivery) && existing.Status == models.ExternalConversationDeliveryDelivered {
			if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
		rebound, ok := models.RebindExternalConversationDelivery(existing, delivery)
		if !ok {
			return nil, false, models.ErrExternalConversationConflict
		}
		payload, marshalErr := json.Marshal(rebound)
		if marshalErr != nil {
			return nil, false, marshalErr
		}
		result, updateErr := conn.ExecContext(ctx, `UPDATE external_conversation_deliveries
			SET endpoint_revision=?,status=?,available_at=?,lease_owner=?,lease_expires_at=?,revision=?,payload=?
			WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			rebound.EndpointRevision, rebound.Status, rebound.AvailableAt, rebound.LeaseOwner,
			nullableSQLiteTime(rebound.LeaseExpiresAt), rebound.Revision, string(payload),
			rebound.Scope.Kind, rebound.Scope.ID, rebound.ID, existing.Revision)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return nil, false, models.ErrExternalConversationConflict
		}
		if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
			return nil, false, err
		}
		return rebound, true, nil
	}
	err = insertSQLiteExternalConversationDelivery(ctx, conn, delivery)
	if sqliteUniqueViolation(err) {
		return nil, false, models.ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, false, err
	}
	return models.CloneExternalConversationDelivery(delivery), false, nil
}

func (s *SQLiteStore) GetExternalConversationDelivery(ctx context.Context, scope models.Scope, id string) (*models.ExternalConversationDelivery, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return sqlstore.ScanExternalConversationDelivery(s.db.QueryRowContext(ctx, `SELECT payload FROM external_conversation_deliveries
		WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) ListExternalConversationDeliveries(ctx context.Context, filter models.ExternalConversationDeliveryFilter) ([]*models.ExternalConversationDelivery, error) {
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
	if filter.CorrelationKind != "" {
		query += ` AND json_extract(payload,'$.correlation.kind')=?`
		args = append(args, filter.CorrelationKind)
	}
	if filter.CorrelationID != "" {
		query += ` AND json_extract(payload,'$.correlation.id')=?`
		args = append(args, filter.CorrelationID)
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY created_at DESC,id ASC LIMIT ? OFFSET ?`
	limit, offset := models.NormalizeExternalConversationPage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ExternalConversationDelivery, 0)
	for rows.Next() {
		delivery, scanErr := sqlstore.ScanExternalConversationDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, delivery)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ClaimExternalConversationDelivery(ctx context.Context, scope models.Scope, worker string, now time.Time, leaseDuration time.Duration) (*models.ExternalConversationDelivery, error) {
	if scope.Validate() != nil || !models.IsValidOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, models.ErrInvalidExternalConversation
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	delivery, err := sqlstore.ScanExternalConversationDelivery(conn.QueryRowContext(ctx, `
		SELECT d.payload FROM external_conversation_deliveries d
		JOIN external_conversation_endpoints e
		  ON e.scope_kind=d.scope_kind AND e.scope_id=d.scope_id AND e.id=d.endpoint_id
		WHERE d.scope_kind=? AND d.scope_id=? AND d.available_at<=?
		  AND (d.status IN (?,?) OR (d.status=? AND d.lease_expires_at<=?))
		  AND e.status=? AND e.revision>=d.endpoint_revision
		  AND NOT EXISTS (
		    SELECT 1 FROM external_conversation_deliveries active
		    WHERE active.scope_kind=d.scope_kind AND active.scope_id=d.scope_id
		      AND active.endpoint_id=d.endpoint_id AND active.id<>d.id
		      AND active.status=? AND active.lease_expires_at>?
		      AND json_extract(active.payload,'$.orderingKey')=json_extract(d.payload,'$.orderingKey')
		  )
		ORDER BY d.available_at ASC,d.created_at ASC,d.id ASC LIMIT 1`,
		scope.Kind, scope.ID, now,
		models.ExternalConversationDeliveryPending, models.ExternalConversationDeliveryRetry,
		models.ExternalConversationDeliveryLeased, now, models.ExternalConversationEndpointActive,
		models.ExternalConversationDeliveryLeased, now))
	if err != nil || delivery == nil {
		return nil, err
	}
	delivery.Status = models.ExternalConversationDeliveryLeased
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
		return nil, models.ErrExternalConversationConflict
	}
	if err := commitSQLiteConn(ctx, conn, &committed); err != nil {
		return nil, err
	}
	return delivery, nil
}

func (s *SQLiteStore) SaveExternalConversationDelivery(ctx context.Context, delivery *models.ExternalConversationDelivery, expectedRevision int64, leaseOwner string) error {
	if err := delivery.Validate(); err != nil {
		return err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	current, err := sqlstore.ScanExternalConversationDelivery(conn.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_deliveries WHERE scope_kind=? AND scope_id=? AND id=?`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.ID))
	if err != nil {
		return err
	}
	if current == nil {
		return models.ErrExternalConversationDeliveryNotFound
	}
	if !models.IsValidExternalConversationDeliverySave(current, delivery, expectedRevision, leaseOwner) {
		return models.ErrExternalConversationLeaseLost
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
		models.ExternalConversationDeliveryLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return models.ErrExternalConversationLeaseLost
	}
	return commitSQLiteConn(ctx, conn, &committed)
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

func (s *SQLiteStore) SaveExternalConversationReplyProgress(ctx context.Context, scope models.Scope, id string, expectedRevision int64, progress models.ExternalConversationReplyProgress) error {
	if scope.Validate() != nil {
		return models.ErrInvalidExternalConversation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := sqlstore.ScanExternalConversationInbox(tx.QueryRowContext(ctx, `SELECT payload FROM external_conversation_inbox
		WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
	if err != nil {
		return err
	}
	next, err := models.ApplyExternalConversationReplyProgress(current, expectedRevision, progress)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE external_conversation_inbox SET revision=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status=?`,
		next.Revision, string(payload), scope.Kind, scope.ID, next.ID, expectedRevision, models.ExternalConversationInboxApplied)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return models.ErrExternalConversationConflict
	}
	return tx.Commit()
}

func insertSQLiteExternalConversationEndpoint(ctx context.Context, exec sqliteExecer, endpoint *models.ExternalConversationEndpoint) error {
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO external_conversation_endpoints
		(scope_kind,scope_id,id,ingress_route,owner_type,owner_id,provider,status,revision,updated_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute,
		endpoint.Owner.Type, endpoint.Owner.ID, endpoint.Provider, endpoint.Status,
		endpoint.Revision, endpoint.UpdatedAt, string(payload))
	return err
}

func insertSQLiteExternalConversationDelivery(ctx context.Context, exec sqliteExecer, delivery *models.ExternalConversationDelivery) error {
	payload, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO external_conversation_deliveries
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,conversation_id,channel_message_id,idempotency_key,status,
		 available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.EndpointID,
		delivery.EndpointRevision, delivery.ConversationID, delivery.ChannelMessageID, delivery.IdempotencyKey, delivery.Status,
		delivery.AvailableAt, delivery.LeaseOwner, nullableSQLiteTime(delivery.LeaseExpiresAt), delivery.CreatedAt, delivery.Revision, string(payload))
	return err
}

var (
	_ models.ExternalConversationEndpointStore  = (*SQLiteStore)(nil)
	_ models.ExternalConversationTransportStore = (*SQLiteStore)(nil)
)
