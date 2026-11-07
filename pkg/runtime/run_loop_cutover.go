package runtime

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RunLoopCutoverCode names the cutover: the FailureExplanation.Code of every
// Run it cancels (SPEC §2.8) and the actor of its adopt and cancel events.
const RunLoopCutoverCode = "run_loop_cutover"

// RunLoopCutoverMessage answers each unanswered chat request the cutover
// stopped. It is also the canceled Run's FailureExplanation.Message.
const RunLoopCutoverMessage = "This request was stopped by a platform update. Send it again to continue."

// runLoopCutoverRetries bounds the re-reads after a revision race on one Run
// or one conversation.
const runLoopCutoverRetries = 5

// RunLoopCutoverRef names one Run the cutover has to look at.
type RunLoopCutoverRef struct {
	Scope Scope
	ID    string
}

// RunLoopCutoverStore is the store the cutover needs. The memory, SQLite and
// PostgreSQL stores implement it; a wrapper that embeds one of them does too.
type RunLoopCutoverStore interface {
	RunCommandStore
	AgentTurnStore
	// ListRunLoopCutoverCandidates lists, across every scope, the
	// non-terminal Runs without an Engine marker, oldest first.
	ListRunLoopCutoverCandidates(ctx context.Context) ([]RunLoopCutoverRef, error)
	// WithRunLoopCutoverLock runs fn under the lock that serializes the
	// cutover of concurrently starting processes.
	WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error
}

// RunLoopCutoverConversations posts the visible cancel message. It is
// satisfied by *ConversationService.
type RunLoopCutoverConversations interface {
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
	GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error)
	ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error)
	PostChannelMessage(ctx context.Context, req PostChannelMessageRequest) (*ChannelMessageCommitResult, error)
}

// RunLoopCutoverReport counts what one cutover pass did.
type RunLoopCutoverReport struct {
	Adopted  int
	Canceled int
	Messages int
	Skipped  int
}

