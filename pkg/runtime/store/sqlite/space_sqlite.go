package sqlite

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

var _ models.SpaceStore = (*Store)(nil)

// migrateSpacesSQLite mirrors PostgreSQL migration 67. SQLite cannot add a
// NOT NULL constraint to an existing column, so conversations.space_id is
// added with an empty default and backfilled in the same step: a database
// from before spaces gets one space named Default per scope.
func migrateSpacesSQLite(db *sql.DB) error {
	if err := migrateDefaultSpaceSQLite(db); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS spaces (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			name TEXT NOT NULL, look TEXT NOT NULL, position INTEGER NOT NULL, is_default INTEGER NOT NULL,
			access TEXT NOT NULL DEFAULT 'owner', owner_id TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS space_members (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL,
			member_type TEXT NOT NULL, member_id TEXT NOT NULL, position INTEGER NOT NULL, added_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, member_type, member_id)
		);
		CREATE INDEX IF NOT EXISTS idx_space_members_member ON space_members(scope_kind, scope_id, member_type, member_id);
	`); err != nil {
		return err
	}
	if err := migrateSpaceAccessSQLite(db); err != nil {
		return err
	}
	exists, err := sqliteColumnExists(db, "conversations", "space_id")
	if err != nil {
		return err
	}
	if !exists {
		if err := addConversationSpacesSQLite(db); err != nil {
			return err
		}
	}
	return migrateOwnerDefaultSpacesSQLite(db)
}

func addConversationSpacesSQLite(db *sql.DB) error {
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
	var scopes []models.Scope
	for rows.Next() {
		var scope models.Scope
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
		// The space the pre-spaces model kept per scope, as PostgreSQL
		// migration 67 writes it.
		digest := md5.Sum([]byte(scope.Kind + ":" + scope.ID))
		defaultSpace := &models.Space{
			ID: "default-" + hex.EncodeToString(digest[:]), Scope: scope, Name: "Default", Look: "tidepool",
			IsDefault: true, Access: models.SpaceAccessEveryone, OwnerID: "system", CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO spaces (`+sqlstore.SpaceColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			defaultSpace.ID, scope.Kind, scope.ID, defaultSpace.Name, defaultSpace.Look, defaultSpace.Position, defaultSpace.IsDefault,
			defaultSpace.Access, defaultSpace.OwnerID, defaultSpace.CreatedAt, defaultSpace.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO space_members (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
			SELECT scope_kind, scope_id, ?, member_type, id, ROW_NUMBER() OVER (ORDER BY kind_order, updated_at, id) - 1, ?
			FROM (
				SELECT scope_kind, scope_id, id, updated_at, 'agent' AS member_type, 0 AS kind_order FROM agent_deployments
					WHERE scope_kind = ? AND scope_id = ?
				UNION ALL SELECT scope_kind, scope_id, id, updated_at, 'team', 1 FROM team_deployments
					WHERE scope_kind = ? AND scope_id = ?
			)`, defaultSpace.ID, now, scope.Kind, scope.ID, scope.Kind, scope.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conversations SET space_id = ?, payload = json_set(payload, '$.spaceId', ?)
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ''`, defaultSpace.ID, defaultSpace.ID, scope.Kind, scope.ID); err != nil {
			return err
		}
	}
	return nil
}

