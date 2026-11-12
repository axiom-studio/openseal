package runtime

// Copies of runtime's base test fixtures for the run loop tests, so they can
// move with the run loop code: the session store helpers of
// agent_session_store_test.go and kernel_store_test.go, the Postgres opener
// of postgres_test_schema_test.go, the person action helpers of
// connect_account_test.go, the governed tool prefix of
// loop_testkit_fixture_test.go, the routine script digest of
// routine_script_test.go and the store matrix, clock and crash store of
// loop_testkit_model_test.go. Change them together.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

const loopModelName = "fake-model"

// loopGovernedToolPrefix starts every governed tool name the model is offered.
const loopGovernedToolPrefix = "action_"

func loopScriptDigest(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)[:64]
}

var loopSessionScope = Scope{Kind: "org", ID: "o1"}

var loopSessionNow = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func loopSessionLease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: "w1", Now: loopSessionNow}
}

func putLoopSessionRun(t *testing.T, store *sqlite.Store, runID, sessionID string, status AgentRunStatus, interventions int) *AgentRun {
	t.Helper()
	expires := loopSessionNow.Add(time.Minute)
	run := &AgentRun{
		ID: runID, Scope: loopSessionScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, Goal: "session test",
		Status: status, Revision: 1, SessionID: sessionID,
		LeaseOwner: "w1", LeaseExpiresAt: &expires, CreatedAt: loopSessionNow, UpdatedAt: loopSessionNow,
	}
	for i := 0; i < interventions; i++ {
		run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{ID: "iv" + string(rune('a'+i)), Instruction: "steer"})
	}
	return putLoopRun(t, store, run)
}

// putLoopRun writes run as given, without the scheduler's rules, so a test
// can rebind or relabel a Run mid-scenario. A new Run is created; an existing
// one is rewritten through the store's update outside any lease, which moves
// its revision on by one (returned in the written Run).
func putLoopRun(t testing.TB, store *sqlite.Store, run *AgentRun) *AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatalf("create run %s: %v", run.ID, err)
		}
		return run
	}
	next := cloneAgentRun(run)
	next.Revision = current.Revision + 1
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.status_changed", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, RunID: run.ID, Actor: ActivityActor{Type: "system", ID: "test"},
		Summary: "Test rewrote the Run", Visibility: ActivityVisibilityScope, CreatedAt: run.UpdatedAt,
	}
	if _, err := store.UpdateAgentRunWithEvent(ctx, next, current.Revision, event, nil); err != nil {
		t.Fatalf("rewrite run %s: %v", run.ID, err)
	}
	return next
}

