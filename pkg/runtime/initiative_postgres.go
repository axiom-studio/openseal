package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *PostgresStore) migrateInitiatives(ctx context.Context, tx *sql.Tx) error {
	if _, e := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("initiatives")+` (id TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,revision BIGINT NOT NULL,updated_at TIMESTAMPTZ NOT NULL,idempotency_key_hash TEXT NOT NULL DEFAULT '',payload JSONB NOT NULL,PRIMARY KEY(scope_kind,scope_id,id),CHECK(revision>0)); CREATE UNIQUE INDEX IF NOT EXISTS initiatives_idempotency_idx ON `+s.table("initiatives")+`(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash<>''; CREATE INDEX IF NOT EXISTS initiatives_scope_idx ON `+s.table("initiatives")+`(scope_kind,scope_id,status,updated_at DESC)`); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name)VALUES(13,'durable initiatives')ON CONFLICT(version)DO NOTHING`)
	return e
}
func (s *PostgresStore) CreateInitiativeWithEvent(ctx context.Context, i *Initiative, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("initiatives")+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, i.IdempotencyKeyHash, string(b)); err != nil {
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
func (s *PostgresStore) GetInitiativeByIdempotency(ctx context.Context, scope Scope, key string) (*Initiative, error) {
	var p []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("initiatives")+` WHERE scope_kind=$1 AND scope_id=$2 AND idempotency_key_hash=$3`, scope.Kind, scope.ID, key).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInitiativeNotFound
	}
	if err != nil {
		return nil, err
	}
	var i Initiative
	err = json.Unmarshal(p, &i)
	return &i, err
}
func (s *PostgresStore) CreateInitiative(ctx context.Context, i *Initiative) error {
	if e := i.Validate(); e != nil {
		return e
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO `+s.table("initiatives")+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, string(b))
	return e
}
func (s *PostgresStore) UpdateInitiativeWithEvent(ctx context.Context, i *Initiative, expected int64, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE `+s.table("initiatives")+` SET status=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
	if err != nil {
		return nil, err
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, ErrInitiativeConflict
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
func (s *PostgresStore) GetInitiative(ctx context.Context, scope Scope, id string) (*Initiative, error) {
	if e := scope.Validate(); e != nil {
		return nil, e
	}
	var p []byte
	e := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("initiatives")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&p)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrInitiativeNotFound
	}
	if e != nil {
		return nil, e
	}
	var i Initiative
	e = json.Unmarshal(p, &i)
	return &i, e
}
func (s *PostgresStore) ListInitiatives(ctx context.Context, f InitiativeFilter) ([]*Initiative, error) {
	if e := f.Scope.Validate(); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("initiatives")+` WHERE scope_kind=$1 AND scope_id=$2 ORDER BY updated_at DESC`, f.Scope.Kind, f.Scope.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []*Initiative{}
	for rows.Next() {
		var p []byte
		if e = rows.Scan(&p); e != nil {
			return nil, e
		}
		var i Initiative
		if e = json.Unmarshal(p, &i); e != nil {
			return nil, e
		}
		if f.Owner != nil && i.Owner != *f.Owner {
			continue
		}
		if len(f.Statuses) > 0 && !initiativeStatusContains(f.Statuses, i.Status) {
			continue
		}
		if f.ObjectiveID != "" && !containsString(i.ObjectiveRefs, f.ObjectiveID) {
			continue
		}
		out = append(out, &i)
	}
	start := f.Offset
	if start > len(out) {
		start = len(out)
	}
	end := len(out)
	if f.Limit > 0 && start+f.Limit < end {
		end = start + f.Limit
	}
	return out[start:end], rows.Err()
}
func (s *PostgresStore) UpdateInitiative(ctx context.Context, i *Initiative, expected int64) error {
	if e := i.Validate(); e != nil {
		return e
	}
	if i.Revision != expected+1 {
		return ErrInitiativeConflict
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	r, e := s.db.ExecContext(ctx, `UPDATE `+s.table("initiatives")+` SET owner_type=$1,owner_id=$2,status=$3,revision=$4,updated_at=$5,payload=$6::jsonb WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrInitiativeConflict
	}
	return nil
}

var _ InitiativeStore = (*PostgresStore)(nil)
