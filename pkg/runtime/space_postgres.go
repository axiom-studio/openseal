package runtime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const spacesMigrationVersion int64 = 67

var _ SpaceStore = (*PostgresStore)(nil)

// migrateSpaces creates spaces and their members, then backfills in one step:
// every scope seen in Agent or Team deployments or conversations gets one
// space named Default, every deployment becomes its member, every
// conversation moves to it, and conversations.space_id becomes NOT NULL.
// Migration 85 made default spaces per owner; nothing after this backfill
// creates a space on its own.
func (s *PostgresStore) migrateSpaces(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, spacesMigrationVersion)
	if err != nil || applied {
		return err
	}
	spaces, members, conversations := s.table("spaces"), s.table("space_members"), s.table("conversations")
	defaultID := `'default-' || md5(scope_kind || ':' || scope_id)`
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ` + spaces + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			name TEXT NOT NULL, look TEXT NOT NULL, position INTEGER NOT NULL, is_default BOOLEAN NOT NULL,
			created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id), CHECK (position >= 0)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS spaces_default_idx ON ` + spaces + ` (scope_kind, scope_id) WHERE is_default`,
		`CREATE TABLE IF NOT EXISTS ` + members + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL,
			member_type TEXT NOT NULL, member_id TEXT NOT NULL, position INTEGER NOT NULL, added_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, member_type, member_id),
			FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + spaces + ` (scope_kind, scope_id, id) ON DELETE CASCADE,
			CHECK (member_type IN ('agent', 'team')), CHECK (position >= 0)
		)`,
		`CREATE INDEX IF NOT EXISTS space_members_member_idx ON ` + members + ` (scope_kind, scope_id, member_type, member_id)`,
		`ALTER TABLE ` + conversations + ` ADD COLUMN IF NOT EXISTS space_id TEXT`,
		`INSERT INTO ` + spaces + ` (scope_kind, scope_id, id, name, look, position, is_default, created_by, created_at, updated_at)
			SELECT scope_kind, scope_id, ` + defaultID + `, 'Default', 'tidepool', 0, TRUE, 'system', now(), now()
			FROM (
				SELECT scope_kind, scope_id FROM ` + s.table("agent_deployments") + `
				UNION SELECT scope_kind, scope_id FROM ` + s.table("team_deployments") + `
				UNION SELECT scope_kind, scope_id FROM ` + conversations + `
			) scopes
			ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + members + ` (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
			SELECT scope_kind, scope_id, ` + defaultID + `, member_type, id,
				(ROW_NUMBER() OVER (PARTITION BY scope_kind, scope_id ORDER BY kind_order, updated_at, id) - 1)::INTEGER, now()
			FROM (
				SELECT scope_kind, scope_id, id, updated_at, 'agent' AS member_type, 0 AS kind_order FROM ` + s.table("agent_deployments") + `
				UNION ALL SELECT scope_kind, scope_id, id, updated_at, 'team', 1 FROM ` + s.table("team_deployments") + `
			) deployments
			ON CONFLICT DO NOTHING`,
		`UPDATE ` + conversations + ` SET space_id = ` + defaultID + `,
			payload = jsonb_set(payload, '{spaceId}', to_jsonb(` + defaultID + `), true)
			WHERE space_id IS NULL`,
		`ALTER TABLE ` + conversations + ` ALTER COLUMN space_id SET NOT NULL`,
		`CREATE INDEX IF NOT EXISTS conversations_space_idx ON ` + conversations + ` (scope_kind, scope_id, space_id, updated_at DESC)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'spaces') ON CONFLICT(version) DO NOTHING`, spacesMigrationVersion)
	return err
}

func (s *PostgresStore) rollbackSpaces(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("conversations_space_idx")+`;
		ALTER TABLE `+s.table("conversations")+` DROP COLUMN IF EXISTS space_id;
		UPDATE `+s.table("conversations")+` SET payload = payload - 'spaceId' WHERE payload ? 'spaceId';
		DROP TABLE IF EXISTS `+s.table("space_members")+` CASCADE;
		DROP TABLE IF EXISTS `+s.table("spaces")+` CASCADE`)
	return err
}

const postgresSpaceColumns = `id, scope_kind, scope_id, name, look, position, is_default, access, owner_id, created_at, updated_at`

func scanPostgresSpace(row interface{ Scan(...interface{}) error }) (*Space, error) {
	var space Space
	if err := row.Scan(&space.ID, &space.Scope.Kind, &space.Scope.ID, &space.Name, &space.Look, &space.Position,
		&space.IsDefault, &space.Access, &space.OwnerID, &space.CreatedAt, &space.UpdatedAt); err != nil {
		return nil, err
	}
	space.CreatedAt, space.UpdatedAt = space.CreatedAt.UTC(), space.UpdatedAt.UTC()
	return &space, nil
}

func (s *PostgresStore) spacesLock(ctx context.Context, tx *sql.Tx, scope Scope) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:spaces:"+scope.key())
	return err
}

// postgresDefaultSpace returns the owner's default space, or
// ErrNoDefaultSpace.
func (s *PostgresStore) postgresDefaultSpace(ctx context.Context, queryer postgresConversationQueryer, scope Scope, ownerID string) (*Space, error) {
	space, err := scanPostgresSpace(queryer.QueryRowContext(ctx, `SELECT `+postgresSpaceColumns+` FROM `+s.table("spaces")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND owner_id = $3 AND is_default`, scope.Kind, scope.ID, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDefaultSpace
	}
	return space, err
}

