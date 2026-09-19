package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *PostgresStore) migrateSkillSetupRequests(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("skill_setup_requests")+` (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, deployment_id TEXT NOT NULL, conversation_id TEXT NOT NULL, status TEXT NOT NULL, revision BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL, PRIMARY KEY(scope_kind,scope_id,id))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS skill_setup_requests_conversation_idx ON `+s.table("skill_setup_requests")+`(scope_kind,scope_id,deployment_id,conversation_id,created_at)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES(45,'durable Skill setup requests') ON CONFLICT(version) DO NOTHING`)
	return err
}
func (s *PostgresStore) GetSkillSetupRequest(ctx context.Context, scope Scope, id string) (*SkillSetupRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidSkillSetup
	}
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_setup_requests")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r SkillSetupRequest
	err = json.Unmarshal(payload, &r)
	return &r, err
}
func (s *PostgresStore) ListSkillSetupRequests(ctx context.Context, scope Scope, deploymentID, conversationID string) ([]*SkillSetupRequest, error) {
	if scope.Validate() != nil || deploymentID == "" || conversationID == "" {
		return nil, ErrInvalidSkillSetup
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("skill_setup_requests")+` WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND conversation_id=$4 ORDER BY created_at,id`, scope.Kind, scope.ID, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*SkillSetupRequest{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r SkillSetupRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}
func (s *PostgresStore) SaveSkillSetupRequest(ctx context.Context, r *SkillSetupRequest, expected int64) error {
	if r.Validate() != nil || r.Revision != expected+1 {
		return ErrInvalidSkillSetup
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var result sql.Result
	if expected == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("skill_setup_requests")+`(scope_kind,scope_id,id,deployment_id,conversation_id,status,revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb) ON CONFLICT DO NOTHING`, r.Scope.Kind, r.Scope.ID, r.ID, r.DeploymentID, r.ConversationID, r.Status, r.Revision, r.CreatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE `+s.table("skill_setup_requests")+` SET status=$1,revision=$2,payload=$3::jsonb WHERE scope_kind=$4 AND scope_id=$5 AND id=$6 AND revision=$7 AND status='pending'`, r.Status, r.Revision, string(payload), r.Scope.Kind, r.Scope.ID, r.ID, expected)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSkillSetupConflict
	}
	return nil
}
