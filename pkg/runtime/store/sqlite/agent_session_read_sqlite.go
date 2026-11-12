package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *Store) GetAgentSession(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	return getSQLiteAgentSession(ctx, s.db, scope, sessionID)
}

func (s *Store) GetSessionEntry(ctx context.Context, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidSessionEntry, err)
	}
	return getSQLiteSessionEntry(ctx, s.db, scope, entryID)
}

// LoadAgentSessionPath reads the chain, the latest compaction and the
// segments on one connection inside BEGIN, so a concurrent append or
// compaction is either wholly visible or not at all.
func (s *Store) LoadAgentSessionPath(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSessionPath, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	leaf, err := getSQLiteAgentSession(ctx, conn, scope, sessionID)
	if err != nil {
		return nil, err
	}
	return loadSQLiteSessionPath(ctx, conn, leaf, nil)
}

// loadSQLiteSessionPath builds the path ending at leaf (which may be a
// modified, or not yet stored, copy of the row), with extra entries appended
// to the leaf's stored segment. It loads only the entries from the start point
// of the latest compaction onward.
func loadSQLiteSessionPath(ctx context.Context, q sqlstore.Reader, leaf *models.AgentSession, extra []*models.SessionEntry) (*models.AgentSessionPath, error) {
	chain := []*models.AgentSession{leaf}
	for current := leaf; current.ParentSessionID != ""; {
		if len(chain) > models.MaxAgentSessionDepth {
			return nil, models.ErrAgentSessionForkTooDeep
		}
		parent, err := getSQLiteAgentSession(ctx, q, current.Scope, current.ParentSessionID)
		if errors.Is(err, models.ErrAgentSessionNotFound) {
			return nil, fmt.Errorf("%w: parent session %s is missing", models.ErrInvalidAgentSession, current.ParentSessionID)
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, parent)
		current = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	index := make(map[string]int, len(chain))
	bounds := make([]int64, len(chain))
	for i, session := range chain {
		index[session.ID] = i
		if i == len(chain)-1 {
			bounds[i] = session.NextSequence - 1
		} else {
			bounds[i] = chain[i+1].ForkSequence
		}
	}

	// Start point: the latest stored compaction's first kept entry (or the
	// compaction itself, so the builder sees it). A compaction among the
	// extra entries can keep anything on the path, so load it all then.
	startSegment, startSequence := 0, int64(1)
	extraCompaction := false
	for _, entry := range extra {
		if entry != nil && entry.Type == models.SessionEntryCompaction {
			extraCompaction = true
		}
	}
	if !extraCompaction {
		for i := len(chain) - 1; i >= 0; i-- {
			compaction, err := latestSQLiteSessionEntry(ctx, q, chain[i], models.SessionEntryCompaction, bounds[i])
			if err != nil {
				return nil, err
			}
			if compaction == nil {
				continue
			}
			if compaction.Compaction == nil {
				return nil, fmt.Errorf("%w: compaction %s has no payload", models.ErrInvalidSessionEntry, compaction.ID)
			}
			startSegment, startSequence = i, compaction.Sequence
			if kept := compaction.Compaction.FirstKeptEntryID; kept != "" {
				var keptSession string
				var keptSequence int64
				err := q.QueryRowContext(ctx, `SELECT session_id, sequence FROM agent_session_entries
					WHERE scope_kind = ? AND scope_id = ? AND id = ?`, leaf.Scope.Kind, leaf.Scope.ID, kept).Scan(&keptSession, &keptSequence)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				seg, ok := index[keptSession]
				if err != nil || !ok || keptSequence > bounds[seg] || seg > i || (seg == i && keptSequence > compaction.Sequence) {
					return nil, fmt.Errorf("%w: compaction %s keeps %s, which is not on the path", models.ErrInvalidSessionEntry, compaction.ID, kept)
				}
				startSegment, startSequence = seg, keptSequence
			}
			break
		}
	}

	segments := make([][]*models.SessionEntry, len(chain))
	for i := startSegment; i < len(chain); i++ {
		from := int64(1)
		if i == startSegment {
			from = startSequence
		}
		entries, err := querySQLiteSessionEntries(ctx, q, `SELECT payload FROM agent_session_entries
			WHERE scope_kind = ? AND scope_id = ? AND session_id = ? AND sequence >= ? AND sequence <= ?
			ORDER BY sequence`, chain[i].Scope.Kind, chain[i].Scope.ID, chain[i].ID, from, bounds[i])
		if err != nil {
			return nil, err
		}
		segments[i] = entries
	}
	if len(extra) > 0 {
		last := len(chain) - 1
		segments[last] = append(segments[last], extra...)
	}
	return models.BuildAgentSessionPath(chain, segments, nil)
}

func (s *Store) ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	filter, err := models.NormalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	query := `SELECT payload FROM agent_session_entries WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	for _, column := range []struct {
		name, value string
	}{
		{"session_id", filter.SessionID},
		{"run_id", filter.RunID},
		{"action_call_id", filter.ActionCallID},
		{"step_id", filter.StepID},
	} {
		if column.value != "" {
			query += ` AND ` + column.name + ` = ?`
			args = append(args, column.value)
		}
	}
	if filter.SessionID != "" {
		query += ` AND sequence > ?`
		args = append(args, filter.AfterSequence)
		if filter.AfterCompaction {
			query += ` AND sequence > COALESCE((SELECT MAX(sequence) FROM agent_session_entries
				WHERE scope_kind = ? AND scope_id = ? AND session_id = ? AND type = 'compaction'), 0)`
			args = append(args, filter.Scope.Kind, filter.Scope.ID, filter.SessionID)
		}
	}
	if len(filter.Types) > 0 {
		query += ` AND type IN (` + sqlitePlaceholders(len(filter.Types)) + `)`
		for _, kind := range filter.Types {
			args = append(args, string(kind))
		}
	}
	if len(filter.States) > 0 {
		query += ` AND type = 'tool' AND state IN (` + sqlitePlaceholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	direction := " ASC"
	if filter.Descending {
		direction = " DESC"
	}
	if filter.SessionID != "" {
		query += ` ORDER BY sequence` + direction
	} else {
		query += ` ORDER BY created_at` + direction + `, session_id` + direction + `, sequence` + direction
	}
	// Tool names and note kinds live in the payload; filter those in Go.
	if len(filter.ToolNames) == 0 && len(filter.NoteKinds) == 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.SessionEntry{}
	for rows.Next() && len(out) < filter.Limit {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		entry, err := decodeSQLiteSessionEntry(payload)
		if err != nil {
			return nil, err
		}
		if models.SessionEntryMatchesFilter(entry, filter) {
			out = append(out, entry)
		}
	}
	return out, rows.Err()
}

func sqlitePlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