func (s *PostgresStore) getPostgresSpace(ctx context.Context, queryer postgresConversationQueryer, scope Scope, spaceID string) (*Space, error) {
	space, err := scanPostgresSpace(queryer.QueryRowContext(ctx, `SELECT `+postgresSpaceColumns+` FROM `+s.table("spaces")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSpaceNotFound
	}
	return space, err
}

func (s *PostgresStore) listPostgresSpaces(ctx context.Context, queryer postgresConversationQueryer, scope Scope) ([]*Space, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT `+postgresSpaceColumns+` FROM `+s.table("spaces")+`
		WHERE scope_kind = $1 AND scope_id = $2 ORDER BY position, created_at, id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Space, 0)
	for rows.Next() {
		space, err := scanPostgresSpace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, space)
	}
	return result, rows.Err()
}

func (s *PostgresStore) DefaultSpace(ctx context.Context, scope Scope, ownerID string) (*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.postgresDefaultSpace(ctx, s.db, scope, ownerID)
}

func (s *PostgresStore) CreateSpace(ctx context.Context, space *Space) (*Space, error) {
	if err := space.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, space.Scope); err != nil {
		return nil, err
	}
	created := cloneSpace(space)
	// The scope lock above makes the owner's first space their only default.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0), NOT COALESCE(BOOL_OR(is_default AND owner_id = $3), FALSE)
		FROM `+s.table("spaces")+` WHERE scope_kind = $1 AND scope_id = $2`,
		space.Scope.Kind, space.Scope.ID, space.OwnerID).Scan(&created.Position, &created.IsDefault); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("spaces")+` (`+postgresSpaceColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, created.ID, created.Scope.Kind, created.Scope.ID, created.Name, created.Look,
		created.Position, created.IsDefault, created.Access, created.OwnerID, created.CreatedAt, created.UpdatedAt); err != nil {
		if postgresUniqueViolation(err) {
			return nil, ErrInvalidSpace
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

func (s *PostgresStore) GetSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	return s.getPostgresSpace(ctx, s.db, scope, spaceID)
}

func (s *PostgresStore) ListSpaces(ctx context.Context, scope Scope) ([]*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.listPostgresSpaces(ctx, s.db, scope)
}

func (s *PostgresStore) UpdateSpace(ctx context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return nil, err
	}
	updated, err := s.getPostgresSpace(ctx, tx, scope, spaceID)
	if err != nil {
		return nil, err
	}
	applySpacePatch(updated, patch, now)
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	if patch.Position != nil {
		ordered, err := s.listPostgresSpaces(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		for id, position := range moveSpace(ordered, spaceID, *patch.Position) {
			if id == spaceID {
				updated.Position = position
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("spaces")+` SET position = $1
				WHERE scope_kind = $2 AND scope_id = $3 AND id = $4 AND position <> $1`, position, scope.Kind, scope.ID, id); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("spaces")+` SET name = $1, look = $2, position = $3, updated_at = $4
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7`, updated.Name, updated.Look, updated.Position, updated.UpdatedAt,
		scope.Kind, scope.ID, spaceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *PostgresStore) DeleteSpace(ctx context.Context, scope Scope, spaceID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return err
	}
	space, err := s.getPostgresSpace(ctx, tx, scope, spaceID)
	if err != nil {
		return err
	}
	if space.IsDefault {
		return ErrDefaultSpaceProtected
	}
	defaultSpace, err := s.postgresDefaultSpace(ctx, tx, scope, space.OwnerID)
	if err != nil {
		return err
	}
	members, conversations := s.table("space_members"), s.table("conversations")
	if _, err := tx.ExecContext(ctx, `UPDATE `+conversations+` SET space_id = $1,
		payload = jsonb_set(payload, '{spaceId}', to_jsonb($1::text), true)
		WHERE scope_kind = $2 AND scope_id = $3 AND space_id = $4`, defaultSpace.ID, scope.Kind, scope.ID, spaceID); err != nil {
		return err
	}
	// Members found only in the deleted space join the owner's default space
	// at its end.
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+members+` (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
		SELECT m.scope_kind, m.scope_id, $1, m.member_type, m.member_id,
			(SELECT COALESCE(MAX(position) + 1, 0) FROM `+members+` WHERE scope_kind = $2 AND scope_id = $3 AND space_id = $1)
				+ (ROW_NUMBER() OVER (ORDER BY m.position, m.added_at, m.member_type, m.member_id) - 1)::INTEGER, $5
		FROM `+members+` m
		WHERE m.scope_kind = $2 AND m.scope_id = $3 AND m.space_id = $4 AND NOT EXISTS (
			SELECT 1 FROM `+members+` other WHERE other.scope_kind = m.scope_kind AND other.scope_id = m.scope_id
				AND other.member_type = m.member_type AND other.member_id = m.member_id AND other.space_id <> $4)`,
		defaultSpace.ID, scope.Kind, scope.ID, spaceID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("spaces")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`,
		scope.Kind, scope.ID, spaceID); err != nil {
		return err
	}
	return tx.Commit()
}