// RunLoopCutover moves the store onto the run loop (A16, SPEC §4.8). It must
// run before any worker starts. Every non-terminal Run without an Engine
// marker is either adopted (queued, never claimed, no step row: the loop can
// start it fresh) or canceled through CommandAgentRun, which closes its
// running step and open tool entries. A canceled Run carries
// FailureExplanation{Code: run_loop_cutover}. Each unanswered chat request of
// a canceled Run gets one visible system message, keyed like the canceled-run
// reconciler's so neither doubles the other. A lane holds at most one
// unanswered request per Run, so this is one message per request, not per
// lane: two old Runs of one lane with different open requests each get one.
//
// It is idempotent: after a full pass no Run qualifies. It fails closed: any
// error is returned and the caller must not start workers.
func RunLoopCutover(ctx context.Context, store RunLoopCutoverStore, conversations RunLoopCutoverConversations) (RunLoopCutoverReport, error) {
	var report RunLoopCutoverReport
	if store == nil {
		return report, errors.New("run loop cutover: store is required")
	}
	err := store.WithRunLoopCutoverLock(ctx, func(ctx context.Context) error {
		refs, err := store.ListRunLoopCutoverCandidates(ctx)
		if err != nil {
			return fmt.Errorf("list cutover candidates: %w", err)
		}
		c := &runLoopCutover{store: store, conversations: conversations, commands: NewRunCommandService(store), posted: map[string]bool{}}
		for _, ref := range refs {
			if err := c.cutRun(ctx, ref, &report); err != nil {
				return fmt.Errorf("cut over Run %s: %w", ref.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("run loop cutover: %w", err)
	}
	return report, nil
}

type runLoopCutover struct {
	store         RunLoopCutoverStore
	conversations RunLoopCutoverConversations
	commands      *RunCommandService
	// posted holds the requests answered in this pass, so a lane whose
	// request also reached a child Run gets one message.
	posted map[string]bool
}

var runLoopCutoverActor = ActivityActor{Type: "runtime", ID: RunLoopCutoverCode}

func (c *runLoopCutover) cutRun(ctx context.Context, ref RunLoopCutoverRef, report *RunLoopCutoverReport) error {
	for attempt := 0; ; attempt++ {
		run, err := c.store.GetAgentRun(ctx, ref.Scope, ref.ID)
		if err != nil {
			return err
		}
		if run == nil || isTerminalAgentRunStatus(run.Status) || run.Engine != "" {
			report.Skipped++
			return nil
		}
		adopt, err := c.adoptable(ctx, run)
		if err != nil {
			return err
		}
		if adopt {
			err = c.adopt(ctx, run)
		} else {
			err = c.cancel(ctx, run, report)
		}
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
			continue
		}
		if err != nil {
			return err
		}
		if adopt {
			report.Adopted++
		} else {
			report.Canceled++
		}
		return nil
	}
}

// adoptable reports whether the loop can start run fresh: it is queued, never
// applied a Turn, has no agent_turns row and has the session binding the loop
// needs, or one adopt can derive. A conversation task Run without a session
// cannot be bound here (its child session forks the chat at promotion), so
// it is canceled.
func (c *runLoopCutover) adoptable(ctx context.Context, run *AgentRun) (bool, error) {
	if run.Status != AgentRunStatusQueued || run.LastAppliedTurn != 0 {
		return false, nil
	}
	if needsLaneSession(run) {
		if task, _ := run.Context[ConversationTaskContextKey].(string); task != "" {
			return false, nil
		}
	}
	turns, err := c.store.ListAgentTurns(ctx, AgentTurnFilter{Scope: run.Scope, RunID: run.ID, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(turns) == 0, nil
}

func (c *runLoopCutover) adopt(ctx context.Context, current *AgentRun) error {
	now := time.Now().UTC()
	run := cloneAgentRun(current)
	run.Engine = RunEngineLoop
	if needsLaneSession(run) {
		// Old chat Runs predate the lane binding; the loop opens the lane
		// session only for a Run bound to it, as the scheduler binds new ones.
		run.SessionID = conversationSessionID(run.Scope, run.ConcurrencyKey)
	}
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.engine_adopted", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run moved to the updated agent runtime", Visibility: ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"engine": RunEngineLoop, "code": RunLoopCutoverCode, "sessionId": run.SessionID},
	}
	_, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil)
	return err
}

// needsLaneSession reports an acting-Agent conversation Run with no session:
// one created before chat Runs were bound to their lane session. Team routing
// Runs (no assigned Agent) have no session.
func needsLaneSession(run *AgentRun) bool {
	return run.Kind == RunKindConversation && run.SessionID == "" && run.AssignedAgentID != ""
}

func runLoopCutoverExplanation() *RunFailureExplanation {
	return &RunFailureExplanation{Kind: "execution", Code: RunLoopCutoverCode, Message: RunLoopCutoverMessage}
}

// cancel posts the lane's message first and then cancels the Run, so a pass
// interrupted between the two still leaves the message: the next pass finds
// the request answered and only cancels.
func (c *runLoopCutover) cancel(ctx context.Context, run *AgentRun, report *RunLoopCutoverReport) error {
	posted, err := c.postMessage(ctx, run)
	if err != nil {
		return fmt.Errorf("post cutover message: %w", err)
	}
	if posted {
		report.Messages++
	}
	if run.FailureExplanation == nil || run.FailureExplanation.Code != RunLoopCutoverCode {
		// Record the cause first; the cancel keeps it. A pass interrupted
		// here cancels the Run on the next pass.
		marked, err := c.markCanceledByCutover(ctx, run)
		if err != nil {
			return err
		}
		run = marked
	}
	if _, err := c.commands.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: run.Scope, RunID: run.ID, ExpectedRevision: run.Revision, Kind: AgentRunCommandCancel,
		Actor: runLoopCutoverActor, Summary: "Stopped by a platform update", Visibility: ActivityVisibilityScope,
	}); err != nil {
		return err
	}
	return nil
}

func (c *runLoopCutover) markCanceledByCutover(ctx context.Context, current *AgentRun) (*AgentRun, error) {
	now := time.Now().UTC()
	run := cloneAgentRun(current)
	run.FailureExplanation = runLoopCutoverExplanation()
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.cutover_stopping", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run is being stopped by a platform update", Visibility: ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"code": RunLoopCutoverCode},
	}
	if _, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil); err != nil {
		return nil, err
	}
	return run, nil
}

// postMessage answers a chat request the cutover stops, once per request.
// It reports whether a new message was written.
func (c *runLoopCutover) postMessage(ctx context.Context, run *AgentRun) (bool, error) {
	if c.conversations == nil || run.Kind != RunKindConversation {
		return false, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(triggerID, 128) {
		return false, nil
	}
	requestKey := run.Scope.Kind + "\x00" + run.Scope.ID + "\x00" + conversationID + "\x00" + triggerID
	if c.posted[requestKey] {
		return false, nil
	}
	trigger, err := c.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if errors.Is(err, ErrChannelMessageNotFound) || errors.Is(err, ErrConversationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !trigger.RequiresResponse {
		return false, nil
	}
	resolved, err := c.requestResolved(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return false, err
	}
	if resolved {
		c.posted[requestKey] = true
		return false, nil
	}
	for attempt := 0; ; attempt++ {
		conversation, err := c.conversations.GetConversation(ctx, run.Scope, conversationID)
		if errors.Is(err, ErrConversationNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner {
			return false, nil
		}
		result, err := c.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "openseal.conversation"},
			Intent: MessageIntentSystem, Content: RunLoopCutoverMessage, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
			References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: "conversation-run-canceled-result:" + run.ID,
		})
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
			continue
		}
		if err != nil {
			return false, err
		}
		c.posted[requestKey] = true
		return !result.Replayed, nil
	}
}

