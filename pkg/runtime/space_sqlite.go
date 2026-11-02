package runtime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var _ SpaceStore = (*SQLiteStore)(nil)

// migrateSpacesSQLite mirrors PostgreSQL migration 67. SQLite cannot add a
// NOT NULL constraint to an existing column, so conversations.space_id is
// added with an empty default and backfilled to Personal in the same step.
func migrateSpacesSQLite(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS spaces (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			name TEXT NOT NULL, look TEXT NOT NULL, position INTEGER NOT NULL, is_personal INTEGER NOT NULL,
			created_by TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_spaces_personal ON spaces(scope_kind, scope_id) WHERE is_personal = 1;
		CREATE TABLE IF NOT EXISTS space_members (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL,
			member_type TEXT NOT NULL, member_id TEXT NOT NULL, position INTEGER NOT NULL, added_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, member_type, member_id)
		);
		CREATE INDEX IF NOT EXISTS idx_space_members_member ON space_members(scope_kind, scope_id, member_type, member_id);
	`); err != nil {
		return err
	}
	exists, err := sqliteColumnExists(db, "conversations", "space_id")
	if err != nil || exists {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE conversations ADD COLUMN space_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := backfillSpacesSQLite(tx, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_space ON conversations(scope_kind, scope_id, space_id, updated_at DESC)`); err != nil {
		return err
	}
	return tx.Commit()
}

func sqliteColumnExists(db *sql.DB, table, column string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	return count > 0, err
}

func backfillSpacesSQLite(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT scope_kind, scope_id FROM agent_deployments
		UNION SELECT scope_kind, scope_id FROM team_deployments
		UNION SELECT scope_kind, scope_id FROM conversations`)
	if err != nil {
		return err
	}
	var scopes []Scope
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, scope := range scopes {
		personal := newPersonalSpace(scope, now)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO spaces (`+postgresSpaceColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			personal.ID, scope.Kind, scope.ID, personal.Name, personal.Look, personal.Position, personal.IsPersonal,
			personal.CreatedBy, personal.CreatedAt, personal.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO space_members (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
			SELECT scope_kind, scope_id, ?, member_type, id, ROW_NUMBER() OVER (ORDER BY kind_order, updated_at, id) - 1, ?
			FROM (
				SELECT scope_kind, scope_id, id, updated_at, 'agent' AS member_type, 0 AS kind_order FROM agent_deployments
					WHERE scope_kind = ? AND scope_id = ?
				UNION ALL SELECT scope_kind, scope_id, id, updated_at, 'team', 1 FROM team_deployments
					WHERE scope_kind = ? AND scope_id = ?
			)`, personal.ID, now, scope.Kind, scope.ID, scope.Kind, scope.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conversations SET space_id = ?, payload = json_set(payload, '$.spaceId', ?)
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ''`, personal.ID, personal.ID, scope.Kind, scope.ID); err != nil {
			return err
		}
	}
	return nil
}

