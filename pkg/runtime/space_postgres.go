package runtime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const spacesMigrationVersion int64 = 67

var _ SpaceStore = (*PostgresStore)(nil)

// migrateSpaces creates spaces and their members, then backfills in one step:
// every scope seen in Agent or Team deployments or conversations gets its
// Personal space, every deployment becomes a Personal member, every
// conversation moves to Personal, and conversations.space_id becomes NOT NULL.
func (s *PostgresStore) migrateSpaces(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, spacesMigrationVersion)
	if err != nil || applied {
		return err
	}
	spaces, members, conversations := s.table("spaces"), s.table("space_members"), s.table("conversations")
	personalID := `'personal-' || md5(scope_kind || ':' || scope_id)`
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ` + spaces + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			name TEXT NOT NULL, look TEXT NOT NULL, position INTEGER NOT NULL, is_personal BOOLEAN NOT NULL,
			created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id), CHECK (position >= 0)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS spaces_personal_idx ON ` + spaces + ` (scope_kind, scope_id) WHERE is_personal`,
		`CREATE TABLE IF NOT EXISTS ` + members + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL,
			member_type TEXT NOT NULL, member_id TEXT NOT NULL, position INTEGER NOT NULL, added_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, member_type, member_id),
			FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + spaces + ` (scope_kind, scope_id, id) ON DELETE CASCADE,
			CHECK (member_type IN ('agent', 'team')), CHECK (position >= 0)
		)`,
		`CREATE INDEX IF NOT EXISTS space_members_member_idx ON ` + members + ` (scope_kind, scope_id, member_type, member_id)`,
		`ALTER TABLE ` + conversations + ` ADD COLUMN IF NOT EXISTS space_id TEXT`,
		`INSERT INTO ` + spaces + ` (scope_kind, scope_id, id, name, look, position, is_personal, created_by, created_at, updated_at)
			SELECT scope_kind, scope_id, ` + personalID + `, '` + PersonalSpaceName + `', '` + string(PersonalSpaceLook) + `', 0, TRUE, 'system', now(), now()
			FROM (
				SELECT scope_kind, scope_id FROM ` + s.table("agent_deployments") + `
				UNION SELECT scope_kind, scope_id FROM ` + s.table("team_deployments") + `
				UNION SELECT scope_kind, scope_id FROM ` + conversations + `
			) scopes
			ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + members + ` (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
			SELECT scope_kind, scope_id, ` + personalID + `, member_type, id,
				(ROW_NUMBER() OVER (PARTITION BY scope_kind, scope_id ORDER BY kind_order, updated_at, id) - 1)::INTEGER, now()
			FROM (
				SELECT scope_kind, scope_id, id, updated_at, 'agent' AS member_type, 0 AS kind_order FROM ` + s.table("agent_deployments") + `
				UNION ALL SELECT scope_kind, scope_id, id, updated_at, 'team', 1 FROM ` + s.table("team_deployments") + `
			) deployments
			ON CONFLICT DO NOTHING`,
		`UPDATE ` + conversations + ` SET space_id = ` + personalID + `,
			payload = jsonb_set(payload, '{spaceId}', to_jsonb(` + personalID + `), true)
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

const postgresSpaceColumns = `id, scope_kind, scope_id, name, look, position, is_personal, created_by, created_at, updated_at`

func scanPostgresSpace(row interface{ Scan(...interface{}) error }) (*Space, error) {
	var space Space
	if err := row.Scan(&space.ID, &space.Scope.Kind, &space.Scope.ID, &space.Name, &space.Look, &space.Position,
		&space.IsPersonal, &space.CreatedBy, &space.CreatedAt, &space.UpdatedAt); err != nil {
		return nil, err
	}
	space.CreatedAt, space.UpdatedAt = space.CreatedAt.UTC(), space.UpdatedAt.UTC()
	return &space, nil
}

func (s *PostgresStore) spacesLock(ctx context.Context, tx *sql.Tx, scope Scope) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:spaces:"+scope.key())
	return err
}