func loopStoredSessionRun(t *testing.T, store *sqlite.Store, runID string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), loopSessionScope, runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func openLoopConversationSession(t *testing.T, store *sqlite.Store, runID string) *AgentSession {
	t.Helper()
	sessionID := ConversationAgentSessionID(loopSessionScope, ConversationRunLane("c1", "", "agent-a"))
	putLoopSessionRun(t, store, runID, sessionID, AgentRunStatusRunning, 0)
	opened, err := store.OpenConversationAgentSession(context.Background(), OpenConversationAgentSessionRequest{
		Scope: loopSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: runID, Lease: loopSessionLease(), Now: loopSessionNow,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return opened.Session
}

// getLoopRun reads a Run that the test knows exists.
func getLoopRun(t testing.TB, store *sqlite.Store, scope Scope, id string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// proposeLoopPersonAction proposes send as the governed tool call of run loop
// step stepID, as the loop does for the Run's lease owner.
func proposeLoopPersonAction(t *testing.T, store *sqlite.Store, catalog *skill.Catalog, scope Scope, run *AgentRun, now time.Time, stepID, key string, policy action.ActionPolicyEvaluator) *ActionProposalResult {
	t.Helper()
	if policy == nil {
		policy = action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
			return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
		})
	}
	coordinator := action.NewActionCoordinator(store, store, catalog, policy, nil, models.WithClock(func() time.Time { return now }))
	proposal, err := coordinator.Propose(context.Background(), action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, TurnID: stepID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		Origin: ActionOriginLoop, Execution: ActionExecutionLoop, Summary: "Send a message to ana@example.com",
		SkillID: "mail", SkillVersion: "1.0.0", Action: "send", Arguments: map[string]interface{}{"to": "ana@example.com"}, IdempotencyKey: key,
		ExternalOperation: &ExternalOperationIdentity{Resource: "mailbox-ana", Operation: "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// executeLoopPersonAction executes the loop call inline for the Run's lease
// owner, as the governed executor does.
func executeLoopPersonAction(t *testing.T, worker *action.ActionWorker, scope Scope, proposal *ActionProposalResult) *ActionExecutionResult {
	t.Helper()
	result, err := worker.ExecuteOwnLoopCall(context.Background(), scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// openLoopPostgresStore opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openLoopPostgresStore(t testing.TB, prefix string, options ...postgres.PostgresStoreOption) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := postgres.NewPostgresStore(ctx, schema.DSN, append([]postgres.PostgresStoreOption{postgres.WithPostgresSchema(schema.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, schema
}

type loopStoreCase struct {
	name string
	open func(*testing.T) KernelStore
}

// loopStoreMatrix is SQLite always, plus Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set (schema prefix+"_"+uuid[:8], dropped in
// Cleanup). SQLite files go under t.TempDir, which follows TMPDIR (tmpfs).
func loopStoreMatrix(t *testing.T, prefix string) []loopStoreCase {
	t.Helper()
	cases := []loopStoreCase{
		{name: "sqlite", open: func(t *testing.T) KernelStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), prefix+".db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, loopStoreCase{name: "postgres", open: func(t *testing.T) KernelStore {
			store, _ := openLoopPostgresStore(t, prefix+"_")
			return store
		}})
	}
	return cases
}

// eachLoopStore runs test once per store in the matrix, as a subtest per store.
func eachLoopStore(t *testing.T, prefix string, test func(t *testing.T, store KernelStore)) {
	t.Helper()
	for _, c := range loopStoreMatrix(t, prefix) {
		c := c
		t.Run(c.name, func(t *testing.T) { test(t, c.open(t)) })
	}
}

type loopClock struct {
	mu  sync.Mutex
	now time.Time
}

func newLoopClock(start time.Time) *loopClock { return &loopClock{now: start.UTC()} }

func (c *loopClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *loopClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var errLoopCrash = errors.New("simulated crash: replica is dead")

// loopCrashStore wraps a KernelStore. After arm(), every write method the loop
// uses returns errLoopCrash and every read still works. It stands for a
// dead replica: no cleanup, no lease release. The session-store methods go
// to the wrapped store's AgentSessionStore.
type loopCrashStore struct {
	KernelStore
	sessions AgentSessionStore
	dead     atomic.Bool
}

func newLoopCrashStore(t testing.TB, store KernelStore) *loopCrashStore {
	t.Helper()
	sessions, ok := store.(AgentSessionStore)
	if !ok {
		t.Fatalf("crash store: %T is not an AgentSessionStore", store)
	}
	return &loopCrashStore{KernelStore: store, sessions: sessions}
}

func (s *loopCrashStore) arm()    { s.dead.Store(true) }
func (s *loopCrashStore) disarm() { s.dead.Store(false) }

func (s *loopCrashStore) refused() error {
	if s.dead.Load() {
		return errLoopCrash
	}
	return nil
}

// Every overridden method is listed here, so a signature change in a store
// interface breaks the kit's build instead of silently bypassing the crash.
var _ = []any{
	(*loopCrashStore).OpenConversationAgentSession,
	(*loopCrashStore).CreateRunAgentSession,
	(*loopCrashStore).AppendAgentSessionEntries,
	(*loopCrashStore).TransitionToolEntry,
	(*loopCrashStore).GetAgentSession,
	(*loopCrashStore).GetSessionEntry,
	(*loopCrashStore).LoadAgentSessionPath,
	(*loopCrashStore).ListAgentSessionEntries,
	(*loopCrashStore).CreateAgentRun,
	(*loopCrashStore).CreateAgentRunWithEvent,
	(*loopCrashStore).UpdateAgentRunWithEvent,
	(*loopCrashStore).AppendActivity,
	(*loopCrashStore).ClaimNextAgentRun,
	(*loopCrashStore).RenewAgentRunLease,
	(*loopCrashStore).CreateAgentTurn,
	(*loopCrashStore).UpdateAgentTurn,
	(*loopCrashStore).ClaimAgentTurn,
	(*loopCrashStore).RecordAgentTurnStepModel,
	(*loopCrashStore).CreateActionProposal,
	(*loopCrashStore).ResolveApproval,
	(*loopCrashStore).ClaimNextAction,
	(*loopCrashStore).RenewActionLease,
	(*loopCrashStore).PersistActionExecution,
}

var _ KernelStore = (*loopCrashStore)(nil)
var _ AgentSessionStore = (*loopCrashStore)(nil)

// Session store.

func (s *loopCrashStore) OpenConversationAgentSession(ctx context.Context, req OpenConversationAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.OpenConversationAgentSession(ctx, req)
}

func (s *loopCrashStore) CreateRunAgentSession(ctx context.Context, req CreateRunAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.CreateRunAgentSession(ctx, req)
}

func (s *loopCrashStore) AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.AppendAgentSessionEntries(ctx, req)
}

func (s *loopCrashStore) TransitionToolEntry(ctx context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.TransitionToolEntry(ctx, req)
}

func (s *loopCrashStore) GetAgentSession(ctx context.Context, scope Scope, sessionID string) (*AgentSession, error) {
	return s.sessions.GetAgentSession(ctx, scope, sessionID)
}

func (s *loopCrashStore) GetSessionEntry(ctx context.Context, scope Scope, entryID string) (*SessionEntry, error) {
	return s.sessions.GetSessionEntry(ctx, scope, entryID)
}

func (s *loopCrashStore) LoadAgentSessionPath(ctx context.Context, scope Scope, sessionID string) (*AgentSessionPath, error) {
	return s.sessions.LoadAgentSessionPath(ctx, scope, sessionID)
}

func (s *loopCrashStore) ListAgentSessionEntries(ctx context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	return s.sessions.ListAgentSessionEntries(ctx, filter)
}

// Runs and activity.

func (s *loopCrashStore) CreateAgentRun(ctx context.Context, run *AgentRun) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.CreateAgentRun(ctx, run)
}

func (s *loopCrashStore) CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentRunWithEvent(ctx, run, event)
}

func (s *loopCrashStore) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
}

func (s *loopCrashStore) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.AppendActivity(ctx, event)
}

func (s *loopCrashStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAgentRun(ctx, claim)
}

func (s *loopCrashStore) RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewAgentRunLease(ctx, scope, runID, workerID, now, leaseDuration)
}

// Steps.

func (s *loopCrashStore) CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentTurn(ctx, turn)
}

func (s *loopCrashStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.UpdateAgentTurn(ctx, turn, expectedRevision, workerID)
}

func (s *loopCrashStore) ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimAgentTurn(ctx, scope, turnID, workerID, now, leaseDuration)
}

func (s *loopCrashStore) RecordAgentTurnStepModel(ctx context.Context, turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.RecordAgentTurnStepModel(ctx, turn, expectedRevision, events)
}

// Actions and approvals.

func (s *loopCrashStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateActionProposal(ctx, proposal)
}

func (s *loopCrashStore) ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ResolveApproval(ctx, resolution)
}

func (s *loopCrashStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAction(ctx, claim)
}

func (s *loopCrashStore) RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewActionLease(ctx, scope, actionID, workerID, now, leaseDuration)
}

func (s *loopCrashStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.PersistActionExecution(ctx, execution)
}