func getSQLiteSpace(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, spaceID string) (*models.Space, error) {
	space, err := sqlstore.ScanSpace(queryer.QueryRowContext(ctx, `SELECT `+sqlstore.SpaceColumns+` FROM spaces
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrSpaceNotFound
	}
	return space, err
}

func listSQLiteSpaces(ctx context.Context, queryer sqlstore.Reader, scope models.Scope) ([]*models.Space, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT `+sqlstore.SpaceColumns+` FROM spaces
		WHERE scope_kind = ? AND scope_id = ? ORDER BY position, created_at, id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.Space, 0)
	for rows.Next() {
		space, err := sqlstore.ScanSpace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, space)
	}
	return result, rows.Err()
}

// sqliteDefaultSpace returns the owner's default space, or ErrNoDefaultSpace.
func sqliteDefaultSpace(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, ownerID string) (*models.Space, error) {
	space, err := sqlstore.ScanSpace(queryer.QueryRowContext(ctx, `SELECT `+sqlstore.SpaceColumns+` FROM spaces
		WHERE scope_kind = ? AND scope_id = ? AND owner_id = ? AND is_default = 1`, scope.Kind, scope.ID, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrNoDefaultSpace
	}
	return space, err
}

func (s *Store) DefaultSpace(ctx context.Context, scope models.Scope, ownerID string) (*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return sqliteDefaultSpace(ctx, s.db, scope, ownerID)
}

func (s *Store) CreateSpace(ctx context.Context, space *models.Space) (*models.Space, error) {
	if err := space.Validate(); err != nil {
		return nil, err
	}
	created := models.CloneSpace(space)
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		// The immediate transaction makes the owner's first space their only default.
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0), COALESCE(MAX(is_default = 1 AND owner_id = ?), 0) = 0
			FROM spaces WHERE scope_kind = ? AND scope_id = ?`,
			space.OwnerID, space.Scope.Kind, space.Scope.ID).Scan(&created.Position, &created.IsDefault); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO spaces (`+sqlstore.SpaceColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			created.ID, created.Scope.Kind, created.Scope.ID, created.Name, created.Look, created.Position, created.IsDefault,
			created.Access, created.OwnerID, created.CreatedAt, created.UpdatedAt)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
			return models.ErrInvalidSpace
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) GetSpace(ctx context.Context, scope models.Scope, spaceID string) (*models.Space, error) {
	return getSQLiteSpace(ctx, s.db, scope, spaceID)
}

func (s *Store) ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return listSQLiteSpaces(ctx, s.db, scope)
}

func (s *Store) UpdateSpace(ctx context.Context, scope models.Scope, spaceID string, patch models.SpacePatch, now time.Time) (*models.Space, error) {
	var updated *models.Space
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var err error
		if updated, err = getSQLiteSpace(ctx, conn, scope, spaceID); err != nil {
			return err
		}
		models.ApplySpacePatch(updated, patch, now)
		if err := updated.Validate(); err != nil {
			return err
		}
		if patch.Position != nil {
			ordered, err := listSQLiteSpaces(ctx, conn, scope)
			if err != nil {
				return err
			}
			for id, position := range models.SpacePositionsAfterMove(ordered, spaceID, *patch.Position) {
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

func (s *Store) DeleteSpace(ctx context.Context, scope models.Scope, spaceID string, now time.Time) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		space, err := getSQLiteSpace(ctx, conn, scope, spaceID)
		if err != nil {
			return err
		}
		if space.IsDefault {
			return models.ErrDefaultSpaceProtected
		}
		defaultSpace, err := sqliteDefaultSpace(ctx, conn, scope, space.OwnerID)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE conversations SET space_id = ?, payload = json_set(payload, '$.spaceId', ?)
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, defaultSpace.ID, defaultSpace.ID, scope.Kind, scope.ID, spaceID); err != nil {
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
			defaultSpace.ID, scope.Kind, scope.ID, defaultSpace.ID, now, scope.Kind, scope.ID, spaceID, spaceID); err != nil {
			return err
		}
		for _, table := range []string{"space_members", "space_people"} {
			if _, err := conn.ExecContext(ctx, `DELETE FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`,
				scope.Kind, scope.ID, spaceID); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM spaces WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, spaceID)
		return err
	})
}

func (s *Store) ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error) {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqlstore.SpaceMemberColumns+` FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? ORDER BY position, added_at, member_type, member_id`, scope.Kind, scope.ID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.SpaceMember, 0)
	for rows.Next() {
		member, err := sqlstore.ScanSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func (s *Store) PutSpaceMember(ctx context.Context, scope models.Scope, member *models.SpaceMember, with ...*models.SpaceMember) (*models.SpaceMember, bool, error) {
	for _, each := range append([]*models.SpaceMember{member}, with...) {
		if err := each.Validate(); err != nil {
			return nil, false, err
		}
	}
	var result *models.SpaceMember
	var created bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		if _, err := getSQLiteSpace(ctx, conn, scope, member.SpaceID); err != nil {
			return err
		}
		var err error
		if result, created, err = putSQLiteSpaceMember(ctx, conn, scope, member); err != nil {
			return err
		}
		for _, each := range with {
			if _, _, err := putSQLiteSpaceMember(ctx, conn, scope, each); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

func putSQLiteSpaceMember(ctx context.Context, conn *sql.Conn, scope models.Scope, member *models.SpaceMember) (*models.SpaceMember, bool, error) {
	existing, err := sqlstore.ScanSpaceMember(conn.QueryRowContext(ctx, `SELECT `+sqlstore.SpaceMemberColumns+` FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND member_type = ? AND member_id = ?`,
		scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	created := models.CloneSpaceMember(member)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, scope.Kind, scope.ID, member.SpaceID).Scan(&created.Position); err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO space_members (scope_kind, scope_id, `+sqlstore.SpaceMemberColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, created.SpaceID, created.Type, created.ID, created.Position, created.AddedAt); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func (s *Store) DeleteSpaceMember(ctx context.Context, scope models.Scope, spaceID string, memberType models.SpaceMemberType, memberID string) error {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	return deleteSQLiteSpaceMember(ctx, s.db, scope, spaceID, memberType, memberID)
}

func deleteSQLiteSpaceMember(ctx context.Context, db sqlstore.ReadWriter, scope models.Scope, spaceID string, memberType models.SpaceMemberType, memberID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM space_members
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND member_type = ? AND member_id = ?`,
		scope.Kind, scope.ID, spaceID, memberType, memberID)
	return err
}

// DeleteSpaceAgentWithRosters commits the Teams' roster changes and the
// Agent's removal from the space together.
func (s *Store) DeleteSpaceAgentWithRosters(ctx context.Context, scope models.Scope, spaceID, agentID string, changes []*kernelteam.RosterChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := getSQLiteSpace(ctx, tx, scope, spaceID); err != nil {
		return err
	}
	for _, change := range changes {
		if err := updateTeamDeploymentSQLiteTx(ctx, tx, change.Deployment, change.ExpectedRevision, change.Activation); err != nil {
			return err
		}
	}
	if err := deleteSQLiteSpaceMember(ctx, tx, scope, spaceID, models.SpaceMemberAgent, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// resolveSQLiteConversationSpace mirrors resolvePostgresConversationSpaceTx.
func resolveSQLiteConversationSpace(ctx context.Context, conn *sql.Conn, conversation *models.Conversation) error {
	scope := conversation.Scope
	if conversation.SpaceID != "" {
		_, err := getSQLiteSpace(ctx, conn, scope, conversation.SpaceID)
		return err
	}
	var spaceID string
	err := conn.QueryRowContext(ctx, `SELECT sp.id FROM space_members m
		JOIN spaces sp ON sp.scope_kind = m.scope_kind AND sp.scope_id = m.scope_id AND sp.id = m.space_id
		WHERE m.scope_kind = ? AND m.scope_id = ? AND m.member_type = ? AND m.member_id = ?
		ORDER BY sp.position, sp.created_at, sp.id LIMIT 1`, scope.Kind, scope.ID, string(conversation.Owner.Type),
		strings.TrimSpace(conversation.Owner.ID)).Scan(&spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		defaultSpace, defaultErr := sqliteDefaultSpace(ctx, conn, scope, conversation.CreatedBy)
		if defaultErr != nil {
			return defaultErr
		}
		spaceID, err = defaultSpace.ID, nil
	}
	if err != nil {
		return err
	}
	conversation.SpaceID = spaceID
	return nil
}

func (s *Store) EnsureMemberHasSpace(ctx context.Context, scope models.Scope, ownerID string, memberType models.SpaceMemberType, memberID string, now time.Time) (*models.SpaceMember, error) {
	var member *models.SpaceMember
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var placed bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND member_type = ? AND member_id = ?)`,
			scope.Kind, scope.ID, memberType, memberID).Scan(&placed); err != nil || placed {
			return err
		}
		defaultSpace, err := sqliteDefaultSpace(ctx, conn, scope, ownerID)
		if err != nil {
			return err
		}
		member = &models.SpaceMember{SpaceID: defaultSpace.ID, Type: memberType, ID: memberID, AddedAt: now}
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM space_members
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ?`, scope.Kind, scope.ID, defaultSpace.ID).Scan(&member.Position); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO space_members (scope_kind, scope_id, `+sqlstore.SpaceMemberColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID, member.Position, member.AddedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}

func (s *Store) SetSpaceAccess(ctx context.Context, scope models.Scope, spaceID string, access models.SpaceAccess, now time.Time) (*models.Space, error) {
	var space *models.Space
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		if _, err := getSQLiteSpace(ctx, conn, scope, spaceID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE spaces SET access = ?, updated_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			access, now, scope.Kind, scope.ID, spaceID); err != nil {
			return err
		}
		var err error
		space, err = getSQLiteSpace(ctx, conn, scope, spaceID)
		return err
	})
	return space, err
}

