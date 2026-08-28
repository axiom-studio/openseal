package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

const externalConversationTransportMigrationVersion int64 = 26

func (s *PostgresStore) migrateExternalConversations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("external_conversation_endpoints")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, provider TEXT NOT NULL,
			status TEXT NOT NULL, revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("external_conversation_inbox")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			endpoint_id TEXT NOT NULL, endpoint_revision BIGINT NOT NULL, provider_event_id TEXT NOT NULL,
			status TEXT NOT NULL, available_at TIMESTAMPTZ NOT NULL, lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, revision BIGINT NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id),
			UNIQUE (scope_kind,scope_id,endpoint_id,provider_event_id),
			CHECK (endpoint_revision > 0), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("external_conversation_mappings")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL,
			external_conversation_id TEXT NOT NULL, external_thread_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL, revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,endpoint_id,external_conversation_id,external_thread_id),
			UNIQUE (scope_kind,scope_id,endpoint_id,conversation_id,external_thread_id), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("external_participant_mappings")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL,
			external_participant_id TEXT NOT NULL, participant_type TEXT NOT NULL, participant_id TEXT NOT NULL,
			revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,endpoint_id,external_participant_id),
			UNIQUE (scope_kind,scope_id,endpoint_id,participant_type,participant_id), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("external_message_mappings")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, endpoint_id TEXT NOT NULL, direction TEXT NOT NULL,
			external_message_id TEXT NOT NULL, conversation_id TEXT NOT NULL, channel_message_id TEXT NOT NULL,
			revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,endpoint_id,direction,external_message_id),
			UNIQUE (scope_kind,scope_id,endpoint_id,direction,conversation_id,channel_message_id), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("external_conversation_deliveries")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			endpoint_id TEXT NOT NULL, endpoint_revision BIGINT NOT NULL, conversation_id TEXT NOT NULL,
			channel_message_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, status TEXT NOT NULL,
			available_at TIMESTAMPTZ NOT NULL, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL, revision BIGINT NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id),
			UNIQUE (scope_kind,scope_id,endpoint_id,idempotency_key),
			CHECK (endpoint_revision > 0), CHECK (revision > 0)
		)`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS external_conversation_endpoints_owner_idx ON ` + s.table("external_conversation_endpoints") + ` (scope_kind,scope_id,owner_type,owner_id,status,updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_endpoints_provider_idx ON ` + s.table("external_conversation_endpoints") + ` (scope_kind,scope_id,provider,status,updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_endpoints_gateway_idx ON ` + s.table("external_conversation_endpoints") + ` (provider,status)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_inbox_claim_idx ON ` + s.table("external_conversation_inbox") + ` (scope_kind,scope_id,status,available_at,lease_expires_at,created_at)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_inbox_endpoint_idx ON ` + s.table("external_conversation_inbox") + ` (scope_kind,scope_id,endpoint_id,created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_deliveries_claim_idx ON ` + s.table("external_conversation_deliveries") + ` (scope_kind,scope_id,status,available_at,lease_expires_at,created_at)`,
		`CREATE INDEX IF NOT EXISTS external_conversation_deliveries_conversation_idx ON ` + s.table("external_conversation_deliveries") + ` (scope_kind,scope_id,endpoint_id,conversation_id,created_at DESC)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'external conversation endpoints and transport')
		ON CONFLICT(version) DO NOTHING`, externalConversationTransportMigrationVersion)
	return err
}

