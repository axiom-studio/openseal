package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/lib/pq"
)

// AppendAgentSessionEntries appends one batch, fenced by the Run lease (or,
// with Lease nil, by the Run being terminal) and by run.SessionID ==
// req.SessionID. The watermark CAS, the intervention cursor, the entries and
// the events commit together.
func (s *Store) AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
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
func (s *Store) appendAgentSessionEntriesTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateAppendAgentSessionEntries(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidSessionEntry); err != nil {
		return nil, err
	}
	return s.appendAgentSessionEntriesCoreTx(ctx, tx, req)
}

func (s *Store) appendAgentSessionEntriesCoreTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
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
func (s *Store) postgresAppendReplayTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest, session *models.AgentSession) (*models.AppendAgentSessionEntriesResult, bool, error) {
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

func (s *Store) postgresExistingSourceKeysTx(ctx context.Context, tx *sql.Tx, req models.AppendAgentSessionEntriesRequest) (map[string]bool, error) {
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

// TransitionToolEntry moves one tool entry (CAS on state and revision), fenced
// by the lease of the entry's Run, or, with a zero Lease, by that Run being
// terminal. The session row is neither locked nor bumped.
func (s *Store) TransitionToolEntry(ctx context.Context, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
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
func (s *Store) transitionToolEntryTx(ctx context.Context, tx *sql.Tx, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	req.Now = postgresSessionNow(req.Now)
	if err := models.ValidateToolEntryTransitionRequest(req); err != nil {
		return nil, err
	}
	if err := models.ValidateSessionEvents(req.Scope, req.RunID, req.Events, models.ErrInvalidToolEntryTransition); err != nil {
		return nil, err
	}
	return s.transitionToolEntryCoreTx(ctx, tx, req)
}

func (s *Store) transitionToolEntryCoreTx(ctx context.Context, tx *sql.Tx, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
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