const postgresSpaceMemberColumns = `space_id, member_type, member_id, position, added_at`

func scanPostgresSpaceMember(row interface{ Scan(...interface{}) error }) (*SpaceMember, error) {
	var member SpaceMember
	if err := row.Scan(&member.SpaceID, &member.Type, &member.ID, &member.Position, &member.AddedAt); err != nil {
		return nil, err
	}
	member.AddedAt = member.AddedAt.UTC()
	return &member, nil
}

func (s *PostgresStore) ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	if _, err := s.getPostgresSpace(ctx, s.db, scope, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM `+s.table("space_members")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 ORDER BY position, added_at, member_type, member_id`,
		scope.Kind, scope.ID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*SpaceMember, 0)
	for rows.Next() {
		member, err := scanPostgresSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func (s *PostgresStore) PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember, with ...*SpaceMember) (*SpaceMember, bool, error) {
	for _, each := range append([]*SpaceMember{member}, with...) {
		if err := each.Validate(); err != nil {
			return nil, false, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return nil, false, err
	}
	if _, err := s.getPostgresSpace(ctx, tx, scope, member.SpaceID); err != nil {
		return nil, false, err
	}
	stored, created, err := s.putPostgresSpaceMemberTx(ctx, tx, scope, member)
	if err != nil {
		return nil, false, err
	}
	for _, each := range with {
		if _, _, err := s.putPostgresSpaceMemberTx(ctx, tx, scope, each); err != nil {
			return nil, false, err
		}
	}
	return stored, created, tx.Commit()
}

func (s *PostgresStore) putPostgresSpaceMemberTx(ctx context.Context, tx *sql.Tx, scope Scope, member *SpaceMember) (*SpaceMember, bool, error) {
	members := s.table("space_members")
	existing, err := scanPostgresSpaceMember(tx.QueryRowContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND member_type = $4 AND member_id = $5`,
		scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	created := cloneSpaceMember(member)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3`, scope.Kind, scope.ID, member.SpaceID).Scan(&created.Position); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+members+` (scope_kind, scope_id, `+postgresSpaceMemberColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, scope.Kind, scope.ID, created.SpaceID, created.Type, created.ID, created.Position, created.AddedAt); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func (s *PostgresStore) DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	if _, err := s.getPostgresSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	return s.deletePostgresSpaceMember(ctx, s.db, scope, spaceID, memberType, memberID)
}

