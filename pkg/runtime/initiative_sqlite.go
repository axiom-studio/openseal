package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func migrateInitiatives(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS initiatives (id TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,revision INTEGER NOT NULL,updated_at DATETIME NOT NULL,idempotency_key_hash TEXT NOT NULL DEFAULT '',payload TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,id))`); err != nil {
		return err
	}
	rows, err := db.Query(`PRAGMA table_info(initiatives)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def interface{}
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "idempotency_key_hash" {
			found = true
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if !found {
		if _, err = db.Exec(`ALTER TABLE initiatives ADD COLUMN idempotency_key_hash TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		if _, err = db.Exec(`UPDATE initiatives SET idempotency_key_hash=COALESCE(json_extract(payload,'$.idempotencyKeyHash'),'')`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_initiatives_scope ON initiatives(scope_kind,scope_id,status,updated_at); CREATE INDEX IF NOT EXISTS idx_initiatives_owner ON initiatives(scope_kind,scope_id,owner_type,owner_id,updated_at); CREATE UNIQUE INDEX IF NOT EXISTS idx_initiatives_idempotency ON initiatives(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash<>''`)
	return err
}
func (s *SQLiteStore) CreateInitiativeWithEvent(ctx context.Context, i *Initiative, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, e, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO initiatives(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload)VALUES(?,?,?,?,?,?,?,?,?,?)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, i.IdempotencyKeyHash, string(b))
		return err
	})
}
func (s *SQLiteStore) GetInitiativeByIdempotency(ctx context.Context, scope Scope, key string) (*Initiative, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM initiatives WHERE scope_kind=? AND scope_id=? AND idempotency_key_hash=?`, scope.Kind, scope.ID, key).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInitiativeNotFound
	}
	if err != nil {
		return nil, err
	}
	var i Initiative
	err = json.Unmarshal([]byte(p), &i)
	return &i, err
}
func (s *SQLiteStore) CreateInitiative(ctx context.Context, i *Initiative) error {
	if err := i.Validate(); err != nil {
		return err
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO initiatives(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload)VALUES(?,?,?,?,?,?,?,?,?,?)`, i.ID, i.Scope.Kind, i.Scope.ID, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, i.IdempotencyKeyHash, string(b))
	return e
}
func (s *SQLiteStore) UpdateInitiativeWithEvent(ctx context.Context, i *Initiative, expected int64, e *ActivityEvent) (*ActivityEvent, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, e, func(c *sql.Conn) error {
		r, err := c.ExecContext(ctx, `UPDATE initiatives SET owner_type=?,owner_id=?,status=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrInitiativeConflict
		}
		return nil
	})
}
func (s *SQLiteStore) GetInitiative(ctx context.Context, scope Scope, id string) (*Initiative, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var p string
	e := s.db.QueryRowContext(ctx, `SELECT payload FROM initiatives WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, id).Scan(&p)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrInitiativeNotFound
	}
	if e != nil {
		return nil, e
	}
	var i Initiative
	e = json.Unmarshal([]byte(p), &i)
	return &i, e
}
func (s *SQLiteStore) ListInitiatives(ctx context.Context, f InitiativeFilter) ([]*Initiative, error) {
	if err := f.Scope.Validate(); err != nil {
		return nil, err
	}
	q := `SELECT payload FROM initiatives WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{f.Scope.Kind, f.Scope.ID}
	if f.Owner != nil {
		q += ` AND owner_type=? AND owner_id=?`
		args = append(args, f.Owner.Type, f.Owner.ID)
	}
	if len(f.Statuses) > 0 {
		q += ` AND status IN (` + strings.TrimRight(strings.Repeat("?,", len(f.Statuses)), ",") + `)`
		for _, v := range f.Statuses {
			args = append(args, v)
		}
	}
	if f.ObjectiveID != "" {
		q += ` AND EXISTS (SELECT 1 FROM json_each(initiatives.payload,'$.objectiveRefs') WHERE value=?)`
		args = append(args, f.ObjectiveID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += ` ORDER BY updated_at DESC,id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, f.Offset)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []*Initiative{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			return nil, e
		}
		var i Initiative
		if e = json.Unmarshal([]byte(p), &i); e != nil {
			return nil, e
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) UpdateInitiative(ctx context.Context, i *Initiative, expected int64) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if i.Revision != expected+1 {
		return ErrInitiativeConflict
	}
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	r, e := s.db.ExecContext(ctx, `UPDATE initiatives SET owner_type=?,owner_id=?,status=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`, i.Owner.Type, i.Owner.ID, i.Status, i.Revision, i.UpdatedAt, string(b), i.Scope.Kind, i.Scope.ID, i.ID, expected)
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
func initiativeStatusContains(v []InitiativeStatus, w InitiativeStatus) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}

var _ InitiativeStore = (*SQLiteStore)(nil)
