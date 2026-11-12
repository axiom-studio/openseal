package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The $n placeholders are accepted by both the PostgreSQL and SQLite drivers.
func getCredentialRequest(ctx context.Context, db *sql.DB, table string, scope Scope, id string) (*CredentialRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidCredentialRequest
	}
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r CredentialRequest
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func listCredentialRequests(ctx context.Context, db *sql.DB, table string, scope Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	if scope.Validate() != nil || deploymentID == "" || conversationID == "" {
		return nil, ErrInvalidCredentialRequest
	}
	rows, err := db.QueryContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND conversation_id=$4 ORDER BY created_at,id`, scope.Kind, scope.ID, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*CredentialRequest{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r CredentialRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

func saveCredentialRequest(ctx context.Context, db *sql.DB, table string, r *CredentialRequest, expected int64) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Revision != expected+1 {
		return ErrInvalidCredentialRequest
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var result sql.Result
	if expected == 0 {
		result, err = db.ExecContext(ctx, `INSERT INTO `+table+`(scope_kind,scope_id,id,deployment_id,conversation_id,status,revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, r.Scope.Kind, r.Scope.ID, r.ID, r.DeploymentID, r.ConversationID, r.Status, r.Revision, r.CreatedAt, string(payload))
	} else {
		result, err = db.ExecContext(ctx, `UPDATE `+table+` SET status=$1,revision=$2,payload=$3 WHERE scope_kind=$4 AND scope_id=$5 AND id=$6 AND revision=$7 AND status='pending'`, r.Status, r.Revision, string(payload), r.Scope.Kind, r.Scope.ID, r.ID, expected)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCredentialRequestConflict
	}
	return nil
}