func (c *runLoopCutover) requestResolved(ctx context.Context, scope Scope, conversationID, triggerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := c.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
			Scope: scope, ConversationID: conversationID, AfterSequence: after, Limit: pageSize,
		})
		if err != nil {
			return false, err
		}
		for _, message := range messages {
			if message.ResolvesMessageID == triggerID {
				return true, nil
			}
		}
		if len(messages) < pageSize {
			return false, nil
		}
		after = messages[len(messages)-1].Sequence
	}
}

// runLoopCutoverLocalMu serializes the cutover inside one process for the
// memory and SQLite stores. A SQLite store is opened by one process (the
// desktop daemon); its write lock cannot be held across the cutover's own
// writes, which use other connections.
var runLoopCutoverLocalMu sync.Mutex

func withRunLoopCutoverLocalLock(ctx context.Context, fn func(context.Context) error) error {
	runLoopCutoverLocalMu.Lock()
	defer runLoopCutoverLocalMu.Unlock()
	return fn(ctx)
}

func (s *MemoryStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	return withRunLoopCutoverLocalLock(ctx, fn)
}

func (s *MemoryStore) ListRunLoopCutoverCandidates(_ context.Context) ([]RunLoopCutoverRef, error) {
	s.mu.RLock()
	runs := make([]*AgentRun, 0)
	for _, run := range s.agentRuns {
		if run != nil && run.Engine == "" && !isTerminalAgentRunStatus(run.Status) {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
	refs := make([]RunLoopCutoverRef, 0, len(runs))
	for _, run := range runs {
		refs = append(refs, RunLoopCutoverRef{Scope: run.Scope, ID: run.ID})
	}
	s.mu.RUnlock()
	return refs, nil
}

func (s *SQLiteStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	return withRunLoopCutoverLocalLock(ctx, fn)
}

func (s *SQLiteStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]RunLoopCutoverRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id FROM agent_runs
		WHERE status NOT IN ('completed', 'failed', 'canceled') AND COALESCE(json_extract(payload, '$.engine'), '') = ''
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanRunLoopCutoverRefs(rows)
}

// WithRunLoopCutoverLock holds the schema migration advisory lock for the
// whole pass, so concurrently starting replicas serialize (with each other
// and with a migration) and the later one finds nothing to do. It is a
// session-level lock on a dedicated connection that is not in a transaction,
// so idle_in_transaction_session_timeout cannot end it; the cutover's writes
// use other connections. The pass fails when the lock is no longer held at
// its end.
func (s *PostgresStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	key := "openseal:migrate:" + s.schema
	if err := s.acquireRunLoopCutoverLock(ctx, conn, key); err != nil {
		return err
	}
	runErr := fn(ctx)
	// Unlock on a context that outlives a canceled pass so the session does
	// not keep the lock; false means the lock was lost during the pass.
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var released bool
	unlockErr := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(hashtext($1))`, key).Scan(&released)
	if unlockErr == nil && !released {
		unlockErr = errors.New("cutover lock was lost during the pass")
	}
	if unlockErr != nil {
		// Drop the connection rather than return it to the pool holding a
		// lock or in an unknown state.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return errors.Join(runErr, fmt.Errorf("release cutover lock: %w", unlockErr))
	}
	return runErr
}

func (s *PostgresStore) acquireRunLoopCutoverLock(ctx context.Context, conn *sql.Conn, key string) error {
	startedAt := time.Now()
	lockContext, cancel := context.WithTimeout(ctx, s.migrationLockTimeout)
	defer cancel()
	for {
		var acquired bool
		err := conn.QueryRowContext(lockContext, `SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&acquired)
		if err == nil && acquired {
			return nil
		}
		if err == nil {
			timer := time.NewTimer(s.migrationLockPollInterval)
			select {
			case <-lockContext.Done():
				timer.Stop()
				err = lockContext.Err()
			case <-timer.C:
				continue
			}
		}
		if errors.Is(lockContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("acquire cutover lock: %w after %s", ErrPostgresMigrationLockTimeout, time.Since(startedAt).Round(time.Millisecond))
		}
		return fmt.Errorf("acquire cutover lock: %w", err)
	}
}

func (s *PostgresStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]RunLoopCutoverRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id FROM `+s.table("agent_runs")+`
		WHERE status NOT IN ('completed', 'failed', 'canceled') AND COALESCE(payload->>'engine', '') = ''
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanRunLoopCutoverRefs(rows)
}

func scanRunLoopCutoverRefs(rows *sql.Rows) ([]RunLoopCutoverRef, error) {
	defer rows.Close()
	refs := make([]RunLoopCutoverRef, 0)
	for rows.Next() {
		var ref RunLoopCutoverRef
		if err := rows.Scan(&ref.Scope.Kind, &ref.Scope.ID, &ref.ID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