func (s *PostgresStore) ensurePostgresPersonalSpaceTx(ctx context.Context, tx *sql.Tx, scope Scope, now time.Time) (*Space, bool, error) {
	space := newPersonalSpace(scope, now)
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("spaces")+` (`+postgresSpaceColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, space.ID, scope.Kind, scope.ID, space.Name, space.Look,
		space.Position, space.IsPersonal, space.CreatedBy, space.CreatedAt, space.UpdatedAt)
	if err != nil {
		return nil, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	stored, err := s.getPostgresSpace(ctx, tx, scope, space.ID)
	return stored, affected == 1, err
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

func (s *PostgresStore) EnsurePersonalSpace(ctx context.Context, scope Scope, now time.Time) (*Space, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	space, created, err := s.ensurePostgresPersonalSpaceTx(ctx, tx, scope, now)
	if err != nil {
		return nil, false, err
	}
	return space, created, tx.Commit()
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
	created.IsPersonal = false
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM `+s.table("spaces")+`
		WHERE scope_kind = $1 AND scope_id = $2`, space.Scope.Kind, space.Scope.ID).Scan(&created.Position); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("spaces")+` (`+postgresSpaceColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, created.ID, created.Scope.Kind, created.Scope.ID, created.Name, created.Look,
		created.Position, created.IsPersonal, created.CreatedBy, created.CreatedAt, created.UpdatedAt); err != nil {
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
	if space.IsPersonal {
		return ErrPersonalSpaceProtected
	}
	personal, _, err := s.ensurePostgresPersonalSpaceTx(ctx, tx, scope, now)
	if err != nil {
		return err
	}
	members, conversations := s.table("space_members"), s.table("conversations")
	if _, err := tx.ExecContext(ctx, `UPDATE `+conversations+` SET space_id = $1,
		payload = jsonb_set(payload, '{spaceId}', to_jsonb($1::text), true)
		WHERE scope_kind = $2 AND scope_id = $3 AND space_id = $4`, personal.ID, scope.Kind, scope.ID, spaceID); err != nil {
		return err
	}
	// Members found only in the deleted space join Personal at its end.
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+members+` (scope_kind, scope_id, space_id, member_type, member_id, position, added_at)
		SELECT m.scope_kind, m.scope_id, $1, m.member_type, m.member_id,
			(SELECT COALESCE(MAX(position) + 1, 0) FROM `+members+` WHERE scope_kind = $2 AND scope_id = $3 AND space_id = $1)
				+ (ROW_NUMBER() OVER (ORDER BY m.position, m.added_at, m.member_type, m.member_id) - 1)::INTEGER, $5
		FROM `+members+` m
		WHERE m.scope_kind = $2 AND m.scope_id = $3 AND m.space_id = $4 AND NOT EXISTS (
			SELECT 1 FROM `+members+` other WHERE other.scope_kind = m.scope_kind AND other.scope_id = m.scope_id
				AND other.member_type = m.member_type AND other.member_id = m.member_id AND other.space_id <> $4)`,
		personal.ID, scope.Kind, scope.ID, spaceID, now); err != nil {
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

func (s *PostgresStore) PutSpaceMember(ctx context.Context, scope Scope, member *SpaceMember) (*SpaceMember, bool, error) {
	if err := member.Validate(); err != nil {
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
	if _, err := s.getPostgresSpace(ctx, tx, scope, member.SpaceID); err != nil {
		return nil, false, err
	}
	members := s.table("space_members")
	existing, err := scanPostgresSpaceMember(tx.QueryRowContext(ctx, `SELECT `+postgresSpaceMemberColumns+` FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND member_type = $4 AND member_id = $5`,
		scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID))
	if err == nil {
		return existing, false, tx.Commit()
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
	return created, true, tx.Commit()
}

func (s *PostgresStore) DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	if _, err := s.getPostgresSpace(ctx, s.db, scope, spaceID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("space_members")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3 AND member_type = $4 AND member_id = $5`,
		scope.Kind, scope.ID, spaceID, memberType, memberID)
	return err
}

// resolvePostgresConversationSpaceTx fills an empty SpaceID with the owner's
// first space by position, else Personal, and verifies an explicit one.
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
		personal, _, ensureErr := s.ensurePostgresPersonalSpaceTx(ctx, tx, scope, conversation.CreatedAt)
		if ensureErr != nil {
			return ensureErr
		}
		spaceID, err = personal.ID, nil
	}
	if err != nil {
		return err
	}
	conversation.SpaceID = spaceID
	return nil
}

func (s *PostgresStore) EnsureMemberHasSpace(ctx context.Context, scope Scope, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error) {
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
	personal, _, err := s.ensurePostgresPersonalSpaceTx(ctx, tx, scope, now)
	if err != nil {
		return nil, err
	}
	member := &SpaceMember{SpaceID: personal.ID, Type: memberType, ID: memberID, AddedAt: now}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM `+members+`
		WHERE scope_kind = $1 AND scope_id = $2 AND space_id = $3`, scope.Kind, scope.ID, personal.ID).Scan(&member.Position); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+members+` (scope_kind, scope_id, `+postgresSpaceMemberColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, scope.Kind, scope.ID, member.SpaceID, member.Type, member.ID, member.Position, member.AddedAt); err != nil {
		return nil, err
	}
	return member, tx.Commit()
}