type sqliteSpaceExecer interface {
	sqliteConversationQueryer
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func scanSQLiteSpace(row interface{ Scan(...interface{}) error }) (*Space, error) {
	var space Space
	if err := row.Scan(&space.ID, &space.Scope.Kind, &space.Scope.ID, &space.Name, &space.Look, &space.Position,
		&space.IsPersonal, &space.CreatedBy, &space.CreatedAt, &space.UpdatedAt); err != nil {
		return nil, err
	}
	space.CreatedAt, space.UpdatedAt = space.CreatedAt.UTC(), space.UpdatedAt.UTC()
	return &space, nil
}

func getSQLiteSpace(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, spaceID string) (*Space, error) {
	space, err := scanSQLiteSpace(queryer.QueryRowContext(ctx, `SELECT `+postgresSpaceColumns+` FROM spaces
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSpaceNotFound
	}
	return space, err
}

func listSQLiteSpaces(ctx context.Context, queryer sqliteConversationQueryer, scope Scope) ([]*Space, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT `+postgresSpaceColumns+` FROM spaces
		WHERE scope_kind = ? AND scope_id = ? ORDER BY position, created_at, id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Space, 0)
	for rows.Next() {
		space, err := scanSQLiteSpace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, space)
	}
	return result, rows.Err()
}

func ensureSQLitePersonalSpace(ctx context.Context, conn sqliteSpaceExecer, scope Scope, now time.Time) (*Space, bool, error) {
	space := newPersonalSpace(scope, now)
	result, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO spaces (`+postgresSpaceColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		space.ID, scope.Kind, scope.ID, space.Name, space.Look, space.Position, space.IsPersonal, space.CreatedBy, space.CreatedAt, space.UpdatedAt)
	if err != nil {
		return nil, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	stored, err := getSQLiteSpace(ctx, conn, scope, space.ID)
	return stored, affected == 1, err
}

// withSQLiteSpaceTx runs fn in an immediate transaction and commits on success.
func (s *SQLiteStore) withSQLiteSpaceTx(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *SQLiteStore) EnsurePersonalSpace(ctx context.Context, scope Scope, now time.Time) (*Space, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	var space *Space
	var created bool
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		var err error
		space, created, err = ensureSQLitePersonalSpace(ctx, conn, scope, now)
		return err
	})
	return space, created, err
}

func (s *SQLiteStore) CreateSpace(ctx context.Context, space *Space) (*Space, error) {
	if err := space.Validate(); err != nil {
		return nil, err
	}
	created := cloneSpace(space)
	created.IsPersonal = false
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM spaces WHERE scope_kind = ? AND scope_id = ?`,
			space.Scope.Kind, space.Scope.ID).Scan(&created.Position); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO spaces (`+postgresSpaceColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			created.ID, created.Scope.Kind, created.Scope.ID, created.Name, created.Look, created.Position, created.IsPersonal,
			created.CreatedBy, created.CreatedAt, created.UpdatedAt)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrInvalidSpace
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *SQLiteStore) GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	return getSQLiteSpace(ctx, s.db, scope, spaceID)
}

func (s *SQLiteStore) ListSpaces(ctx context.Context, scope Scope) ([]*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return listSQLiteSpaces(ctx, s.db, scope)
}

