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
	"github.com/lib/pq"
)

// Postgres backend of AgentSessionStore (run-loop SPEC §2.1, §4.1, §4.7 and
// section 01 §4). Lock order inside every write transaction is Run row ->
// session row -> entry row. Path loads run in one REPEATABLE READ, read-only
// transaction. The …Tx helpers are the in-transaction variants for stores
// that already hold the Run row lock (child Run create, promotion).

var _ models.AgentSessionStore = (*PostgresStore)(nil)

func postgresSessionNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// lockPostgresSessionRunTx locks the Run row (FOR UPDATE) and checks the
// lease as UpdateAgentRunWithEvent does.
func (s *PostgresStore) lockPostgresSessionRunTx(ctx context.Context, tx *sql.Tx, scope models.Scope, runID string, lease *models.AgentRunLeaseGuard) (*models.AgentRun, error) {
	run, err := s.getPostgresAgentRunTx(ctx, tx, scope, runID, true)
	if err != nil {
		return nil, err
	}
	if lease != nil && (run.LeaseOwner != lease.WorkerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(lease.Now)) {
		return nil, models.ErrLeaseLost
	}
	return run, nil
}

func (s *PostgresStore) getPostgresAgentSessionTx(ctx context.Context, queryer postgresDependencyQueryer, scope models.Scope, sessionID string, lock bool) (*models.AgentSession, error) {
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
func (s *PostgresStore) insertPostgresAgentSessionTx(ctx context.Context, tx *sql.Tx, session *models.AgentSession) (bool, error) {
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

func (s *PostgresStore) updatePostgresAgentSessionTx(ctx context.Context, tx *sql.Tx, session *models.AgentSession, expectedRevision int64) error {
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

func (s *PostgresStore) queryPostgresSessionEntries(ctx context.Context, queryer postgresDependencyQueryer, query string, args ...interface{}) ([]*models.SessionEntry, error) {
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return decodePostgresSessionEntries(rows)
}

func (s *PostgresStore) getPostgresSessionEntryTx(ctx context.Context, queryer postgresDependencyQueryer, scope models.Scope, entryID string, lock bool) (*models.SessionEntry, error) {
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
func (s *PostgresStore) insertPostgresSessionEntriesTx(ctx context.Context, tx *sql.Tx, entries []*models.SessionEntry) error {
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

func (s *PostgresStore) insertPostgresSessionEventsTx(ctx context.Context, tx *sql.Tx, events []*models.ActivityEvent, now time.Time) ([]*models.ActivityEvent, error) {
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

func (s *PostgresStore) beginPostgresSessionTx(ctx context.Context, run func(*sql.Tx) error) error {
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

func (s *PostgresStore) OpenConversationAgentSession(ctx context.Context, req models.OpenConversationAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
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
func (s *PostgresStore) CreateRunAgentSession(ctx context.Context, req models.CreateRunAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
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
func (s *PostgresStore) createRunAgentSessionTx(ctx context.Context, tx *sql.Tx, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
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

func (s *PostgresStore) createRunAgentSessionCoreTx(ctx context.Context, tx *sql.Tx, run *models.AgentRun, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
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
func (s *PostgresStore) postgresForkModelTx(ctx context.Context, tx *sql.Tx, parent *models.AgentSession, forkSequence int64) (string, string, error) {
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

// ---------------------------------------------------------------------------
// Reads

func (s *PostgresStore) GetAgentSession(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidAgentSession, err)
	}
	return s.getPostgresAgentSessionTx(ctx, s.db, scope, sessionID, false)
}

func (s *PostgresStore) GetSessionEntry(ctx context.Context, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidSessionEntry, err)
	}
	return s.getPostgresSessionEntryTx(ctx, s.db, scope, entryID, false)
}

// LoadAgentSessionPath builds the path in one REPEATABLE READ read-only
// transaction, so a concurrent append or compaction is either wholly in the
// snapshot or wholly absent.
func (s *PostgresStore) LoadAgentSessionPath(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSessionPath, error) {
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
func (s *PostgresStore) postgresSessionPathTx(ctx context.Context, tx *sql.Tx, leaf *models.AgentSession, extra []*models.SessionEntry) (*models.AgentSessionPath, error) {
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

func (s *PostgresStore) ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
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

// ---------------------------------------------------------------------------
// Append

// AppendAgentSessionEntries appends one batch, fenced by the Run lease (or,
// with Lease nil, by the Run being terminal) and by run.SessionID ==
// req.SessionID. The watermark CAS, the intervention cursor, the entries and
// the events commit together.
func (s *PostgresStore) AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	var result *models.AppendAgentSessionEntriesResult
	err := s.beginPostgresSessionTx(ctx, func(tx *sql.Tx) error {
		run, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.RunID, req.Lease)
		if err != nil {
			return err
		}
		if req.Lease == nil && !models.IsTerminalAgentRunStatus(run.Status) {
			return models.ErrLeaseLost
		}
		if run.SessionID != req.SessionID {
			return models.ErrSessionMoved
		}
		result, err = s.appendAgentSessionEntriesCoreTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// appendAgentSessionEntriesTx is the in-transaction append. The caller owns
// the fence (lease and session binding are not checked); everything else,
// including the watermark CAS and the cursor, is as AppendAgentSessionEntries.
func (s *PostgresStore) appendAgentSessionEntriesTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	return s.appendAgentSessionEntriesCoreTx(ctx, tx, req)
}

func (s *PostgresStore) appendAgentSessionEntriesCoreTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	var run *models.AgentRun
	if req.ConsumeInterventionsThrough != nil {
		// Run before session (lock order); the caller usually holds it already.
		var err error
		if run, err = s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.RunID, nil); err != nil {
			return nil, err
		}
	}
	session, err := s.getPostgresAgentSessionTx(ctx, tx, req.Scope, req.SessionID, true)
	if err != nil {
		return nil, err
	}
	if replayed, ok, err := s.postgresAppendReplayTx(ctx, tx, req, session); err != nil || ok {
		return replayed, err
	}
	if run != nil {
		through := *req.ConsumeInterventionsThrough
		if through < run.ConsumedInterventions || through > len(run.PendingInterventions) {
			return nil, models.InvalidSessionEntryf("intervention cursor %d is outside %d..%d", through, run.ConsumedInterventions, len(run.PendingInterventions))
		}
	}
	if req.ExpectedChannelWatermark != nil && session.ChannelWatermark != *req.ExpectedChannelWatermark {
		return nil, models.ErrChannelWatermarkMoved
	}
	existingKeys, err := s.postgresExistingSourceKeysTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}

	next := models.CloneAgentSession(session)
	planned := make([]*models.SessionEntry, 0, len(req.Entries))
	skipped := []string{}
	skippedSteps := make(map[string]bool)
	for _, entry := range req.Entries {
		if entry.SourceKey != "" && existingKeys[entry.SourceKey] {
			skipped = append(skipped, entry.ID)
			if entry.Type == models.SessionEntryAssistant {
				skippedSteps[entry.StepID] = true
			}
			continue
		}
		if entry.Type == models.SessionEntryTool && len(entry.Tool.Directive) == 0 && skippedSteps[entry.StepID] {
			skipped = append(skipped, entry.ID)
			continue
		}
		stamped := models.StampAppendedSessionEntry(entry, req, next.NextSequence+int64(len(planned)))
		if err := stamped.Validate(); err != nil {
			return nil, err
		}
		if stamped.Type == models.SessionEntryCompaction {
			leaf := models.CloneAgentSession(session)
			leaf.NextSequence = stamped.Sequence
			path, err := s.postgresSessionPathTx(ctx, tx, leaf, planned)
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
		planned = append(planned, stamped)
	}
	sessionChanged := false
	if len(planned) > 0 {
		next.NextSequence += int64(len(planned))
		sessionChanged = true
	}
	if req.ExpectedChannelWatermark != nil && req.ChannelWatermark != next.ChannelWatermark {
		next.ChannelWatermark = req.ChannelWatermark
		sessionChanged = true
	}

	if err := s.insertPostgresSessionEntriesTx(ctx, tx, planned); err != nil {
		return nil, err
	}
	if sessionChanged {
		next.Revision = session.Revision + 1
		next.UpdatedAt = req.Now
		if err := s.updatePostgresAgentSessionTx(ctx, tx, next, session.Revision); err != nil {
			return nil, err
		}
	}
	if run != nil && *req.ConsumeInterventionsThrough != run.ConsumedInterventions {
		updated := models.CloneAgentRun(run)
		updated.ConsumedInterventions = *req.ConsumeInterventionsThrough
		updated.Revision = run.Revision + 1
		updated.UpdatedAt = req.Now
		if err := s.updatePostgresAgentRunTx(ctx, tx, updated, run.Revision); err != nil {
			return nil, err
		}
	}
	events, err := s.insertPostgresSessionEventsTx(ctx, tx, req.Events, req.Now)
	if err != nil {
		return nil, err
	}
	return &models.AppendAgentSessionEntriesResult{Session: next, Entries: planned, Skipped: skipped, Events: events}, nil
}

// postgresAppendReplayTx reports a whole-batch replay: every request entry is
// stored in this session already, or would be skipped by its SourceKey. An
// entry id stored in another session, or a partial batch, is a conflict.
func (s *PostgresStore) postgresAppendReplayTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest, session *models.AgentSession) (*models.AppendAgentSessionEntriesResult, bool, error) {
	if len(req.Entries) == 0 {
		return nil, false, nil
	}
	ids := make([]string, len(req.Entries))
	for i, entry := range req.Entries {
		ids[i] = entry.ID
	}
	stored, err := s.queryPostgresSessionEntries(ctx, tx, `SELECT payload FROM `+s.table("agent_session_entries")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = ANY($3)`, req.Scope.Kind, req.Scope.ID, pq.Array(ids))
	if err != nil {
		return nil, false, err
	}
	if len(stored) == 0 {
		return nil, false, nil
	}
	byID := make(map[string]*models.SessionEntry, len(stored))
	for _, entry := range stored {
		if entry.SessionID != session.ID {
			return nil, false, models.ErrAgentSessionConflict
		}
		byID[entry.ID] = entry
	}
	keys, err := s.postgresExistingSourceKeysTx(ctx, tx, req)
	if err != nil {
		return nil, false, err
	}
	result := &models.AppendAgentSessionEntriesResult{Session: session, Replayed: true, Entries: []*models.SessionEntry{}}
	for _, entry := range req.Entries {
		if found := byID[entry.ID]; found != nil {
			result.Entries = append(result.Entries, found)
			continue
		}
		if entry.SourceKey != "" && keys[entry.SourceKey] {
			result.Skipped = append(result.Skipped, entry.ID)
			continue
		}
		return nil, false, models.ErrAgentSessionConflict
	}
	return result, true, nil
}

func (s *PostgresStore) postgresExistingSourceKeysTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (map[string]bool, error) {
	var keys []string
	for _, entry := range req.Entries {
		if entry.SourceKey != "" {
			keys = append(keys, entry.SourceKey)
		}
	}
	existing := make(map[string]bool)
	if len(keys) == 0 {
		return existing, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_key FROM `+s.table("agent_session_entries")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND session_id = $3 AND source_key = ANY($4)`,
		req.Scope.Kind, req.Scope.ID, req.SessionID, pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		existing[key] = true
	}
	return existing, rows.Err()
}

// ---------------------------------------------------------------------------
// Tool entry transitions

// TransitionToolEntry moves one tool entry (CAS on state and revision), fenced
// by the lease of the entry's Run, or, with a zero Lease, by that Run being
// terminal. The session row is neither locked nor bumped.
func (s *PostgresStore) TransitionToolEntry(ctx context.Context, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	var result *models.ToolEntryTransitionResult
	err := s.beginPostgresSessionTx(ctx, func(tx *sql.Tx) error {
		if req.Lease.WorkerID == "" {
			run, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.RunID, nil)
			if err != nil {
				return err
			}
			if !models.IsTerminalAgentRunStatus(run.Status) {
				return models.ErrLeaseLost
			}
		} else {
			lease := req.Lease
			if _, err := s.lockPostgresSessionRunTx(ctx, tx, req.Scope, req.RunID, &lease); err != nil {
				return err
			}
		}
		var err error
		result, err = s.transitionToolEntryCoreTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// transitionToolEntryTx is the in-transaction transition. The caller owns the
// lease check and has locked the Run row.
func (s *PostgresStore) transitionToolEntryTx(ctx context.Context, tx *sql.Tx, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	return s.transitionToolEntryCoreTx(ctx, tx, req)
}

func (s *PostgresStore) transitionToolEntryCoreTx(ctx context.Context, tx *sql.Tx, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	current, err := s.getPostgresSessionEntryTx(ctx, tx, req.Scope, req.EntryID, true)
	if err != nil {
		return nil, err
	}
	next, err := models.ApplyToolEntryTransition(current, req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_session_entries")+` SET state = $1, action_call_id = $2,
		tokens = $3, revision = $4, updated_at = $5, payload = $6::jsonb
		WHERE scope_kind = $7 AND scope_id = $8 AND id = $9 AND revision = $10`,
		next.State, next.ActionCallID, next.Tokens, next.Revision, next.UpdatedAt, string(payload),
		req.Scope.Kind, req.Scope.ID, req.EntryID, current.Revision)
	if err != nil {
		return nil, err
	}
	affected, err := updated.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, models.ErrToolEntryStateConflict
	}
	events, err := s.insertPostgresSessionEventsTx(ctx, tx, req.Events, req.Now)
	if err != nil {
		return nil, err
	}
	return &models.ToolEntryTransitionResult{Entry: next, Events: events}, nil
}