func (s *PostgresStore) CreateExternalConversationEndpoint(ctx context.Context, endpoint *ExternalConversationEndpoint) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("external_conversation_endpoints")+`
		(scope_kind,scope_id,id,ingress_route,owner_type,owner_id,provider,status,revision,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute,
		endpoint.Owner.Type, endpoint.Owner.ID, endpoint.Provider, endpoint.Status,
		endpoint.Revision, endpoint.UpdatedAt, string(payload))
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *PostgresStore) GetExternalConversationEndpointByIngressRoute(ctx context.Context, route string) (*ExternalConversationEndpoint, error) {
	route = strings.TrimSpace(route)
	if !validOpaqueIdentifier(route, 128) {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_endpoints")+` WHERE ingress_route=$1`,
		route))
}

func (s *PostgresStore) GetExternalConversationEndpoint(ctx context.Context, scope Scope, id string) (*ExternalConversationEndpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationEndpoint(s.db.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_endpoints")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) ListExternalConversationEndpoints(ctx context.Context, filter ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("external_conversation_endpoints") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	if filter.Owner != nil {
		query += fmt.Sprintf(` AND owner_type=$%d AND owner_id=$%d`, placeholder, placeholder+1)
		args = append(args, filter.Owner.Type, filter.Owner.ID)
		placeholder += 2
	}
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

func (s *PostgresStore) ListExternalConversationEndpointsByVerifiedRoute(
	ctx context.Context,
	route ExternalConversationVerifiedRoute,
) ([]*ExternalConversationEndpoint, error) {
	if err := route.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("external_conversation_endpoints")+`
		WHERE provider=$1 AND status=$2
		  AND payload->>'installationId'=$3
		  AND (COALESCE((payload->>'installationWide')::boolean,false)
		       OR COALESCE(payload->>'address','')='' OR payload->>'address'=$4)
		  AND payload->'adapter'->>'skillId'=$5
		  AND payload->'adapter'->>'skillVersion'=$6
		  AND COALESCE(payload->'adapter'->>'sourceIdentity','')=$7
		  AND payload->'adapter'->>'adapterId'=$8
		  AND ($9='' OR payload->>'applicationId'=$9)
		ORDER BY scope_kind,scope_id,id`,
		route.Provider, ExternalConversationEndpointActive, route.InstallationID, route.Address,
		route.SkillID, route.SkillVersion, route.SourceIdentity, route.AdapterID, route.ApplicationID)
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

func (s *PostgresStore) UpdateExternalConversationEndpoint(ctx context.Context, endpoint *ExternalConversationEndpoint, expectedRevision int64) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("external_conversation_endpoints")+`
		SET status=$1,revision=$2,updated_at=$3,payload=$4::jsonb
		WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND ingress_route=$8 AND revision=$9`,
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

func (s *PostgresStore) ReceiveExternalConversationEvent(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationInboxItem, bool, error) {
	if err := item.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	endpoint, err := scanSQLiteExternalConversationEndpoint(tx.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_endpoints")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, false, ErrExternalConversationConflict
	}
	existing, err := scanSQLiteExternalConversationInbox(tx.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_inbox")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND endpoint_id=$3 AND provider_event_id=$4 FOR UPDATE`,
		item.Scope.Kind, item.Scope.ID, item.EndpointID, item.Event.ID))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !sameExternalConversationInboxIntent(existing, item) {
			return nil, false, ErrExternalConversationConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("external_conversation_inbox")+`
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,provider_event_id,status,available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)`,
		item.Scope.Kind, item.Scope.ID, item.ID, item.EndpointID, item.EndpointRevision, item.Event.ID,
		item.Status, item.AvailableAt, item.LeaseOwner, nullablePostgresTime(item.LeaseExpiresAt), item.CreatedAt, item.Revision, string(payload))
	if postgresUniqueViolation(err) {
		return nil, false, ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneExternalConversationInboxItem(item), false, nil
}