func (s *PostgresStore) deletePostgresSpaceMember(ctx context.Context, db sqlExecQueryer, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM `+s.table("space_members")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND member_type = $4 AND member_id = $5`,
		scope.Kind, scope.ID, spaceID, memberType, memberID)
	return err
}

// DeleteSpaceAgentWithRosters commits the Teams' roster changes and the
// Agent's removal from the space together.
func (s *PostgresStore) DeleteSpaceAgentWithRosters(ctx context.Context, scope Scope, spaceID, agentID string, changes []*kernelteam.RosterChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.getPostgresSpace(ctx, tx, scope, spaceID); err != nil {
		return err
	}
	for _, change := range changes {
		if err := s.updateTeamDeploymentTx(ctx, tx, change.Deployment, change.ExpectedRevision, change.Activation); err != nil {
			return err
		}
	}
	if err := s.deletePostgresSpaceMember(ctx, tx, scope, spaceID, SpaceMemberAgent, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// resolvePostgresConversationSpaceTx verifies an explicit SpaceID. It fills
// an empty one with the owner deployment's first space by position, else the
// default space of the person who created the conversation.
func (s *PostgresStore) resolvePostgresConversationSpaceTx(ctx context.Context, tx *sql.Tx, conversation *Conversation) error {
	scope := conversation.Scope
	if conversation.SpaceID != "" {
		_, err := s.getPostgresSpace(ctx, tx, scope, conversation.SpaceID)
		return err
	}
	var spaceID string
	err := tx.QueryRowContext(ctx, `SELECT sp.id FROM `+s.table("space_members")+` m
		JOIN `+s.table("spaces")+` sp ON sp.scope_kind = m.scope_kind AND sp.scope_id = m.scope_id AND sp.id = m.space_id
		WHERE m.scope_kind = $1 AND m.scope_id = $2 AND m.member_type = $3 AND m.member_id = $4
		ORDER BY sp.position, sp.created_at, sp.id LIMIT 1`, scope.Kind, scope.ID, string(conversation.Owner.Type),
		strings.TrimSpace(conversation.Owner.ID)).Scan(&spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		defaultSpace, defaultErr := s.postgresDefaultSpace(ctx, tx, scope, conversation.CreatedBy)
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

func (s *PostgresStore) EnsureMemberHasSpace(ctx context.Context, scope Scope, ownerID string, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return nil, err
	}
	members := s.table("space_members")
	var placed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND member_type = $3 AND member_id = $4)`,
		scope.Kind, scope.ID, memberType, memberID).Scan(&placed); err != nil || placed {
		return nil, err
	}
	defaultSpace, err := s.postgresDefaultSpace(ctx, tx, scope, ownerID)
	if err != nil {
		return nil, err
	}
	member := &SpaceMember{SpaceID: defaultSpace.ID, Type: memberType, ID: memberID, AddedAt: now}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3`, scope.Kind, scope.ID, defaultSpace.ID).Scan(&member.Position); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+members+` (scope_kind, scope_id, `+postgresSpaceMemberColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID, member.Position, member.AddedAt); err != nil {
		return nil, err
	}
	return member, tx.Commit()
}

