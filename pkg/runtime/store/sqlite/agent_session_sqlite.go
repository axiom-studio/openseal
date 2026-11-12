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

var _ models.AgentSessionStore = (*Store)(nil)

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

// ---------------------------------------------------------------------------
// Open / create

func (s *Store) OpenConversationAgentSession(ctx context.Context, req models.OpenConversationAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
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
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
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
func (s *Store) CreateRunAgentSession(ctx context.Context, req models.CreateRunAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
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
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
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
func (s *Store) createRunAgentSessionTx(ctx context.Context, conn *sql.Conn, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
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
