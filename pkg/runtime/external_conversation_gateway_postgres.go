package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const externalConversationGatewayMigrationVersion int64 = 28

func (s *PostgresStore) migrateExternalConversationGateways(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("external_conversation_gateways")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			ingress_route TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			status TEXT NOT NULL, revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,id), CHECK (revision > 0)
		);
		CREATE INDEX IF NOT EXISTS external_conversation_gateways_scope_idx
			ON `+s.table("external_conversation_gateways")+` (scope_kind,scope_id,provider,status,updated_at DESC)
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'shared external conversation gateways')
		ON CONFLICT(version) DO NOTHING`, externalConversationGatewayMigrationVersion)
	return err
}

func (s *PostgresStore) CreateExternalConversationGateway(
	ctx context.Context,
	value *ExternalConversationGatewayRegistration,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("external_conversation_gateways")+`
		(scope_kind,scope_id,id,ingress_route,provider,status,revision,updated_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`,
		value.Gateway.Scope.Kind, value.Gateway.Scope.ID, value.ID, value.IngressRoute,
		value.Gateway.Provider, value.Status, value.Revision, value.UpdatedAt, string(payload))
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	return err
}

func (s *PostgresStore) GetExternalConversationGateway(
	ctx context.Context,
	scope Scope,
	id string,
) (*ExternalConversationGatewayRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanPostgresExternalConversationGateway(s.db.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_gateways")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) GetExternalConversationGatewayByIngressRoute(
	ctx context.Context,
	route string,
) (*ExternalConversationGatewayRegistration, error) {
	if !validOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, ErrInvalidExternalConversation
	}
	return scanPostgresExternalConversationGateway(s.db.QueryRowContext(ctx,
		`SELECT payload FROM `+s.table("external_conversation_gateways")+` WHERE ingress_route=$1`,
		strings.TrimSpace(route)))
}

func (s *PostgresStore) ListExternalConversationGateways(
	ctx context.Context,
	filter ExternalConversationGatewayFilter,
) ([]*ExternalConversationGatewayRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("external_conversation_gateways") + ` WHERE scope_kind=$1 AND scope_id=$2`
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
	result := make([]*ExternalConversationGatewayRegistration, 0)
	for rows.Next() {
		value, scanErr := scanPostgresExternalConversationGateway(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateExternalConversationGateway(
	ctx context.Context,
	value *ExternalConversationGatewayRegistration,
	expectedRevision int64,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("external_conversation_gateways")+`
		SET ingress_route=$1,provider=$2,status=$3,revision=$4,updated_at=$5,payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`,
		value.IngressRoute, value.Gateway.Provider, value.Status, value.Revision, value.UpdatedAt, string(payload),
		value.Gateway.Scope.Kind, value.Gateway.Scope.ID, value.ID, expectedRevision)
	if postgresUniqueViolation(err) {
		return ErrExternalConversationConflict
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		current, getErr := s.GetExternalConversationGateway(ctx, value.Gateway.Scope, value.ID)
		if getErr != nil {
			return getErr
		}
		if current == nil {
			return ErrExternalConversationGatewayNotFound
		}
		return ErrExternalConversationConflict
	}
	return nil
}

func scanPostgresExternalConversationGateway(
	scanner sqliteExternalConversationScanner,
) (*ExternalConversationGatewayRegistration, error) {
	var payload []byte
	if err := scanner.Scan(&payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationGatewayRegistration
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}