func (s *PostgresStore) SetSpaceAccess(ctx context.Context, scope Scope, spaceID string, access SpaceAccess, now time.Time) (*Space, error) {
	space, err := scanPostgresSpace(s.db.QueryRowContext(ctx, `UPDATE `+s.table("spaces")+` SET access = $1, updated_at = $2
		WHERE scope_kind = $3 AND scope_id = $4 AND id = $5 RETURNING `+postgresSpaceColumns, access, now, scope.Kind, scope.ID, spaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSpaceNotFound
	}
	return space, err
}

func (s *PostgresStore) SetSpaceOwner(ctx context.Context, scope Scope, spaceID, ownerID string, now time.Time) (*Space, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return nil, err
	}
	space, err := s.getPostgresSpace(ctx, tx, scope, spaceID)
	if err != nil {
		return nil, err
	}
	spaces := s.table("spaces")
	// Both owners give up their default, the space changes hands, and each
	// owner's oldest space becomes their default again.
	if _, err := tx.ExecContext(ctx, `UPDATE `+spaces+` SET is_default = FALSE
		WHERE scope_kind = $1 AND scope_id = $2 AND owner_id IN ($3, $4) AND is_default`, scope.Kind, scope.ID, space.OwnerID, ownerID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+spaces+` SET owner_id = $1, updated_at = $2
		WHERE scope_kind = $3 AND scope_id = $4 AND id = $5`, ownerID, now, scope.Kind, scope.ID, spaceID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, markOwnerDefaultSpaces(spaces, "TRUE")+` AND s.scope_kind = $1 AND s.scope_id = $2`, scope.Kind, scope.ID); err != nil {
		return nil, err
	}
	if space, err = s.getPostgresSpace(ctx, tx, scope, spaceID); err != nil {
		return nil, err
	}
	return space, tx.Commit()
}

const postgresSpacePersonColumns = `space_id, user_id, added_by, added_at`

func scanSpacePerson(row interface{ Scan(...interface{}) error }) (*SpacePerson, error) {
	var person SpacePerson
	if err := row.Scan(&person.SpaceID, &person.UserID, &person.AddedBy, &person.AddedAt); err != nil {
		return nil, err
	}
	person.AddedAt = person.AddedAt.UTC()
	return &person, nil
}

func (s *PostgresStore) PutSpacePerson(ctx context.Context, scope Scope, person *SpacePerson) (*SpacePerson, bool, error) {
	if err := person.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := s.spacesLock(ctx, tx, scope); err != nil {
		return nil, false, err
	}
	if _, err := s.getPostgresSpace(ctx, tx, scope, person.SpaceID); err != nil {
		return nil, false, err
	}
	people := s.table("space_people")
	result, err := tx.ExecContext(ctx, `INSERT INTO `+people+` (scope_kind, scope_id, `+postgresSpacePersonColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, scope.Kind, scope.ID, person.SpaceID, person.UserID, person.AddedBy, person.AddedAt)
	if err != nil {
		return nil, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	stored, err := scanSpacePerson(tx.QueryRowContext(ctx, `SELECT `+postgresSpacePersonColumns+` FROM `+people+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND user_id = $4`, scope.Kind, scope.ID, person.SpaceID, person.UserID))
	if err != nil {
		return nil, false, err
	}
	return stored, affected == 1, tx.Commit()
}

func (s *PostgresStore) DeleteSpacePerson(ctx context.Context, scope Scope, spaceID, userID string) error {
	if _, err := s.getPostgresSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("space_people")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND user_id = $4`, scope.Kind, scope.ID, spaceID, userID)
	return err
}

func (s *PostgresStore) ListSpacePeople(ctx context.Context, scope Scope, spaceID string) ([]*SpacePerson, error) {
	if _, err := s.getPostgresSpace(ctx, s.db, scope, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+postgresSpacePersonColumns+` FROM `+s.table("space_people")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 ORDER BY added_at, user_id`, scope.Kind, scope.ID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*SpacePerson, 0)
	for rows.Next() {
		person, err := scanSpacePerson(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, person)
	}
	return result, rows.Err()
}

func (s *PostgresStore) VisibleSpaces(ctx context.Context, scope Scope, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sp.id FROM `+s.table("spaces")+` sp
		WHERE sp.scope_kind = $1 AND sp.scope_id = $2 AND (
			(sp.owner_id = $3 AND $3 <> '') OR sp.access = 'everyone' OR (sp.access = 'invited' AND EXISTS (
				SELECT 1 FROM `+s.table("space_people")+` p
				WHERE p.scope_kind = sp.scope_kind AND p.scope_id = sp.scope_id AND p.space_id = sp.id AND p.user_id = $3)))
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

func (s *PostgresStore) ListScopeSpaceMembers(ctx context.Context, scope Scope) ([]*SpaceMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM `+s.table("space_members")+`
		WHERE scope_kind = $1 AND scope_id = $2`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*SpaceMember, 0)
	for rows.Next() {
		member, err := scanPostgresSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}