func (s *Store) SetSpaceOwner(ctx context.Context, scope models.Scope, spaceID, ownerID string, now time.Time) (*models.Space, error) {
	var space *models.Space
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		current, err := getSQLiteSpace(ctx, conn, scope, spaceID)
		if err != nil {
			return err
		}
		if err := setSQLiteSpaceOwner(ctx, conn, current, ownerID, now); err != nil {
			return err
		}
		space, err = getSQLiteSpace(ctx, conn, scope, spaceID)
		return err
	})
	return space, err
}

// setSQLiteSpaceOwner hands the space to ownerID. Both owners give up their
// default, the space changes hands, and each owner's oldest space becomes
// their default again.
func setSQLiteSpaceOwner(ctx context.Context, conn *sql.Conn, current *models.Space, ownerID string, now time.Time) error {
	scope := current.Scope
	for _, step := range []struct {
		statement string
		args      []interface{}
	}{
		{`UPDATE spaces SET is_default = 0 WHERE scope_kind = ? AND scope_id = ? AND owner_id IN (?, ?) AND is_default = 1`,
			[]interface{}{scope.Kind, scope.ID, current.OwnerID, ownerID}},
		{`UPDATE spaces SET owner_id = ?, updated_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			[]interface{}{ownerID, now, scope.Kind, scope.ID, current.ID}},
		{sqlstore.MarkOwnerDefaultSpaces("spaces", "1") + ` AND s.scope_kind = ? AND s.scope_id = ?`, []interface{}{scope.Kind, scope.ID}},
	} {
		if _, err := conn.ExecContext(ctx, step.statement, step.args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetSpaceAccessState(ctx context.Context, scope models.Scope, spaceID string, state models.SpaceAccessState, now time.Time) (*models.Space, error) {
	var space *models.Space
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		current, err := getSQLiteSpace(ctx, conn, scope, spaceID)
		if err != nil {
			return err
		}
		if state.OwnerID != "" && state.OwnerID != current.OwnerID {
			if err := setSQLiteSpaceOwner(ctx, conn, current, state.OwnerID, now); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE spaces SET access = ?, updated_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			state.Access, now, scope.Kind, scope.ID, spaceID); err != nil {
			return err
		}
		existing, err := listSQLiteSpacePeople(ctx, conn, scope, spaceID)
		if err != nil {
			return err
		}
		for _, person := range existing {
			if slices.ContainsFunc(state.People, func(want *models.SpacePerson) bool { return want.UserID == person.UserID }) {
				continue
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM space_people WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND user_id = ?`,
				scope.Kind, scope.ID, spaceID, person.UserID); err != nil {
				return err
			}
		}
		for _, person := range state.People {
			if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO space_people (scope_kind, scope_id, `+sqlstore.SpacePersonColumns+`)
				VALUES (?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, spaceID, person.UserID, person.AddedBy, person.AddedAt); err != nil {
				return err
			}
		}
		space, err = getSQLiteSpace(ctx, conn, scope, spaceID)
		return err
	})
	return space, err
}

