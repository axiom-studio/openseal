package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// Postgres backend of AgentSessionStore (run-loop SPEC §2.1, §4.1, §4.7 and
// section 01 §4). Lock order inside every write transaction is Run row ->
// session row -> entry row. Path loads run in one REPEATABLE READ, read-only
// transaction. The …Tx helpers are the in-transaction variants for stores
// that already hold the Run row lock (child Run create, promotion).

var _ models.AgentSessionStore = (*Store)(nil)

func postgresSessionNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// lockPostgresSessionRunTx locks the Run row (FOR UPDATE) and checks the
// lease as UpdateAgentRunWithEvent does.
func (s *Store) lockPostgresSessionRunTx(ctx context.Context, tx *sql.Tx, scope models.Scope, runID string, lease *models.AgentRunLeaseGuard) (*models.AgentRun, error) {
	run, err := s.getPostgresAgentRunTx(ctx, tx, scope, runID, true)
	if err != nil {
		return nil, err
	}
	if lease != nil && (run.LeaseOwner != lease.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(lease.Now)) {
		return nil, models.ErrLeaseLost
	}
	return run, nil
}

func (s *Store) getPostgresAgentSessionTx(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, sessionID string, lock bool) (*models.AgentSession, error) {
	query := `SELECT payload FROM ` + s.table("agent_sessions") + ` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, sessionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrAgentSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	var session models.AgentSession
	if err := json.Unmarshal([]byte(payload), &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// insertPostgresAgentSessionTx inserts a validated session row. It reports
// false (and writes nothing) when a row with the same id, lane or owner
// already exists.
func (s *Store) insertPostgresAgentSessionTx(ctx context.Context, tx *sql.Tx, session *models.AgentSession) (bool, error) {
	if err := session.Validate(); err != nil {
		return false, err
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_sessions")+`
		(scope_kind, scope_id, id, kind, lane_key, conversation_id, agent_id, owner_run_id, parent_session_id,
		 fork_sequence, root_session_id, depth, next_sequence, channel_watermark, revision, created_at, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::jsonb)
		ON CONFLICT DO NOTHING`,
		session.Scope.Kind, session.Scope.ID, session.ID, session.Kind, session.LaneKey, session.ConversationID, session.AgentID,
		session.OwnerRunID, session.ParentSessionID, session.ForkSequence, session.RootSessionID, session.Depth,
		session.NextSequence, session.ChannelWatermark, session.Revision, session.CreatedAt, session.UpdatedAt, string(payload))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) updatePostgresAgentSessionTx(ctx context.Context, tx *sql.Tx, session *models.AgentSession, expectedRevision int64) error {
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_sessions")+` SET next_sequence = $1, channel_watermark = $2,
		revision = $3, updated_at = $4, payload = $5::jsonb WHERE scope_kind = $6 AND scope_id = $7 AND id = $8 AND revision = $9`,
		session.NextSequence, session.ChannelWatermark, session.Revision, session.UpdatedAt, string(payload),
		session.Scope.Kind, session.Scope.ID, session.ID, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return models.ErrAgentSessionConflict
	}
	return nil
}

func decodePostgresSessionEntries(rows *sql.Rows) ([]*models.SessionEntry, error) {
	defer rows.Close()
	var entries []*models.SessionEntry
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var entry models.SessionEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, &entry)
	}
	return entries, rows.Err()
}

func (s *Store) queryPostgresSessionEntries(ctx context.Context, queryer sqlstore.Reader, query string, args ...interface{}) ([]*models.SessionEntry, error) {
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return decodePostgresSessionEntries(rows)
}

func (s *Store) getPostgresSessionEntryTx(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, entryID string, lock bool) (*models.SessionEntry, error) {
	query := `SELECT payload FROM ` + s.table("agent_session_entries") + ` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, entryID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrSessionEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	var entry models.SessionEntry
	if err := json.Unmarshal([]byte(payload), &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// insertPostgresSessionEntriesTx stores stamped entries in one multi-row INSERT.
func (s *Store) insertPostgresSessionEntriesTx(ctx context.Context, tx *sql.Tx, entries []*models.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	const columns = 18
	var query strings.Builder
	query.WriteString(`INSERT INTO ` + s.table("agent_session_entries") + `
		(scope_kind, scope_id, session_id, sequence, id, run_id, step_id, step, type, state, tool_call_id,
		 action_call_id, source_key, tokens, revision, created_at, updated_at, payload) VALUES `)
	args := make([]interface{}, 0, len(entries)*columns)
	for i, entry := range entries {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if i > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(")
		for c := 1; c <= columns; c++ {
			if c > 1 {
				query.WriteString(", ")
			}
			query.WriteString("$" + strconv.Itoa(i*columns+c))
			if c == columns {
				query.WriteString("::jsonb")
			}
		}
		query.WriteString(")")
		args = append(args, entry.Scope.Kind, entry.Scope.ID, entry.SessionID, entry.Sequence, entry.ID, entry.RunID,
			entry.StepID, entry.Step, entry.Type, entry.State, entry.ToolCallID, entry.ActionCallID, entry.SourceKey,
			entry.Tokens, entry.Revision, entry.CreatedAt, entry.UpdatedAt, string(payload))
	}
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		if postgresUniqueViolation(err) {
			return models.ErrAgentSessionConflict
		}
		return err
	}
	return nil
}

func (s *Store) insertPostgresSessionEventsTx(ctx context.Context, tx *sql.Tx, events []*models.ActivityEvent, now time.Time) ([]*models.ActivityEvent, error) {
	if len(events) == 0 {
		return nil, nil
	}
	out := make([]*models.ActivityEvent, 0, len(events))
	for _, event := range events {
		event = models.CloneActivityEvent(event)
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
		if err != nil {
			return nil, err
		}
		out = append(out, persisted)
	}
	return out, nil
}

func (s *Store) beginPostgresSessionTx(ctx context.Context, run func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
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
	now := postgresSessionNow(req.Now)
	lane := models.ConversationRunLane(req.ConversationID, req.ThreadRootID, req.AgentID)
	id := models.ConversationAgentSessionID(req.Scope, lane)
	var result *models.OpenAgentSessionResult
	err := s.beginPostgresSessionTx(ctx, func(tx *sql.Tx) error {
		lease := req.Lease
		run, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.RunID, &lease)
		if err != nil {
			return err
		}
		if run.SessionID != id {
			return models.ErrSessionMoved
		}
		session := &models.AgentSession{
			ID: id, Scope: req.Scope, Kind: models.AgentSessionKindConversation, LaneKey: lane,
			ConversationID: req.ConversationID, ThreadRootID: req.ThreadRootID, AgentID: req.AgentID,
			RootSessionID: id, NextSequence: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		created, err := s.insertPostgresAgentSessionTx(ctx, tx, session)
		if err != nil {
			return err
		}
		if created {
			result = &models.OpenAgentSessionResult{Session: session, Created: true}
			return nil
		}
		current, err := s.getPostgresAgentSessionTx(ctx, tx, req.Scope, id, true)
		if errors.Is(err, models.ErrAgentSessionNotFound) {
			// The lane index holds another id for this lane.
			return models.ErrAgentSessionConflict
		}
		if err != nil {
			return err
		}
		if current.Kind != models.AgentSessionKindConversation || current.LaneKey != lane || current.AgentID != req.AgentID {
			return models.ErrAgentSessionConflict
		}
		result = &models.OpenAgentSessionResult{Session: current}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CreateRunAgentSession creates the parentless run session of OwnerRunID at
// its first claim, fenced by that Run's lease. A Run bound to another session
// gets ErrSessionMoved.
func (s *Store) CreateRunAgentSession(ctx context.Context, req models.CreateRunAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
	txReq := models.CreateRunAgentSessionTxRequest{
		Scope:          req.Scope,
		OwnerRunID:     req.OwnerRunID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ThreadRootID:   req.ThreadRootID,
		InitialEntries: req.InitialEntries,
		Now:            postgresSessionNow(req.Now),
	}
	if strings.TrimSpace(req.Lease.WorkerID) == "" {
		return nil, fmt.Errorf("%w: a lease is required", models.ErrInvalidAgentSession)
	}
	if err := models.ValidateCreateRunAgentSessionTxRequest(txReq); err != nil {
		return nil, err
	}
	var result *models.OpenAgentSessionResult
	err := s.beginPostgresSessionTx(ctx, func(tx *sql.Tx) error {
		lease := req.Lease
		run, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.OwnerRunID, &lease)
		if err != nil {
			return err
		}
		if id := models.RunAgentSessionID(req.Scope, req.OwnerRunID); run.SessionID != "" && run.SessionID != id {
			return models.ErrSessionMoved
		}
		result, err = s.createRunAgentSessionCoreTx(ctx, tx, run, txReq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// createRunAgentSessionTx is the in-transaction create of a run session
// (child Run creation, dependency groups, AgentRequests, ConversationTasks,
// promotion). The caller owns the fence. The owner Run must already be
// stored; it is locked here (Run before session) and its SessionID is set to
// the new session in the same transaction (Revision+1 when it changes). A
// replay with the same parent and Agent returns the existing session with
// Created=false and writes nothing.
func (s *Store) createRunAgentSessionTx(ctx context.Context, tx *sql.Tx, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateCreateRunAgentSessionTxRequest(req); err != nil {
		return nil, err
	}
	run, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.OwnerRunID, nil)
	if err != nil {
		return nil, err
	}
	return s.createRunAgentSessionCoreTx(ctx, tx, run, req)
}

func (s *Store) createRunAgentSessionCoreTx(ctx context.Context, tx *sql.Tx, run *models.AgentRun, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	id := models.RunAgentSessionID(req.Scope, req.OwnerRunID)
	matches := func(current *models.AgentSession) bool {
		return current.Kind == models.AgentSessionKindRun && current.OwnerRunID == req.OwnerRunID &&
			current.ParentSessionID == req.ParentSessionID && current.AgentID == req.AgentID
	}
	current, err := s.getPostgresAgentSessionTx(ctx, tx, req.Scope, id, true)
	switch {
	case err == nil:
		if !matches(current) {
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
		parent, err := s.getPostgresAgentSessionTx(ctx, tx, req.Scope, req.ParentSessionID, true)
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
			if err := tx.QueryRowContext(ctx, `SELECT id FROM `+s.table("agent_session_entries")+`
				WHERE scope_kind = $1 AND scope_id = $2 AND session_id = $3 AND sequence = $4`,
				req.Scope.Kind, req.Scope.ID, parent.ID, req.ForkSequence).Scan(&session.ForkEntryID); err != nil {
				return nil, err
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
		session.ModelProvider, session.Model, err = s.postgresForkModelTx(ctx, tx, parent, req.ForkSequence)
		if err != nil {
			return nil, err
		}
	}
	created, err := s.insertPostgresAgentSessionTx(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	if !created {
		// Same id inserted concurrently (owner index or primary key).
		current, err := s.getPostgresAgentSessionTx(ctx, tx, req.Scope, id, true)
		if err != nil || !matches(current) {
			return nil, models.ErrAgentSessionConflict
		}
		return &models.OpenAgentSessionResult{Session: current}, nil
	}
	stored := session
	if len(req.InitialEntries) > 0 {
		appended, err := s.appendAgentSessionEntriesCoreTx(ctx, tx, models.InitialEntriesAppendRequest(req, id))
		if err != nil {
			return nil, err
		}
		stored = appended.Session
	}
	if run.SessionID != id {
		next := models.CloneAgentRun(run)
		next.SessionID = id
		next.Revision = run.Revision + 1
		next.UpdatedAt = req.Now
		if err := s.updatePostgresAgentRunTx(ctx, tx, next, run.Revision); err != nil {
			return nil, err
		}
	}
	return &models.OpenAgentSessionResult{Session: stored, Created: true}, nil
}

// postgresForkModelTx is the model of the latest model_change entry on the
// parent's chain up to the fork point, or the parent's own model.
func (s *Store) postgresForkModelTx(ctx context.Context, tx *sql.Tx, parent *models.AgentSession, forkSequence int64) (string, string, error) {
	session, bound := parent, forkSequence
	for depth := 0; depth <= models.MaxAgentSessionDepth; depth++ {
		entries, err := s.queryPostgresSessionEntries(ctx, tx, `SELECT payload FROM `+s.table("agent_session_entries")+`
			WHERE scope_kind = $1 AND scope_id = $2 AND session_id = $3 AND type = 'model_change' AND sequence <= $4
			ORDER BY sequence DESC LIMIT 1`, session.Scope.Kind, session.Scope.ID, session.ID, bound)
		if err != nil {
			return "", "", err
		}
		if len(entries) == 1 && entries[0].ModelChange != nil {
			return entries[0].ModelChange.Provider, entries[0].ModelChange.Model, nil
		}
		if session.ParentSessionID == "" {
			break
		}
		bound = session.ForkSequence
		session, err = s.getPostgresAgentSessionTx(ctx, tx, session.Scope, session.ParentSessionID, false)
		if err != nil {
			return "", "", err
		}
	}
	return parent.ModelProvider, parent.Model, nil
}
