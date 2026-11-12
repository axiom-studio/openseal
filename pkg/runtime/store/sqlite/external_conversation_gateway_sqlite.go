package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *Store) CreateExternalConversationGateway(
	ctx context.Context,
	value *models.ExternalConversationGatewayRegistration,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO external_conversation_gateways
		(scope_kind,scope_id,id,ingress_route,provider,status,revision,updated_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		value.Gateway.Scope.Kind, value.Gateway.Scope.ID, value.ID, value.IngressRoute,
		value.Gateway.Provider, value.Status, value.Revision, value.UpdatedAt, string(payload))
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
	}
	return err
}

func (s *Store) GetExternalConversationGateway(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.ExternalConversationGatewayRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanSQLiteExternalConversationGateway(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_gateways WHERE scope_kind=? AND scope_id=? AND id=?`,
		scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *Store) GetExternalConversationGatewayByIngressRoute(
	ctx context.Context,
	route string,
) (*models.ExternalConversationGatewayRegistration, error) {
	if !models.IsValidOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, models.ErrInvalidExternalConversation
	}
	return scanSQLiteExternalConversationGateway(s.db.QueryRowContext(ctx,
		`SELECT payload FROM external_conversation_gateways WHERE ingress_route=?`, strings.TrimSpace(route)))
}

func (s *Store) ListExternalConversationGateways(
	ctx context.Context,
	filter models.ExternalConversationGatewayFilter,
) ([]*models.ExternalConversationGatewayRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM external_conversation_gateways WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Provider != "" {
		query += ` AND provider=?`
		args = append(args, filter.Provider)
	}
	if filter.DeploymentID != "" {
		query += ` AND json_extract(payload,'$.gateway.deploymentId')=?`
		args = append(args, filter.DeploymentID)
	}
	if filter.Owner != nil {
		query += ` AND json_extract(payload,'$.gateway.owner.type')=? AND json_extract(payload,'$.gateway.owner.id')=?`
		args = append(args, string(filter.Owner.Type), filter.Owner.ID)
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
	result := make([]*models.ExternalConversationGatewayRegistration, 0)
	for rows.Next() {
		value, scanErr := scanSQLiteExternalConversationGateway(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpdateExternalConversationGateway(
	ctx context.Context,
	value *models.ExternalConversationGatewayRegistration,
	expectedRevision int64,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_conversation_gateways
		SET ingress_route=?,provider=?,status=?,revision=?,updated_at=?,payload=?
		WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
		value.IngressRoute, value.Gateway.Provider, value.Status, value.Revision, value.UpdatedAt, string(payload),
		value.Gateway.Scope.Kind, value.Gateway.Scope.ID, value.ID, expectedRevision)
	if sqliteUniqueViolation(err) {
		return models.ErrExternalConversationConflict
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
			return models.ErrExternalConversationGatewayNotFound
		}
		return models.ErrExternalConversationConflict
	}
	return nil
}

func scanSQLiteExternalConversationGateway(
	scanner sqlstore.RowScanner,
) (*models.ExternalConversationGatewayRegistration, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalConversationGatewayRegistration
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}