func (s *Store) PutSpacePerson(ctx context.Context, scope models.Scope, person *models.SpacePerson) (*models.SpacePerson, bool, error) {
	if err := person.Validate(); err != nil {
		return nil, false, err
	}
	var stored *models.SpacePerson
	var created bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		if _, err := getSQLiteSpace(ctx, conn, scope, person.SpaceID); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO space_people (scope_kind, scope_id, `+sqlstore.SpacePersonColumns+`)
			VALUES (?, ?, ?, ?, ?, ?)`, scope.Kind, scope.ID, person.SpaceID, person.UserID, person.AddedBy, person.AddedAt)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		created = affected == 1
		stored, err = sqlstore.ScanSpacePerson(conn.QueryRowContext(ctx, `SELECT `+sqlstore.SpacePersonColumns+` FROM space_people
			WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND user_id = ?`, scope.Kind, scope.ID, person.SpaceID, person.UserID))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

func (s *Store) DeleteSpacePerson(ctx context.Context, scope models.Scope, spaceID, userID string) error {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM space_people WHERE scope_kind = ? AND scope_id = ? AND space_id = ? AND user_id = ?`,
		scope.Kind, scope.ID, spaceID, userID)
	return err
}

func (s *Store) ListSpacePeople(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpacePerson, error) {
	if _, err := getSQLiteSpace(ctx, s.db, scope, spaceID); err != nil {
		return nil, err
	}
	return listSQLiteSpacePeople(ctx, s.db, scope, spaceID)
}

func listSQLiteSpacePeople(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, spaceID string) ([]*models.SpacePerson, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT `+sqlstore.SpacePersonColumns+` FROM space_people
		WHERE scope_kind = ? AND scope_id = ? AND space_id = ? ORDER BY added_at, user_id`, scope.Kind, scope.ID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.SpacePerson, 0)
	for rows.Next() {
		person, err := sqlstore.ScanSpacePerson(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, person)
	}
	return result, rows.Err()
}

func (s *Store) VisibleSpaces(ctx context.Context, scope models.Scope, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sp.id FROM spaces sp
		WHERE sp.scope_kind = ?1 AND sp.scope_id = ?2 AND (
			(sp.owner_id = ?3 AND ?3 <> '') OR sp.access = 'everyone' OR (sp.access = 'invited' AND EXISTS (
				SELECT 1 FROM space_people p
				WHERE p.scope_kind = sp.scope_kind AND p.scope_id = sp.scope_id AND p.space_id = sp.id AND p.user_id = ?3)))
		ORDER BY sp.position, sp.created_at, sp.id`, scope.Kind, scope.ID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) ListScopeSpaceMembers(ctx context.Context, scope models.Scope) ([]*models.SpaceMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqlstore.SpaceMemberColumns+` FROM space_members
		WHERE scope_kind = ? AND scope_id = ?`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.SpaceMember, 0)
	for rows.Next() {
		member, err := sqlstore.ScanSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}
