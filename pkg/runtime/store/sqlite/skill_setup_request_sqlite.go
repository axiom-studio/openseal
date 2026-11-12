package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
)

func migrateSkillSetupRequests(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS skill_setup_requests (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, deployment_id TEXT NOT NULL, conversation_id TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(scope_kind,scope_id,id))`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS skill_setup_requests_chat_idx ON skill_setup_requests(scope_kind,scope_id,conversation_id,created_at)`)
	return err
}
func (s *Store) GetSkillSetupRequest(ctx context.Context, scope models.Scope, id string) (*models.SkillSetupRequest, error) {
	if scope.Validate() != nil {
		return nil, models.ErrInvalidSkillSetup
	}
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM skill_setup_requests WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r models.SkillSetupRequest
	err = json.Unmarshal(payload, &r)
	return &r, err
}
func (s *Store) ListSkillSetupRequests(ctx context.Context, scope models.Scope, deploymentID, conversationID string) ([]*models.SkillSetupRequest, error) {
	if scope.Validate() != nil || conversationID == "" {
		return nil, models.ErrInvalidSkillSetup
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM skill_setup_requests WHERE scope_kind=? AND scope_id=? AND (?='' OR deployment_id=?) AND conversation_id=? ORDER BY created_at,id`, scope.Kind, scope.ID, deploymentID, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*models.SkillSetupRequest{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r models.SkillSetupRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}
func (s *Store) SaveSkillSetupRequest(ctx context.Context, r *models.SkillSetupRequest, expected int64) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Revision != expected+1 {
		return models.NewSkillSetupValidationError("revision", "must advance the expected request revision by one")
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var result sql.Result
	if expected == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO skill_setup_requests(scope_kind,scope_id,id,deployment_id,conversation_id,status,revision,created_at,payload) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, r.Scope.Kind, r.Scope.ID, r.ID, r.DeploymentID, r.ConversationID, r.Status, r.Revision, r.CreatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE skill_setup_requests SET status=?,revision=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND status='pending'`, r.Status, r.Revision, string(payload), r.Scope.Kind, r.Scope.ID, r.ID, expected)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return models.ErrSkillSetupConflict
	}
	return nil
}
