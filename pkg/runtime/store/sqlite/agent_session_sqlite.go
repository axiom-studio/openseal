package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// SQLite backend of AgentSessionStore (run-loop SPEC §2.1, section 01 §2.2).
// Writes run on one connection in BEGIN IMMEDIATE (the SQLite write lock);
// the path load runs on one connection in BEGIN, so every path query reads
// the same snapshot (SPEC §4.7). The …Tx methods are the in-transaction
// variants for stores that already hold a BEGIN IMMEDIATE connection (child
// Run, dependency group, AgentRequest and ConversationTask create, promotion).

var _ models.AgentSessionStore = (*SQLiteStore)(nil)

// migrateAgentSessionLogSQLite mirrors PostgreSQL migration 75 and is safe to
// replay on every open. It creates tables only.
func migrateAgentSessionLogSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS agent_sessions (
			scope_kind        TEXT     NOT NULL,
			scope_id          TEXT     NOT NULL,
			id                TEXT     NOT NULL,
			kind              TEXT     NOT NULL CHECK (kind IN ('conversation','run')),
			lane_key          TEXT     NOT NULL DEFAULT '',
			conversation_id   TEXT     NOT NULL DEFAULT '',
			agent_id          TEXT     NOT NULL,
			owner_run_id      TEXT     NOT NULL DEFAULT '',
			parent_session_id TEXT     NOT NULL DEFAULT '',
			fork_sequence     INTEGER  NOT NULL DEFAULT 0 CHECK (fork_sequence >= 0),
			root_session_id   TEXT     NOT NULL,
			depth             INTEGER  NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 8),
			next_sequence     INTEGER  NOT NULL DEFAULT 1 CHECK (next_sequence > 0),
			channel_watermark INTEGER  NOT NULL DEFAULT 0 CHECK (channel_watermark >= 0),
			revision          INTEGER  NOT NULL CHECK (revision > 0),
			created_at        DATETIME NOT NULL,
			updated_at        DATETIME NOT NULL,
			payload           TEXT     NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK ((kind = 'conversation') = (lane_key <> '')),
			CHECK ((kind = 'run') = (owner_run_id <> '')),
			CHECK ((parent_session_id = '') = (depth = 0))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_sessions_lane_idx
			ON agent_sessions (scope_kind, scope_id, lane_key) WHERE kind = 'conversation'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_sessions_owner_run_idx
			ON agent_sessions (scope_kind, scope_id, owner_run_id) WHERE kind = 'run'`,
		`CREATE INDEX IF NOT EXISTS agent_sessions_parent_idx
			ON agent_sessions (scope_kind, scope_id, parent_session_id) WHERE parent_session_id <> ''`,
		`CREATE TABLE IF NOT EXISTS agent_session_entries (
			scope_kind     TEXT     NOT NULL,
			scope_id       TEXT     NOT NULL,
			session_id     TEXT     NOT NULL,
			sequence       INTEGER  NOT NULL CHECK (sequence > 0),
			id             TEXT     NOT NULL,
			run_id         TEXT     NOT NULL,
			step_id        TEXT     NOT NULL DEFAULT '',
			step           INTEGER  NOT NULL DEFAULT 0 CHECK (step >= 0),
			type           TEXT     NOT NULL CHECK (type IN ('user','assistant','tool','compaction','note','model_change')),
			state          TEXT     NOT NULL DEFAULT '' CHECK (state IN ('','pending','awaiting_approval','awaiting_child','awaiting_event','running','completed','error','interrupted')),
			tool_call_id   TEXT     NOT NULL DEFAULT '',
			action_call_id TEXT     NOT NULL DEFAULT '',
			source_key     TEXT     NOT NULL DEFAULT '',
			tokens         INTEGER  NOT NULL DEFAULT 0 CHECK (tokens >= 0),
			revision       INTEGER  NOT NULL CHECK (revision > 0),
			created_at     DATETIME NOT NULL,
			updated_at     DATETIME NOT NULL,
			payload        TEXT     NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, session_id, sequence),
			UNIQUE (scope_kind, scope_id, id),
			CHECK ((type = 'tool') = (state <> '')),
			CHECK ((type = 'tool') = (tool_call_id <> '')),
			FOREIGN KEY (scope_kind, scope_id, session_id)
				REFERENCES agent_sessions (scope_kind, scope_id, id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_session_entries_source_idx
			ON agent_session_entries (scope_kind, scope_id, session_id, source_key) WHERE source_key <> ''`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_compaction_idx
			ON agent_session_entries (scope_kind, scope_id, session_id, sequence DESC) WHERE type = 'compaction'`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_run_idx
			ON agent_session_entries (scope_kind, scope_id, run_id, created_at, session_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_step_idx
			ON agent_session_entries (scope_kind, scope_id, step_id) WHERE step_id <> ''`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_action_idx
			ON agent_session_entries (scope_kind, scope_id, action_call_id) WHERE action_call_id <> ''`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_open_tool_idx
			ON agent_session_entries (scope_kind, scope_id, run_id)
			WHERE type = 'tool' AND state IN ('pending','awaiting_approval','awaiting_child','awaiting_event','running')`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sqliteSessionTime is the fixed-width UTC text form of the time columns, so
// ORDER BY created_at is temporal. The payload keeps the exact value.
func sqliteSessionTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// ---------------------------------------------------------------------------
// Rows

func getSQLiteAgentSession(ctx context.Context, q sqlstore.Reader, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	var payload string
	err := q.QueryRowContext(ctx, `SELECT payload FROM agent_sessions WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, sessionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrAgentSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	var session models.AgentSession
	if err := json.Unmarshal([]byte(payload), &session); err != nil {
		return nil, fmt.Errorf("decode agent session: %w", err)
	}
	return &session, nil
}

func insertSQLiteAgentSession(ctx context.Context, conn *sql.Conn, session *models.AgentSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_sessions
		(scope_kind, scope_id, id, kind, lane_key, conversation_id, agent_id, owner_run_id, parent_session_id,
		 fork_sequence, root_session_id, depth, next_sequence, channel_watermark, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.Scope.Kind, session.Scope.ID, session.ID, session.Kind, session.LaneKey, session.ConversationID,
		session.AgentID, session.OwnerRunID, session.ParentSessionID, session.ForkSequence, session.RootSessionID,
		session.Depth, session.NextSequence, session.ChannelWatermark, session.Revision,
		sqliteSessionTime(session.CreatedAt), sqliteSessionTime(session.UpdatedAt), string(payload))
	if sqliteUniqueViolation(err) {
		return models.ErrAgentSessionConflict
	}
	return err
}

func updateSQLiteAgentSession(ctx context.Context, conn *sql.Conn, session *models.AgentSession, expectedRevision int64) error {
	if err := session.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_sessions SET next_sequence = ?, channel_watermark = ?, revision = ?,
		updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		session.NextSequence, session.ChannelWatermark, session.Revision, sqliteSessionTime(session.UpdatedAt),
		string(payload), session.Scope.Kind, session.Scope.ID, session.ID, expectedRevision)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return models.ErrAgentSessionConflict
	}
	return nil
}

