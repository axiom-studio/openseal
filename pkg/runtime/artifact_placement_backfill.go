package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const artifactPlacementMigrationVersion int64 = 68

const artifactPlacementBackfillPage = 500

// migrateArtifactPlacement places every artifact stored without a space,
// including those made before artifacts recorded their chat and space. It
// runs after migration 67, so every conversation already has its space.
func (s *PostgresStore) migrateArtifactPlacement(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, artifactPlacementMigrationVersion)
	if err != nil || applied {
		return err
	}
	backfill := artifactPlacementBackfill{tx: tx, postgres: true, table: s.table,
		ownerSpace: func(ctx context.Context, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error) {
			return s.postgresOwnerSpaceIDTx(ctx, tx, scope, owner, now)
		}}
	if err := backfill.run(ctx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'artifact space placement') ON CONFLICT(version) DO NOTHING`, artifactPlacementMigrationVersion)
	return err
}

// migrateArtifactPlacementSQLite mirrors PostgreSQL migration 68. It only
// reads artifacts that still have no space, so it is a no-op once they are
// placed.
func migrateArtifactPlacementSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	backfill := artifactPlacementBackfill{tx: tx, table: func(name string) string { return name },
		ownerSpace: func(ctx context.Context, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error) {
			return sqliteOwnerSpaceID(ctx, tx, scope, owner, now)
		}}
	if err := backfill.run(context.Background()); err != nil {
		return err
	}
	return tx.Commit()
}

// artifactPlacementBackfill applies ArtifactPlacement to stored artifacts
// inside a migration transaction: the producing run's chat, else the chat in
// app metadata, gives the space; otherwise the owner's first space, else
// Personal. The fingerprint is recomputed so a replayed registration, which is
// placed the same way, still matches.
type artifactPlacementBackfill struct {
	tx         *sql.Tx
	postgres   bool
	table      func(string) string
	ownerSpace func(context.Context, Scope, *ObjectiveOwner, time.Time) (string, error)
}

type artifactPlacementRow struct {
	scope   Scope
	id      string
	version int64
	payload string
}

func (b artifactPlacementBackfill) run(ctx context.Context) error {
	placement := ArtifactPlacement{Runs: b, Conversations: b, Spaces: b}
	spaceID := "json_extract(payload, '$.provenance.spaceId')"
	if b.postgres {
		spaceID = "payload->'provenance'->>'spaceId'"
	}
	query := b.rebind(`SELECT scope_kind, scope_id, id, version, payload FROM ` + b.table("artifacts") + `
		WHERE COALESCE(` + spaceID + `, '') = '' AND (scope_kind, scope_id, id, version) > (?, ?, ?, ?)
		ORDER BY scope_kind, scope_id, id, version LIMIT ` + strconv.Itoa(artifactPlacementBackfillPage))
	update := b.rebind(`UPDATE ` + b.table("artifacts") + ` SET payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND version = ?`)
	after := artifactPlacementRow{}
	for {
		rows, err := b.page(ctx, query, after)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var artifact Artifact
			if err := json.Unmarshal([]byte(row.payload), &artifact); err != nil {
				return fmt.Errorf("decode artifact %s@%d: %w", row.id, row.version, err)
			}
			artifact.Scope = row.scope
			if err := placement.Place(ctx, &artifact); err != nil {
				return fmt.Errorf("place artifact %s@%d: %w", row.id, row.version, err)
			}
			if artifact.Fingerprint, err = artifactFingerprint(&artifact); err != nil {
				return err
			}
			payload, err := json.Marshal(&artifact)
			if err != nil {
				return err
			}
			if _, err := b.tx.ExecContext(ctx, update, string(payload), row.scope.Kind, row.scope.ID, row.id, row.version); err != nil {
				return err
			}
		}
		if len(rows) < artifactPlacementBackfillPage {
			return nil
		}
		after = rows[len(rows)-1]
	}
}

func (b artifactPlacementBackfill) page(ctx context.Context, query string, after artifactPlacementRow) ([]artifactPlacementRow, error) {
	rows, err := b.tx.QueryContext(ctx, query, after.scope.Kind, after.scope.ID, after.id, after.version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []artifactPlacementRow
	for rows.Next() {
		var row artifactPlacementRow
		if err := rows.Scan(&row.scope.Kind, &row.scope.ID, &row.id, &row.version, &row.payload); err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	return page, rows.Err()
}

func (b artifactPlacementBackfill) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	var payload string
	err := b.tx.QueryRowContext(ctx, b.rebind(`SELECT payload FROM `+b.table("agent_runs")+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`),
		scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRun(payload)
}

func (b artifactPlacementBackfill) GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error) {
	var spaceID string
	err := b.tx.QueryRowContext(ctx, b.rebind(`SELECT space_id FROM `+b.table("conversations")+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`),
		scope.Kind, scope.ID, conversationID).Scan(&spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Conversation{ID: conversationID, Scope: scope, SpaceID: spaceID}, nil
}

func (b artifactPlacementBackfill) OwnerSpaceID(ctx context.Context, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error) {
	return b.ownerSpace(ctx, scope, owner, now)
}

// rebind turns ? placeholders into PostgreSQL's numbered ones.
func (b artifactPlacementBackfill) rebind(query string) string {
	if !b.postgres {
		return query
	}
	var out strings.Builder
	next := 1
	for _, r := range query {
		if r == '?' {
			out.WriteString("$" + strconv.Itoa(next))
			next++
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
