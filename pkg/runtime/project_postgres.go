package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (s *PostgresStore) migrateProjects(ctx context.Context, tx *sql.Tx) error {
	if _, e := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("projects")+` (id TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,revision BIGINT NOT NULL,updated_at TIMESTAMPTZ NOT NULL,idempotency_key_hash TEXT NOT NULL DEFAULT '',payload JSONB NOT NULL,PRIMARY KEY(scope_kind,scope_id,id),CHECK(revision>0)); CREATE UNIQUE INDEX IF NOT EXISTS projects_idempotency_idx ON `+s.table("projects")+`(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash<>''; CREATE INDEX IF NOT EXISTS projects_scope_idx ON `+s.table("projects")+`(scope_kind,scope_id,status,updated_at DESC); CREATE INDEX IF NOT EXISTS projects_owner_idx ON `+s.table("projects")+`(scope_kind,scope_id,owner_type,owner_id,updated_at DESC)`); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name)VALUES(14,'durable projects')ON CONFLICT(version)DO NOTHING`)
	return e
}
func (s *PostgresStore) CreateProjectWithEvent(ctx context.Context, i *Project, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("projects")+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, i.IdempotencyKeyHash, string(b)); err != nil {
		return nil, err
	}
	p, err := s.insertPostgresActivityTx(ctx, tx, e)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *PostgresStore) GetProjectByIdempotency(ctx context.Context, scope Scope, key string) (*Project, error) {
	var p []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("projects")+` WHERE scope_kind=$1 AND scope_id=$2 AND idempotency_key_hash=$3`, scope.Kind, scope.ID, key).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	var i Project
	err = json.Unmarshal(p, &i)
	return &i, err
}
func (s *PostgresStore) CreateProject(ctx context.Context, i *Project) error {
	if e := i.Validate(); e != nil {
		return e
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO `+s.table("projects")+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, i.IdempotencyKeyHash, string(b))
	return e
}
func (s *PostgresStore) UpdateProjectWithEvent(ctx context.Context, i *Project, expected int64, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE `+s.table("projects")+` SET status=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
	if err != nil {
		return nil, err
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, ErrProjectConflict
	}
	p, err := s.insertPostgresActivityTx(ctx, tx, e)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *PostgresStore) GetProject(ctx context.Context, scope Scope, id string) (*Project, error) {
	if e := scope.Validate(); e != nil {
		return nil, e
	}
	var p []byte
	e := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("projects")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&p)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if e != nil {
		return nil, e
	}
	var i Project
	e = json.Unmarshal(p, &i)
	return &i, e
}
func (s *PostgresStore) ListProjects(ctx context.Context, f ProjectFilter) ([]*Project, error) {
	if e := f.Scope.Validate(); e != nil {
		return nil, e
	}
	q := `SELECT payload FROM ` + s.table("projects") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{f.Scope.Kind, f.Scope.ID}
	next := 3
	if f.Owner != nil {
		q += fmt.Sprintf(` AND owner_type=$%d AND owner_id=$%d`, next, next+1)
		args = append(args, f.Owner.Type, f.Owner.ID)
		next += 2
	}
	if len(f.Statuses) > 0 {
		marks := make([]string, len(f.Statuses))
		for x, v := range f.Statuses {
			marks[x] = fmt.Sprintf("$%d", next)
			next++
			args = append(args, v)
		}
		q += ` AND status IN (` + strings.Join(marks, ",") + `)`
	}
	if f.ObjectiveID != "" {
		q += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(payload->'objectiveRefs') value WHERE value=$%d)`, next)
		args = append(args, f.ObjectiveID)
		next++
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += fmt.Sprintf(` ORDER BY updated_at DESC,id ASC LIMIT $%d OFFSET $%d`, next, next+1)
	args = append(args, limit, f.Offset)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []*Project{}
	for rows.Next() {
		var p []byte
		if e = rows.Scan(&p); e != nil {
			return nil, e
		}
		var i Project
		if e = json.Unmarshal(p, &i); e != nil {
			return nil, e
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}
func (s *PostgresStore) UpdateProject(ctx context.Context, i *Project, expected int64) error {
	if e := i.Validate(); e != nil {
		return e
	}
	if i.Revision != expected+1 {
		return ErrProjectConflict
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	r, e := s.db.ExecContext(ctx, `UPDATE `+s.table("projects")+` SET owner_type=$1,owner_id=$2,status=$3,revision=$4,updated_at=$5,payload=$6::jsonb WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrProjectConflict
	}
	return nil
}

var _ ProjectStore = (*PostgresStore)(nil)
