package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/lib/pq"
)

func (s *Store) GetAgentSession(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	return s.getPostgresAgentSessionTx(ctx, s.db, scope, sessionID, false)
}

func (s *Store) GetSessionEntry(ctx context.Context, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidSessionEntry, err)
	}
	return s.getPostgresSessionEntryTx(ctx, s.db, scope, entryID, false)
}

// LoadAgentSessionPath builds the path in one REPEATABLE READ read-only
// transaction, so a concurrent append or compaction is either wholly in the
// snapshot or wholly absent.
func (s *Store) LoadAgentSessionPath(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSessionPath, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	leaf, err := s.getPostgresAgentSessionTx(ctx, tx, scope, sessionID, false)
	if err != nil {
		return nil, err
	}
	path, err := s.postgresSessionPathTx(ctx, tx, leaf, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return path, nil
}

// postgresSessionPathTx builds the path ending at leaf (which may be a
// modified copy of the stored row), with extra not-yet-stored entries
// appended to the leaf's segment. Only the entries from the start point on
// are loaded.
func (s *Store) postgresSessionPathTx(ctx context.Context, tx *sql.Tx, leaf *models.AgentSession, extra []*models.SessionEntry) (*models.AgentSessionPath, error) {
	chain := []*models.AgentSession{leaf}
	for current := leaf; current.ParentSessionID != ""; {
		if len(chain) > models.MaxAgentSessionDepth {
			return nil, models.ErrAgentSessionForkTooDeep
		}
		parent, err := s.getPostgresAgentSessionTx(ctx, tx, current.Scope, current.ParentSessionID, false)
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
	last := len(chain) - 1
	bounds := make([]int64, len(chain))
	index := make(map[string]int, len(chain))
	for i, session := range chain {
		index[session.ID] = i
		if i == last {
			bounds[i] = session.NextSequence - 1
		} else {
			bounds[i] = chain[i+1].ForkSequence
		}
	}
	scope := leaf.Scope
	entries := s.table("agent_session_entries")

	// Latest compaction: the newest in the extra entries, else the first hit
	// walking the stored segments from the leaf to the root.
	var compaction *models.SessionEntry
	for i := len(extra) - 1; i >= 0; i-- {
		if extra[i].Type == models.SessionEntryCompaction {
			compaction = extra[i]
			break
		}
	}
	for i := last; compaction == nil && i >= 0; i-- {
		found, err := s.queryPostgresSessionEntries(ctx, tx, `SELECT payload FROM `+entries+`
			WHERE scope_kind = $1 AND scope_id = $2 AND session_id = $3 AND type = 'compaction' AND sequence <= $4
			ORDER BY sequence DESC LIMIT 1`, scope.Kind, scope.ID, chain[i].ID, bounds[i])
		if err != nil {
			return nil, err
		}
		if len(found) == 1 {
			compaction = found[0]
		}
	}

	startSegment, startSequence := 0, int64(1)
	if compaction != nil {
		seg, ok := index[compaction.SessionID]
		if !ok {
			return nil, fmt.Errorf("%w: compaction %s is not on the path", models.ErrInvalidSessionEntry, compaction.ID)
		}
		startSegment, startSequence = seg, compaction.Sequence+1
		if kept := compaction.Compaction.FirstKeptEntryID; kept != "" {
			keptSession, keptSequence, found := "", int64(0), false
			for _, entry := range extra {
				if entry.ID == kept {
					keptSession, keptSequence, found = entry.SessionID, entry.Sequence, true
					break
				}
			}
			if !found {
				err := tx.QueryRowContext(ctx, `SELECT session_id, sequence FROM `+entries+`
					WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, kept).Scan(&keptSession, &keptSequence)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				found = err == nil
			}
			seg, ok := index[keptSession]
			if !found || !ok || keptSequence < 1 || keptSequence > bounds[seg] {
				return nil, fmt.Errorf("%w: compaction %s keeps %s, which is not on the path", models.ErrInvalidSessionEntry, compaction.ID, kept)
			}
			startSegment, startSequence = seg, keptSequence
		}
	}

	segments := make([][]*models.SessionEntry, len(chain))
	total := 0
	for i := startSegment; i < len(chain); i++ {
		from := int64(1)
		if i == startSegment {
			from = startSequence
		}
		if from > bounds[i] {
			continue
		}
		loaded, err := s.queryPostgresSessionEntries(ctx, tx, `SELECT payload FROM `+entries+`
			WHERE scope_kind = $1 AND scope_id = $2 AND session_id = $3 AND type <> 'compaction'
			  AND sequence >= $4 AND sequence <= $5 ORDER BY sequence LIMIT $6`,
			scope.Kind, scope.ID, chain[i].ID, from, bounds[i], models.MaxAgentSessionPathEntries+1-total)
		if err != nil {
			return nil, err
		}
		total += len(loaded)
		if total > models.MaxAgentSessionPathEntries {
			return nil, models.ErrAgentSessionPathTooLarge
		}
		segments[i] = loaded
	}
	if len(extra) > 0 {
		merged := make([]*models.SessionEntry, 0, len(segments[last])+len(extra))
		merged = append(merged, segments[last]...)
		segments[last] = append(merged, extra...)
	}
	return models.BuildAgentSessionPath(chain, segments, compaction)
}

func (s *Store) ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	filter, err := models.NormalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	entries := s.table("agent_session_entries")
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	arg := func(value interface{}) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"scope_kind = $1", "scope_id = $2"}
	if filter.SessionID != "" {
		where = append(where, "session_id = "+arg(filter.SessionID))
	}
	if filter.RunID != "" {
		where = append(where, "run_id = "+arg(filter.RunID))
	}
	if filter.ActionCallID != "" {
		where = append(where, "action_call_id = "+arg(filter.ActionCallID))
	}
	if filter.StepID != "" {
		where = append(where, "step_id = "+arg(filter.StepID))
	}
	if filter.AfterSequence > 0 {
		where = append(where, "sequence > "+arg(filter.AfterSequence))
	}
	if filter.AfterCompaction {
		where = append(where, `sequence > COALESCE((SELECT MAX(c.sequence) FROM `+entries+` c
			WHERE c.scope_kind = $1 AND c.scope_id = $2 AND c.session_id = `+arg(filter.SessionID)+` AND c.type = 'compaction'), 0)`)
	}
	if len(filter.Types) > 0 {
		types := make([]string, len(filter.Types))
		for i, kind := range filter.Types {
			types[i] = string(kind)
		}
		where = append(where, "type = ANY("+arg(pq.Array(types))+")")
	}
	if len(filter.States) > 0 || len(filter.ToolNames) > 0 {
		where = append(where, "type = 'tool'")
	}
	if len(filter.States) > 0 {
		states := make([]string, len(filter.States))
		for i, state := range filter.States {
			states[i] = string(state)
		}
		where = append(where, "state = ANY("+arg(pq.Array(states))+")")
	}
	if len(filter.ToolNames) > 0 {
		where = append(where, "payload->'tool'->>'name' = ANY("+arg(pq.Array(filter.ToolNames))+")")
	}
	if len(filter.NoteKinds) > 0 {
		kinds := make([]string, len(filter.NoteKinds))
		for i, kind := range filter.NoteKinds {
			kinds[i] = string(kind)
		}
		where = append(where, "type = 'note'", "payload->'note'->>'kind' = ANY("+arg(pq.Array(kinds))+")")
	}
	direction := "ASC"
	if filter.Descending {
		direction = "DESC"
	}
	order := "sequence " + direction
	if filter.SessionID == "" {
		order = "created_at " + direction + ", session_id " + direction + ", sequence " + direction
	}
	query := `SELECT payload FROM ` + entries + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + order + ` LIMIT ` + arg(filter.Limit)
	out, err := s.queryPostgresSessionEntries(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*models.SessionEntry{}
	}
	return out, nil
}