func (s *PostgresStore) GetExternalConversationInboxItem(ctx context.Context, scope Scope, id string) (*ExternalConversationInboxItem, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationInbox(s.db.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_inbox")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) ListExternalConversationInbox(ctx context.Context, filter ExternalConversationInboxFilter) ([]*ExternalConversationInboxItem, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("external_conversation_inbox") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	if filter.EndpointID != "" {
		query += fmt.Sprintf(` AND endpoint_id=$%d`, placeholder)
		args = append(args, filter.EndpointID)
		placeholder++
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "status", statuses)
	query += fmt.Sprintf(` ORDER BY created_at DESC,id ASC LIMIT $%d OFFSET $%d`, placeholder, placeholder+1)
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

func (s *PostgresStore) ClaimExternalConversationInbox(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationInboxItem, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := postgresConversationAdvisoryLock(ctx, tx, scope, "external-conversation-inbox-claim"); err != nil {
		return nil, err
	}
	item, err := scanSQLiteExternalConversationInbox(tx.QueryRowContext(ctx, `
		SELECT i.payload FROM `+s.table("external_conversation_inbox")+` i
		JOIN `+s.table("external_conversation_endpoints")+` e
		  ON e.scope_kind=i.scope_kind AND e.scope_id=i.scope_id AND e.id=i.endpoint_id
		WHERE i.scope_kind=$1 AND i.scope_id=$2 AND i.available_at<=$3
		  AND (i.status=ANY($4) OR (i.status=$5 AND i.lease_expires_at<=$3))
		  AND e.status=$6 AND e.revision=i.endpoint_revision
		  AND NOT EXISTS (
		    SELECT 1 FROM `+s.table("external_conversation_inbox")+` active
		    WHERE active.scope_kind=i.scope_kind AND active.scope_id=i.scope_id
		      AND active.endpoint_id=i.endpoint_id AND active.id<>i.id
		      AND active.status=$5 AND active.lease_expires_at>$3
		      AND active.payload->'event'->>'orderingKey'=i.payload->'event'->>'orderingKey'
		  )
		ORDER BY i.available_at ASC,i.created_at ASC,i.id ASC
		FOR UPDATE OF i SKIP LOCKED LIMIT 1`,
		scope.Kind, scope.ID, now, pq.Array([]string{
			string(ExternalConversationInboxPending), string(ExternalConversationInboxRetry),
		}), ExternalConversationInboxLeased, ExternalConversationEndpointActive))
	if err != nil || item == nil {
		if err == nil {
			err = tx.Commit()
		}
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("external_conversation_inbox")+`
		SET status=$1,lease_owner=$2,lease_expires_at=$3,revision=$4,payload=$5::jsonb
		WHERE scope_kind=$6 AND scope_id=$7 AND id=$8 AND revision=$9`,
		item.Status, item.LeaseOwner, item.LeaseExpiresAt, item.Revision, string(payload),
		item.Scope.Kind, item.Scope.ID, item.ID, item.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrExternalConversationConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *PostgresStore) SaveExternalConversationInbox(ctx context.Context, item *ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) error {
	if err := item.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanSQLiteExternalConversationInbox(tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_conversation_inbox")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, item.Scope.Kind, item.Scope.ID, item.ID))
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("external_conversation_inbox")+`
		SET status=$1,available_at=$2,lease_owner=$3,lease_expires_at=$4,revision=$5,payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10 AND status=$11 AND lease_owner=$12`,
		item.Status, item.AvailableAt, item.LeaseOwner, nullablePostgresTime(item.LeaseExpiresAt), item.Revision, string(payload),
		item.Scope.Kind, item.Scope.ID, item.ID, expectedRevision, ExternalConversationInboxLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrExternalConversationLeaseLost
	}
	return tx.Commit()
}

func (s *PostgresStore) GetExternalConversationMapping(ctx context.Context, scope Scope, endpointID, externalConversationID, externalThreadID string) (*ExternalConversationMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalConversationMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_conversation_mappings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND endpoint_id=$3 AND external_conversation_id=$4 AND external_thread_id=$5`,
		scope.Kind, scope.ID, endpointID, externalConversationID, externalThreadID))
}

func (s *PostgresStore) SaveExternalConversationMapping(ctx context.Context, mapping *ExternalConversationMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("external_conversation_mappings")+`
			(scope_kind,scope_id,endpoint_id,external_conversation_id,external_thread_id,conversation_id,revision,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalConversationID, mapping.ExternalThreadID, mapping.ConversationID, mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE `+s.table("external_conversation_mappings")+`
			SET revision=$1,updated_at=$2,payload=$3::jsonb
			WHERE scope_kind=$4 AND scope_id=$5 AND endpoint_id=$6 AND external_conversation_id=$7
			  AND external_thread_id=$8 AND conversation_id=$9 AND revision=$10`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalConversationID, mapping.ExternalThreadID, mapping.ConversationID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *PostgresStore) GetExternalParticipantMapping(ctx context.Context, scope Scope, endpointID, externalParticipantID string) (*ExternalParticipantMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalParticipantMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_participant_mappings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND endpoint_id=$3 AND external_participant_id=$4`,
		scope.Kind, scope.ID, endpointID, externalParticipantID))
}

func (s *PostgresStore) SaveExternalParticipantMapping(ctx context.Context, mapping *ExternalParticipantMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("external_participant_mappings")+`
			(scope_kind,scope_id,endpoint_id,external_participant_id,participant_type,participant_id,revision,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalParticipantID, mapping.Participant.Type, mapping.Participant.ID, mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE `+s.table("external_participant_mappings")+`
			SET revision=$1,updated_at=$2,payload=$3::jsonb
			WHERE scope_kind=$4 AND scope_id=$5 AND endpoint_id=$6 AND external_participant_id=$7
			  AND participant_type=$8 AND participant_id=$9 AND revision=$10`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.ExternalParticipantID, mapping.Participant.Type, mapping.Participant.ID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *PostgresStore) GetExternalMessageMapping(ctx context.Context, scope Scope, endpointID string, direction ExternalMessageDirection, externalMessageID string) (*ExternalMessageMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	return scanSQLiteExternalMessageMapping(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_message_mappings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND endpoint_id=$3 AND direction=$4 AND external_message_id=$5`,
		scope.Kind, scope.ID, endpointID, direction, externalMessageID))
}

func (s *PostgresStore) SaveExternalMessageMapping(ctx context.Context, mapping *ExternalMessageMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("external_message_mappings")+`
			(scope_kind,scope_id,endpoint_id,direction,external_message_id,conversation_id,channel_message_id,revision,updated_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.Direction, mapping.ExternalMessageID, mapping.ConversationID, mapping.ChannelMessageID,
			mapping.Revision, mapping.UpdatedAt, string(payload))
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE `+s.table("external_message_mappings")+`
			SET revision=$1,updated_at=$2,payload=$3::jsonb
			WHERE scope_kind=$4 AND scope_id=$5 AND endpoint_id=$6 AND direction=$7 AND external_message_id=$8
			  AND conversation_id=$9 AND channel_message_id=$10 AND revision=$11`,
			mapping.Revision, mapping.UpdatedAt, string(payload), mapping.Scope.Kind, mapping.Scope.ID, mapping.EndpointID,
			mapping.Direction, mapping.ExternalMessageID, mapping.ConversationID, mapping.ChannelMessageID, expectedRevision)
		err = updateErr
		if err == nil {
			if rows, _ := result.RowsAffected(); rows != 1 {
				err = ErrExternalConversationConflict
			}
		}
	}
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *PostgresStore) EnqueueExternalConversationDelivery(ctx context.Context, delivery *ExternalConversationDelivery) (*ExternalConversationDelivery, bool, error) {
	if err := delivery.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	endpoint, err := scanSQLiteExternalConversationEndpoint(tx.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_endpoints")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID))
	if err != nil {
		return nil, false, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != delivery.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, delivery.Adapter) {
		return nil, false, ErrExternalConversationConflict
	}
	existing, err := scanSQLiteExternalConversationDelivery(tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_conversation_deliveries")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND endpoint_id=$3 AND idempotency_key=$4 FOR UPDATE`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.EndpointID, delivery.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if sameExternalConversationDeliveryIntent(existing, delivery) {
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
		if sameExternalConversationDeliveryEffect(existing, delivery) && existing.Status == ExternalConversationDeliveryDelivered {
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
		rebound, ok := rebindExternalConversationDelivery(existing, delivery)
		if !ok {
			return nil, false, ErrExternalConversationConflict
		}
		payload, marshalErr := json.Marshal(rebound)
		if marshalErr != nil {
			return nil, false, marshalErr
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE `+s.table("external_conversation_deliveries")+`
			SET endpoint_revision=$1,status=$2,available_at=$3,lease_owner=$4,lease_expires_at=$5,revision=$6,payload=$7::jsonb
			WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`,
			rebound.EndpointRevision, rebound.Status, rebound.AvailableAt, rebound.LeaseOwner,
			nullablePostgresTime(rebound.LeaseExpiresAt), rebound.Revision, string(payload),
			rebound.Scope.Kind, rebound.Scope.ID, rebound.ID, existing.Revision)
		if updateErr != nil {
			return nil, false, updateErr
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return nil, false, ErrExternalConversationConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return rebound, true, nil
	}
	payload, err := json.Marshal(delivery)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("external_conversation_deliveries")+`
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,conversation_id,channel_message_id,idempotency_key,status,
		 available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb)`,
		delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.EndpointID, delivery.EndpointRevision,
		delivery.ConversationID, delivery.ChannelMessageID, delivery.IdempotencyKey, delivery.Status,
		delivery.AvailableAt, delivery.LeaseOwner, nullablePostgresTime(delivery.LeaseExpiresAt),
		delivery.CreatedAt, delivery.Revision, string(payload))
	if postgresUniqueViolation(err) {
		return nil, false, ErrExternalConversationConflict
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneExternalConversationDelivery(delivery), false, nil
}

func (s *PostgresStore) GetExternalConversationDelivery(ctx context.Context, scope Scope, id string) (*ExternalConversationDelivery, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationDelivery(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_conversation_deliveries")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) ListExternalConversationDeliveries(ctx context.Context, filter ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("external_conversation_deliveries") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	if filter.EndpointID != "" {
		query += fmt.Sprintf(` AND endpoint_id=$%d`, placeholder)
		args = append(args, filter.EndpointID)
		placeholder++
	}
	if filter.ConversationID != "" {
		query += fmt.Sprintf(` AND conversation_id=$%d`, placeholder)
		args = append(args, filter.ConversationID)
		placeholder++
	}
	if filter.CorrelationKind != "" {
		query += fmt.Sprintf(` AND payload->'correlation'->>'kind'=$%d`, placeholder)
		args = append(args, filter.CorrelationKind)
		placeholder++
	}
	if filter.CorrelationID != "" {
		query += fmt.Sprintf(` AND payload->'correlation'->>'id'=$%d`, placeholder)
		args = append(args, filter.CorrelationID)
		placeholder++
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "status", statuses)
	query += fmt.Sprintf(` ORDER BY created_at DESC,id ASC LIMIT $%d OFFSET $%d`, placeholder, placeholder+1)
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

func (s *PostgresStore) ClaimExternalConversationDelivery(ctx context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationDelivery, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := postgresConversationAdvisoryLock(ctx, tx, scope, "external-conversation-delivery-claim"); err != nil {
		return nil, err
	}
	delivery, err := scanSQLiteExternalConversationDelivery(tx.QueryRowContext(ctx, `
		SELECT d.payload FROM `+s.table("external_conversation_deliveries")+` d
		JOIN `+s.table("external_conversation_endpoints")+` e
		  ON e.scope_kind=d.scope_kind AND e.scope_id=d.scope_id AND e.id=d.endpoint_id
		WHERE d.scope_kind=$1 AND d.scope_id=$2 AND d.available_at<=$3
		  AND (d.status=ANY($4) OR (d.status=$5 AND d.lease_expires_at<=$3))
		  AND e.status=$6 AND e.revision=d.endpoint_revision
		  AND NOT EXISTS (
		    SELECT 1 FROM `+s.table("external_conversation_deliveries")+` active
		    WHERE active.scope_kind=d.scope_kind AND active.scope_id=d.scope_id
		      AND active.endpoint_id=d.endpoint_id AND active.id<>d.id
		      AND active.status=$5 AND active.lease_expires_at>$3
		      AND active.payload->>'orderingKey'=d.payload->>'orderingKey'
		  )
		ORDER BY d.available_at ASC,d.created_at ASC,d.id ASC
		FOR UPDATE OF d SKIP LOCKED LIMIT 1`,
		scope.Kind, scope.ID, now, pq.Array([]string{
			string(ExternalConversationDeliveryPending), string(ExternalConversationDeliveryRetry),
		}), ExternalConversationDeliveryLeased, ExternalConversationEndpointActive))
	if err != nil || delivery == nil {
		if err == nil {
			err = tx.Commit()
		}
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("external_conversation_deliveries")+`
		SET status=$1,lease_owner=$2,lease_expires_at=$3,revision=$4,payload=$5::jsonb
		WHERE scope_kind=$6 AND scope_id=$7 AND id=$8 AND revision=$9`,
		delivery.Status, delivery.LeaseOwner, delivery.LeaseExpiresAt, delivery.Revision, string(payload),
		delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.Revision-1)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, ErrExternalConversationConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return delivery, nil
}

func (s *PostgresStore) SaveExternalConversationDelivery(ctx context.Context, delivery *ExternalConversationDelivery, expectedRevision int64, leaseOwner string) error {
	if err := delivery.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanSQLiteExternalConversationDelivery(tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("external_conversation_deliveries")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, delivery.Scope.Kind, delivery.Scope.ID, delivery.ID))
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("external_conversation_deliveries")+`
		SET status=$1,available_at=$2,lease_owner=$3,lease_expires_at=$4,revision=$5,payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10 AND status=$11 AND lease_owner=$12`,
		delivery.Status, delivery.AvailableAt, delivery.LeaseOwner, nullablePostgresTime(delivery.LeaseExpiresAt),
		delivery.Revision, string(payload), delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, expectedRevision,
		ExternalConversationDeliveryLeased, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrExternalConversationLeaseLost
	}
	return tx.Commit()
}

func nullablePostgresTime(value time.Time) interface{} {
	if value.IsZero() {
		return nil
	}
	return value
}

var (
	_ ExternalConversationEndpointStore  = (*PostgresStore)(nil)
	_ ExternalConversationTransportStore = (*PostgresStore)(nil)
	_ ExternalConversationEndpointStore  = (*SQLiteStore)(nil)
	_ ExternalConversationTransportStore = (*SQLiteStore)(nil)
)