func (s *SQLiteStore) UpdateSpace(ctx context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error) {
	var updated *Space
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		var err error
		if updated, err = getSQLiteSpace(ctx, conn, scope, spaceID); err != nil {
			return err
		}
		applySpacePatch(updated, patch, now)
		if err := updated.Validate(); err != nil {
			return err
		}
		if patch.Position != nil {
			ordered, err := listSQLiteSpaces(ctx, conn, scope)
			if err != nil {
				return err
			}
			for id, position := range moveSpace(ordered, spaceID, *patch.Position) {
				if id == spaceID {
					updated.Position = position
					continue
				}
				if _, err := conn.ExecContext(ctx, `UPDATE spaces SET position = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
					position, scope.Kind, scope.ID, id); err != nil {
					return err
				}
			}
		}
		_, err = conn.ExecContext(ctx, `UPDATE spaces SET name = ?, look = ?, position = ?, updated_at = ?
			WHERE scope_kind = ? AND scope_id = ? AND id = ?`, updated.Name, updated.Look, updated.Position, updated.UpdatedAt,
			scope.Kind, scope.ID, spaceID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *SQLiteStore) DeleteSpace(ctx context.Context, scope Scope, spaceID string, now time.Time) error {
	return s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		space, err := getSQLiteSpace(ctx, conn, scope, spaceID)
		if err != nil {
			return err
		}
		if space.IsPersonal {
			return ErrPersonalSpaceProtected
		}
		personal, _, err := ensureSQLitePersonalSpace(ctx, conn, scope, now)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE conversations SET space_id = ?, payload = json_set(payload, '$.spaceId', ?)
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, personal.ID, personal.ID, scope.Kind, scope.ID, spaceID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO space_members (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
			SELECT m.scope_kind, m.scope_id, ?, m.member_type, m.member_id,
				(SELECT COALESCE(MAX(position) + 1, 0) FROM space_members WHERE scope_kind = ? AND scope_id = ? AND space_id = ?)
					+ ROW_NUMBER() OVER (ORDER BY m.position, m.added_at, m.member_type, m.member_id) - 1, ?
			FROM space_members m
			WHERE m.scope_kind = ? AND m.scope_id = ? AND m.space_id = ? AND NOT EXISTS (
				SELECT 1 FROM space_members other WHERE other.scope_kind = m.scope_kind AND other.scope_id = m.scope_id
					AND other.member_type = m.member_type AND other.member_id = m.member_id AND other.space_id <> ?)`,
			personal.ID, scope.Kind, scope.ID, personal.ID, now, scope.Kind, scope.ID, spaceID, spaceID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM space_members WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`,
			scope.Kind, scope.ID, spaceID); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM spaces WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, spaceID)
		return err
	})
}

func scanSQLiteSpaceMember(row interface{ Scan(...interface{}) error }) (*SpaceMember, error) {
	var member SpaceMember
	if err := row.Scan(&member.SpaceID, &member.Type, &member.ID, &member.Position, &member.AddedAt); err != nil {
		return nil, err
	}
	member.AddedAt = member.AddedAt.UTC()
	return &member, nil
}

func (s *SQLiteStore) ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? ORDER BY position, added_at, member_type, member_id`, scope.Kind, scope.ID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*SpaceMember, 0)
	for rows.Next() {
		member, err := scanSQLiteSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember) (*SpaceMember, bool, error) {
	if err := member.Validate(); err != nil {
		return nil, false, err
	}
	var result *SpaceMember
	var created bool
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		if _, err := getSQLiteSpace(ctx, conn, scope, member.SpaceID); err != nil {
			return err
		}
		existing, err := scanSQLiteSpaceMember(conn.QueryRowContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND member_type = ? AND member_id = ?`,
			scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID))
		if err == nil {
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		result, created = cloneSpaceMember(member), true
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, scope.Kind, scope.ID, member.SpaceID).Scan(&result.Position); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO space_members (scope_kind, scope_id, `+postgresSpaceMemberColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, result.SpaceID, result.Type, result.ID, result.Position, result.AddedAt)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

func (s *SQLiteStore) DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND member_type = ? AND member_id = ?`,
		scope.Kind, scope.ID, spaceID, memberType, memberID)
	return err
}

// resolveSQLiteConversationSpace mirrors resolvePostgresConversationSpaceTx.
func resolveSQLiteConversationSpace(ctx context.Context, conn *sql.Conn, conversation *Conversation) error {
	if conversation.SpaceID != "" {
		_, err := getSQLiteSpace(ctx, conn, conversation.Scope, conversation.SpaceID)
		return err
	}
	spaceID, err := sqliteOwnerSpaceID(ctx, conn, conversation.Scope, &conversation.Owner, conversation.CreatedAt)
	if err != nil {
		return err
	}
	conversation.SpaceID = spaceID
	return nil
}

// sqliteOwnerSpaceID mirrors postgresOwnerSpaceIDTx.
func sqliteOwnerSpaceID(ctx context.Context, conn sqliteSpaceExecer, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error) {
	if owner != nil && strings.TrimSpace(owner.ID) != "" {
		var spaceID string
		err := conn.QueryRowContext(ctx, `SELECT sp.id FROM space_members m
			JOIN spaces sp ON sp.scope_kind = m.scope_kind AND sp.scope_id = m.scope_id AND sp.id = m.space_id
			WHERE m.scope_kind = ? AND m.scope_id = ? AND m.member_type = ? AND m.member_id = ?
			ORDER BY sp.position, sp.created_at, sp.id LIMIT 1`, scope.Kind, scope.ID, string(owner.Type),
			strings.TrimSpace(owner.ID)).Scan(&spaceID)
		if err == nil {
			return spaceID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	personal, _, err := ensureSQLitePersonalSpace(ctx, conn, scope, now)
	if err != nil {
		return "", err
	}
	return personal.ID, nil
}

// OwnerSpaceID places something an owner made outside any chat.
func (s *SQLiteStore) OwnerSpaceID(ctx context.Context, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error) {
	var spaceID string
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		var err error
		spaceID, err = sqliteOwnerSpaceID(ctx, conn, scope, owner, now)
		return err
	})
	return spaceID, err
}

func (s *SQLiteStore) EnsureMemberHasSpace(ctx context.Context, scope Scope, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error) {
	var member *SpaceMember
	err := s.withSQLiteSpaceTx(ctx, func(conn *sql.Conn) error {
		var placed bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND member_type = ? AND member_id = ?)`,
			scope.Kind, scope.ID, memberType, memberID).Scan(&placed); err != nil || placed {
			return err
		}
		personal, _, err := ensureSQLitePersonalSpace(ctx, conn, scope, now)
		if err != nil {
			return err
		}
		member = &SpaceMember{SpaceID: personal.ID, Type: memberType, ID: memberID, AddedAt: now}
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, scope.Kind, scope.ID, personal.ID).Scan(&member.Position); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO space_members (scope_kind, scope_id, `+postgresSpaceMemberColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID, member.Position, member.AddedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}
