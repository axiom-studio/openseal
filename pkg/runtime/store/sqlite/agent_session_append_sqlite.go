package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// AppendAgentSessionEntries appends one batch, fenced by the Run lease (or,
// with Lease nil, by the Run being terminal) and by run.SessionID ==
// req.SessionID. The watermark CAS, the intervention cursor, the entries and
// the events commit together.
func (s *Store) AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	var result *models.AppendAgentSessionEntriesResult
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
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
func (s *Store) appendAgentSessionEntriesTx(ctx context.Context, conn *sql.Conn, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
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

// TransitionToolEntry moves one tool entry (CAS on state and revision), fenced
// by the lease of the entry's Run, or, with an empty lease, by that Run being
// terminal (loop-16 terminal close). The session binding is not checked, so a
// promoted Run can still settle entries it left in the parent session.
func (s *Store) TransitionToolEntry(ctx context.Context, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = sessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	var result *models.ToolEntryTransitionResult
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
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
func (s *Store) transitionToolEntryTx(ctx context.Context, conn *sql.Conn, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
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