func decodeSQLiteSessionEntry(payload string) (*models.SessionEntry, error) {
	var entry models.SessionEntry
	if err := json.Unmarshal([]byte(payload), &entry); err != nil {
		return nil, fmt.Errorf("decode agent session entry: %w", err)
	}
	return &entry, nil
}

func getSQLiteSessionEntry(ctx context.Context, q sqlstore.Reader, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	var payload string
	err := q.QueryRowContext(ctx, `SELECT payload FROM agent_session_entries WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, entryID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrSessionEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeSQLiteSessionEntry(payload)
}

func querySQLiteSessionEntries(ctx context.Context, q sqlstore.Reader, query string, args ...interface{}) ([]*models.SessionEntry, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.SessionEntry
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		entry, err := decodeSQLiteSessionEntry(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// latestSQLiteSessionEntry is the highest-sequence entry of one type in a
// session at or below bound, or nil.
func latestSQLiteSessionEntry(ctx context.Context, q sqlstore.Reader, session *models.AgentSession, kind models.SessionEntryType, bound int64) (*models.SessionEntry, error) {
	entries, err := querySQLiteSessionEntries(ctx, q, `SELECT payload FROM agent_session_entries
		WHERE scope_kind = ? AND scope_id = ? AND session_id = ? AND type = ? AND sequence <= ?
		ORDER BY sequence DESC LIMIT 1`, session.Scope.Kind, session.Scope.ID, session.ID, kind, bound)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return entries[0], nil
}

func insertSQLiteSessionEntry(ctx context.Context, conn *sql.Conn, entry *models.SessionEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_session_entries
		(scope_kind, scope_id, session_id, sequence, id, run_id, step_id, step, type, state, tool_call_id,
		 action_call_id, source_key, tokens, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Scope.Kind, entry.Scope.ID, entry.SessionID, entry.Sequence, entry.ID, entry.RunID, entry.StepID,
		entry.Step, entry.Type, entry.State, entry.ToolCallID, entry.ActionCallID, entry.SourceKey, entry.Tokens,
		entry.Revision, sqliteSessionTime(entry.CreatedAt), sqliteSessionTime(entry.UpdatedAt), string(payload))
	if sqliteUniqueViolation(err) {
		return models.ErrAgentSessionConflict
	}
	return err
}

func updateSQLiteSessionEntry(ctx context.Context, conn *sql.Conn, entry *models.SessionEntry, expectedRevision int64) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_session_entries SET state = ?, action_call_id = ?, tokens = ?,
		revision = ?, updated_at = ?, payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		entry.State, entry.ActionCallID, entry.Tokens, entry.Revision, sqliteSessionTime(entry.UpdatedAt),
		string(payload), entry.Scope.Kind, entry.Scope.ID, entry.ID, expectedRevision)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return models.ErrToolEntryStateConflict
	}
	return nil
}

// sqliteSessionRunLease is the lease check of UpdateAgentRunWithEvent on a
// write connection. The expiry is compared at full precision from the payload.
func sqliteSessionRunLease(ctx context.Context, conn *sql.Conn, scope models.Scope, runID string, lease models.AgentRunLeaseGuard) (*models.AgentRun, error) {
	run, err := getSQLiteAgentRun(ctx, conn, scope, runID)
	if err != nil {
		return nil, err
	}
	if run.LeaseOwner != lease.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(lease.Now) {
		return nil, models.ErrLeaseLost
	}
	return run, nil
}

func insertSQLiteSessionEvents(ctx context.Context, conn *sql.Conn, events []*models.ActivityEvent, now time.Time) ([]*models.ActivityEvent, error) {
	if len(events) == 0 {
		return nil, nil
	}
	out := make([]*models.ActivityEvent, 0, len(events))
	for _, event := range events {
		event = models.CloneActivityEvent(event)
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		persisted, err := insertSQLiteActivityConn(ctx, conn, event)
		if err != nil {
			return nil, err
		}
		out = append(out, persisted)
	}
	return out, nil
}

// withSQLiteSessionWrite runs fn on one connection in BEGIN IMMEDIATE and
// commits when fn succeeds.
func (s *SQLiteStore) withSQLiteSessionWrite(ctx context.Context, fn func(*sql.Conn) error) error {
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

// ---------------------------------------------------------------------------
// Open / create

func (s *SQLiteStore) OpenConversationAgentSession(ctx context.Context, req models.OpenConversationAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	if strings.TrimSpace(req.ConversationID) == "" || strings.TrimSpace(req.AgentID) == "" ||
		strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.Lease.WorkerID) == "" {
		return nil, fmt.Errorf("%w: conversationId, agentId, runId and a lease are required", models.ErrInvalidAgentSession)
	}
	now := sessionNow(req.Now)
	lane := models.ConversationRunLane(req.ConversationID, req.ThreadRootID, req.AgentID)
	id := models.ConversationAgentSessionID(req.Scope, lane)

	var result *models.OpenAgentSessionResult
	err := s.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		run, err := sqliteSessionRunLease(ctx, conn, req.Scope, req.RunID, req.Lease)
		if err != nil {
			return err
		}
		if run.SessionID != id {
			return models.ErrSessionMoved
		}
		current, err := getSQLiteAgentSession(ctx, conn, req.Scope, id)
		switch {
		case err == nil:
			if current.Kind != models.AgentSessionKindConversation || current.LaneKey != lane || current.AgentID != req.AgentID {
				return models.ErrAgentSessionConflict
			}
			result = &models.OpenAgentSessionResult{Session: current}
			return nil
		case !errors.Is(err, models.ErrAgentSessionNotFound):
			return err
		}
		session := &models.AgentSession{
			ID:             id,
			Scope:          req.Scope,
			Kind:           models.AgentSessionKindConversation,
			LaneKey:        lane,
			ConversationID: req.ConversationID,
			ThreadRootID:   req.ThreadRootID,
			AgentID:        req.AgentID,
			RootSessionID:  id,
			NextSequence:   1,
			Revision:       1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := insertSQLiteAgentSession(ctx, conn, session); err != nil {
			return err
		}
		result = &models.OpenAgentSessionResult{Session: session, Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CreateRunAgentSession creates the parentless run session of OwnerRunID at its
// first claim, fenced by that Run's lease. The Run must be unbound or already
// bound to this session; a Run bound elsewhere gets ErrSessionMoved.
func (s *SQLiteStore) CreateRunAgentSession(ctx context.Context, req models.CreateRunAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
	txReq := models.CreateRunAgentSessionTxRequest{
		Scope:          req.Scope,
		OwnerRunID:     req.OwnerRunID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		InitialEntries: req.InitialEntries,
		Now:            sessionNow(req.Now),
	}
	if strings.TrimSpace(req.Lease.WorkerID) == "" {
		return nil, fmt.Errorf("%w: a lease is required", models.ErrInvalidAgentSession)
	}
	if err := models.ValidateCreateRunAgentSessionTxRequest(txReq); err != nil {
		return nil, err
	}
	var result *models.OpenAgentSessionResult
	err := s.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		run, err := sqliteSessionRunLease(ctx, conn, req.Scope, req.OwnerRunID, req.Lease)
		if err != nil {
			return err
		}
		if id := models.RunAgentSessionID(req.Scope, req.OwnerRunID); run.SessionID != "" && run.SessionID != id {
			return models.ErrSessionMoved
		}
		result, err = createSQLiteRunAgentSessionCore(ctx, conn, txReq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// createRunAgentSessionTx is the in-transaction create of a run session. conn
// holds BEGIN IMMEDIATE and the caller owns the fence. The owner Run must
// already be stored; its SessionID is set to the new session in the same
// transaction (Revision+1 when it changes). A replay with the same parent and
// Agent returns the existing session with Created=false and writes nothing.
func (s *SQLiteStore) createRunAgentSessionTx(ctx context.Context, conn *sql.Conn, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateCreateRunAgentSessionTxRequest(req); err != nil {
		return nil, err
	}
	return createSQLiteRunAgentSessionCore(ctx, conn, req)
}

func createSQLiteRunAgentSessionCore(ctx context.Context, conn *sql.Conn, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	id := models.RunAgentSessionID(req.Scope, req.OwnerRunID)
	run, err := getSQLiteAgentRun(ctx, conn, req.Scope, req.OwnerRunID)
	if err != nil {
		return nil, err
	}
	current, err := getSQLiteAgentSession(ctx, conn, req.Scope, id)
	switch {
	case err == nil:
		if current.Kind != models.AgentSessionKindRun || current.OwnerRunID != req.OwnerRunID ||
			current.ParentSessionID != req.ParentSessionID || current.AgentID != req.AgentID {
			return nil, models.ErrAgentSessionConflict
		}
		return &models.OpenAgentSessionResult{Session: current}, nil
	case !errors.Is(err, models.ErrAgentSessionNotFound):
		return nil, err
	}
	session := &models.AgentSession{
		ID:             id,
		Scope:          req.Scope,
		Kind:           models.AgentSessionKindRun,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		AgentID:        req.AgentID,
		OwnerRunID:     req.OwnerRunID,
		RootSessionID:  id,
		NextSequence:   1,
		Revision:       1,
		CreatedAt:      req.Now,
		UpdatedAt:      req.Now,
	}
	if req.ParentSessionID != "" {
		parent, err := getSQLiteAgentSession(ctx, conn, req.Scope, req.ParentSessionID)
		if err != nil {
			return nil, err
		}
		if req.ForkSequence > parent.NextSequence-1 {
			return nil, fmt.Errorf("%w: fork sequence %d is past the parent leaf %d", models.ErrInvalidAgentSession, req.ForkSequence, parent.NextSequence-1)
		}
		if parent.Depth+1 > models.MaxAgentSessionDepth {
			return nil, models.ErrAgentSessionForkTooDeep
		}
		session.ParentSessionID = parent.ID
		session.ForkSequence = req.ForkSequence
		if req.ForkSequence > 0 {
			if err := conn.QueryRowContext(ctx, `SELECT id FROM agent_session_entries
				WHERE scope_kind = ? AND scope_id = ? AND session_id = ? AND sequence = ?`,
				req.Scope.Kind, req.Scope.ID, parent.ID, req.ForkSequence).Scan(&session.ForkEntryID); err != nil {
				return nil, fmt.Errorf("fork entry %d of session %s: %w", req.ForkSequence, parent.ID, err)
			}
		}
		session.RootSessionID = parent.RootSessionID
		session.Depth = parent.Depth + 1
		session.ChannelWatermark = parent.ChannelWatermark
		if session.ConversationID == "" {
			session.ConversationID = parent.ConversationID
		}
		if session.ThreadRootID == "" {
			session.ThreadRootID = parent.ThreadRootID
		}
		session.ModelProvider, session.Model, err = sqliteForkModel(ctx, conn, parent, req.ForkSequence)
		if err != nil {
			return nil, err
		}
	}
	if err := session.Validate(); err != nil {
		return nil, err
	}

	var plan *sqliteAppendPlan
	if len(req.InitialEntries) > 0 {
		plan, err = planSQLiteAppend(ctx, conn, models.InitialEntriesAppendRequest(req, id), session, nil)
		if err != nil {
			return nil, err
		}
	}
	if err := insertSQLiteAgentSession(ctx, conn, session); err != nil {
		return nil, err
	}
	stored := session
	if plan != nil {
		if err := commitSQLiteAppend(ctx, conn, plan, session.Revision); err != nil {
			return nil, err
		}
		stored = plan.session
	}
	if run.SessionID != id {
		next := models.CloneAgentRun(run)
		next.SessionID = id
		next.Revision++
		next.UpdatedAt = req.Now
		if err := updateSQLiteAgentRunConn(ctx, conn, next, run.Revision); err != nil {
			return nil, err
		}
	}
	return &models.OpenAgentSessionResult{Session: stored, Created: true}, nil
}

// sqliteForkModel is the model of the latest model_change entry on the
// parent's chain up to the fork point, or the parent's own model when the
// chain has none in bounds.
func sqliteForkModel(ctx context.Context, q sqlstore.Reader, parent *models.AgentSession, forkSequence int64) (string, string, error) {
	session, bound := parent, forkSequence
	for depth := 0; depth <= models.MaxAgentSessionDepth; depth++ {
		change, err := latestSQLiteSessionEntry(ctx, q, session, models.SessionEntryModelChange, bound)
		if err != nil {
			return "", "", err
		}
		if change != nil && change.ModelChange != nil {
			return change.ModelChange.Provider, change.ModelChange.Model, nil
		}
		if session.ParentSessionID == "" {
			break
		}
		bound = session.ForkSequence
		next, err := getSQLiteAgentSession(ctx, q, session.Scope, session.ParentSessionID)
		if errors.Is(err, models.ErrAgentSessionNotFound) {
			break
		}
		if err != nil {
			return "", "", err
		}
		session = next
	}
	return parent.ModelProvider, parent.Model, nil
}

// ---------------------------------------------------------------------------
// Reads

func (s *SQLiteStore) GetAgentSession(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	return getSQLiteAgentSession(ctx, s.db, scope, sessionID)
}

func (s *SQLiteStore) GetSessionEntry(ctx context.Context, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidSessionEntry, err)
	}
	return getSQLiteSessionEntry(ctx, s.db, scope, entryID)
}

// LoadAgentSessionPath reads the chain, the latest compaction and the
// segments on one connection inside BEGIN, so a concurrent append or
// compaction is either wholly visible or not at all.
func (s *SQLiteStore) LoadAgentSessionPath(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSessionPath, error) {
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

func (s *SQLiteStore) ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
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

// ---------------------------------------------------------------------------
// Append

// AppendAgentSessionEntries appends one batch, fenced by the Run lease (or,
// with Lease nil, by the Run being terminal) and by run.SessionID ==
// req.SessionID. The watermark CAS, the intervention cursor, the entries and
// the events commit together.
func (s *SQLiteStore) AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	var result *models.AppendAgentSessionEntriesResult
	err := s.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		var run *models.AgentRun
		var err error
		if req.Lease != nil {
			run, err = sqliteSessionRunLease(ctx, conn, req.Scope, req.RunID, *req.Lease)
		} else {
			run, err = getSQLiteAgentRun(ctx, conn, req.Scope, req.RunID)
		}
		if err != nil {
			return err
		}
		if req.Lease == nil && !models.IsTerminalAgentRunStatus(run.Status) {
			return models.ErrLeaseLost
		}
		if run.SessionID != req.SessionID {
			return models.ErrSessionMoved
		}
		result, err = appendSQLiteAgentSessionEntriesCore(ctx, conn, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// appendAgentSessionEntriesTx is the in-transaction append. conn holds BEGIN
// IMMEDIATE and the caller owns the fence (lease and session binding are not
// checked); everything else, including the watermark CAS and the cursor, is
// as AppendAgentSessionEntries.
func (s *SQLiteStore) appendAgentSessionEntriesTx(ctx context.Context, conn *sql.Conn, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	return appendSQLiteAgentSessionEntriesCore(ctx, conn, req)
}

func appendSQLiteAgentSessionEntriesCore(ctx context.Context, conn *sql.Conn, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	session, err := getSQLiteAgentSession(ctx, conn, req.Scope, req.SessionID)
	if err != nil {
		return nil, err
	}
	if replayed, ok, err := sqliteAppendReplay(ctx, conn, req, session); err != nil || ok {
		return replayed, err
	}
	var run *models.AgentRun
	if req.ConsumeInterventionsThrough != nil {
		run, err = getSQLiteAgentRun(ctx, conn, req.Scope, req.RunID)
		if err != nil {
			return nil, err
		}
		through := *req.ConsumeInterventionsThrough
		if through < run.ConsumedInterventions || through > len(run.PendingInterventions) {
			return nil, models.InvalidSessionEntryf("intervention cursor %d is outside %d..%d", through, run.ConsumedInterventions, len(run.PendingInterventions))
		}
	}
	plan, err := planSQLiteAppend(ctx, conn, req, session, run)
	if err != nil {
		return nil, err
	}
	if err := commitSQLiteAppend(ctx, conn, plan, session.Revision); err != nil {
		return nil, err
	}
	events, err := insertSQLiteSessionEvents(ctx, conn, req.Events, req.Now)
	if err != nil {
		return nil, err
	}
	entries := plan.entries
	if entries == nil {
		entries = []*models.SessionEntry{}
	}
	return &models.AppendAgentSessionEntriesResult{
		Session: plan.session,
		Entries: entries,
		Skipped: plan.skipped,
		Events:  events,
	}, nil
}

// sqliteAppendReplay reports a whole-batch replay: every request entry is
// stored in this session already, or would be skipped by its SourceKey. An
// entry id stored in another session, or a partial batch, is a conflict.
func sqliteAppendReplay(ctx context.Context, conn *sql.Conn, req models.AppendAgentSessionEntriesRequest, session *models.AgentSession) (*models.AppendAgentSessionEntriesResult, bool, error) {
	if len(req.Entries) == 0 {
		return nil, false, nil
	}
	stored := make([]*models.SessionEntry, len(req.Entries))
	found := false
	for i, entry := range req.Entries {
		existing, err := getSQLiteSessionEntry(ctx, conn, req.Scope, entry.ID)
		if errors.Is(err, models.ErrSessionEntryNotFound) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if existing.SessionID != session.ID {
			return nil, false, models.ErrAgentSessionConflict
		}
		stored[i] = existing
		found = true
	}
	if !found {
		return nil, false, nil
	}
	result := &models.AppendAgentSessionEntriesResult{Session: session, Replayed: true, Entries: []*models.SessionEntry{}}
	for i, entry := range req.Entries {
		if stored[i] != nil {
			result.Entries = append(result.Entries, stored[i])
			continue
		}
		if entry.SourceKey != "" {
			exists, err := sqliteSessionSourceKeyExists(ctx, conn, req.Scope, session.ID, entry.SourceKey)
			if err != nil {
				return nil, false, err
			}
			if exists {
				result.Skipped = append(result.Skipped, entry.ID)
				continue
			}
		}
		return nil, false, models.ErrAgentSessionConflict
	}
	return result, true, nil
}

func sqliteSessionSourceKeyExists(ctx context.Context, q sqlstore.Reader, scope models.Scope, sessionID, sourceKey string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM agent_session_entries
		WHERE scope_kind = ? AND scope_id = ? AND session_id = ? AND source_key = ?`,
		scope.Kind, scope.ID, sessionID, sourceKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func sqliteSessionEntryExists(ctx context.Context, q sqlstore.Reader, scope models.Scope, entryID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM agent_session_entries WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, entryID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type sqliteAppendPlan struct {
	session   *models.AgentSession // the session row after the append
	entries   []*models.SessionEntry
	skipped   []string
	run       *models.AgentRun // the Run row after the cursor write; nil when unchanged
	sessionUp bool             // the session row changes
}

// planSQLiteAppend runs every check and builds the new rows without writing,
// mirroring planMemoryAppendLocked. session is the current row (or the
// not-yet-inserted row of a new session); run is the current Run row when the
// request moves the cursor.
func planSQLiteAppend(ctx context.Context, conn *sql.Conn, req models.AppendAgentSessionEntriesRequest, session *models.AgentSession, run *models.AgentRun) (*sqliteAppendPlan, error) {
	if req.ExpectedChannelWatermark != nil && session.ChannelWatermark != *req.ExpectedChannelWatermark {
		return nil, models.ErrChannelWatermarkMoved
	}
	next := models.CloneAgentSession(session)
	plan := &sqliteAppendPlan{session: next, skipped: []string{}}
	skippedSteps := make(map[string]bool)
	batchKeys := make(map[string]bool)
	for _, entry := range req.Entries {
		if entry.SourceKey != "" {
			exists, err := sqliteSessionSourceKeyExists(ctx, conn, req.Scope, session.ID, entry.SourceKey)
			if err != nil {
				return nil, err
			}
			if exists || batchKeys[entry.SourceKey] {
				plan.skipped = append(plan.skipped, entry.ID)
				if entry.Type == models.SessionEntryAssistant {
					skippedSteps[entry.StepID] = true
				}
				continue
			}
			batchKeys[entry.SourceKey] = true
		}
		if entry.Type == models.SessionEntryTool && len(entry.Tool.Directive) == 0 && skippedSteps[entry.StepID] {
			plan.skipped = append(plan.skipped, entry.ID)
			continue
		}
		exists, err := sqliteSessionEntryExists(ctx, conn, req.Scope, entry.ID)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, models.ErrAgentSessionConflict
		}
		stamped := models.StampAppendedSessionEntry(entry, req, next.NextSequence+int64(len(plan.entries)))
		if err := stamped.Validate(); err != nil {
			return nil, err
		}
		if stamped.Type == models.SessionEntryCompaction {
			leaf := models.CloneAgentSession(session)
			leaf.NextSequence = stamped.Sequence
			path, err := loadSQLiteSessionPath(ctx, conn, leaf, plan.entries)
			if err != nil {
				return nil, err
			}
			if err := models.ValidateCompactionAgainstPath(path, stamped); err != nil {
				return nil, err
			}
		}
		if stamped.Type == models.SessionEntryModelChange {
			next.ModelProvider = stamped.ModelChange.Provider
			next.Model = stamped.ModelChange.Model
		}
		plan.entries = append(plan.entries, stamped)
	}
	if len(plan.entries) > 0 {
		next.NextSequence += int64(len(plan.entries))
		plan.sessionUp = true
	}
	if req.ExpectedChannelWatermark != nil && req.ChannelWatermark != next.ChannelWatermark {
		next.ChannelWatermark = req.ChannelWatermark
		plan.sessionUp = true
	}
	if plan.sessionUp {
		next.Revision++
		next.UpdatedAt = req.Now
	}
	if run != nil && *req.ConsumeInterventionsThrough != run.ConsumedInterventions {
		updated := models.CloneAgentRun(run)
		updated.ConsumedInterventions = *req.ConsumeInterventionsThrough
		updated.Revision++
		updated.UpdatedAt = req.Now
		plan.run = updated
	}
	return plan, nil
}

// commitSQLiteAppend writes a plan inside the caller's transaction.
// sessionRevision is the stored session row's revision before the append.
func commitSQLiteAppend(ctx context.Context, conn *sql.Conn, plan *sqliteAppendPlan, sessionRevision int64) error {
	for _, entry := range plan.entries {
		if err := insertSQLiteSessionEntry(ctx, conn, entry); err != nil {
			return err
		}
	}
	if plan.sessionUp {
		if err := updateSQLiteAgentSession(ctx, conn, plan.session, sessionRevision); err != nil {
			return err
		}
	}
	if plan.run != nil {
		if err := updateSQLiteAgentRunConn(ctx, conn, plan.run, plan.run.Revision-1); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tool entry transitions

// TransitionToolEntry moves one tool entry (CAS on state and revision), fenced
// by the lease of the entry's Run, or, with an empty lease, by that Run being
// terminal (loop-16 terminal close). The session binding is not checked, so a
// promoted Run can still settle entries it left in the parent session.
func (s *SQLiteStore) TransitionToolEntry(ctx context.Context, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	var result *models.ToolEntryTransitionResult
	err := s.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		if req.Lease.WorkerID == "" {
			run, err := getSQLiteAgentRun(ctx, conn, req.Scope, req.RunID)
			if err != nil {
				return err
			}
			if !models.IsTerminalAgentRunStatus(run.Status) {
				return models.ErrLeaseLost
			}
		} else if _, err := sqliteSessionRunLease(ctx, conn, req.Scope, req.RunID, req.Lease); err != nil {
			return err
		}
		var err error
		result, err = transitionSQLiteToolEntryCore(ctx, conn, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// transitionToolEntryTx is the in-transaction transition. conn holds BEGIN
// IMMEDIATE and the caller owns the lease check.
func (s *SQLiteStore) transitionToolEntryTx(ctx context.Context, conn *sql.Conn, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	return transitionSQLiteToolEntryCore(ctx, conn, req)
}

func transitionSQLiteToolEntryCore(ctx context.Context, conn *sql.Conn, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	current, err := getSQLiteSessionEntry(ctx, conn, req.Scope, req.EntryID)
	if err != nil {
		return nil, err
	}
	next, err := models.ApplyToolEntryTransition(current, req)
	if err != nil {
		return nil, err
	}
	if err := updateSQLiteSessionEntry(ctx, conn, next, current.Revision); err != nil {
		return nil, err
	}
	events, err := insertSQLiteSessionEvents(ctx, conn, req.Events, req.Now)
	if err != nil {
		return nil, err
	}
	return &models.ToolEntryTransitionResult{Entry: next, Events: events}, nil
}
