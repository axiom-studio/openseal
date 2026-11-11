package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// dependencySessionStore is a backend under test for the dependency-group
// create's session half (CTL-5a).
type dependencySessionStore interface {
	agentSessionConformanceStore
	RunDependencyStore
}

// forEachDependencySessionStore runs fn against SQLite and, when
// OPENSEAL_TEST_POSTGRES_DSN is set, Postgres.
func forEachDependencySessionStore(t *testing.T, fn func(f *sessionConformance, store dependencySessionStore)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T) dependencySessionStore
	}{
		{"sqlite", func(t *testing.T) dependencySessionStore { return openTestSQLiteSessionStore(t) }},
		{"postgres", openDependencySessionPostgres},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			f := &sessionConformance{
				t: t, ctx: context.Background(), store: store,
				scope: Scope{Kind: "tenant", ID: "fork-tx-" + uuid.NewString()[:8]},
				now:   time.Now().UTC().Truncate(time.Millisecond),
			}
			fn(f, store)
		})
	}
}

func openDependencySessionPostgres(t *testing.T) dependencySessionStore {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("dependency_session_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	return dependencySessionPostgres{store}
}

// dependencySessionPostgres adds the conformance fixture's fork hook to the
// Postgres store (that hook lives in the integration-tagged test file).
type dependencySessionPostgres struct{ *PostgresStore }

func (s dependencySessionPostgres) conformanceCreateRunAgentSessionTx(ctx context.Context, req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := s.createRunAgentSessionTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// forkTxFixture is a leased conversation Run whose last step called a
// control tool (entry "t1", still pending), plus the record that forks it
// into two same-Agent children.
type forkTxFixture struct {
	source    *AgentRun
	session   *AgentSession
	assistant *SessionEntry
	groupID   string
	childIDs  []string
}

func newForkTxFixture(f *sessionConformance) *forkTxFixture {
	f.t.Helper()
	source, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, source.ID, testUserEntry("u1", "compare the two vendors"))
	appended := f.mustAppend(session.ID, source.ID,
		testAssistantEntry("a1", "s1", "c1"),
		testToolEntry("t1", "s1", "c1", ToolEntryPending))
	return &forkTxFixture{
		source:    f.run(source.ID),
		session:   session,
		assistant: appended.Entries[0],
		groupID:   "group-" + source.ID,
		childIDs:  []string{"child-1-" + source.ID, "child-2-" + source.ID},
	}
}

// record builds the create record from the source as read at plan time.
func (x *forkTxFixture) record(f *sessionConformance) RunDependencyGroupCreateRecord {
	f.t.Helper()
	source := x.source
	children := make([]*AgentRun, 0, len(x.childIDs))
	edges := make([]*RunDependency, 0, len(x.childIDs))
	sessions := make([]CreateRunAgentSessionTxRequest, 0, len(x.childIDs))
	for i, childID := range x.childIDs {
		child, err := buildAgentRun(f.ctx, f.store, CreateAgentRunRequest{
			Kind: RunKindAgentWork, Scope: f.scope, ParentRunID: source.ID, Owner: source.Owner,
			AssignedAgentID: source.AssignedAgentID, Goal: "branch goal", Source: RunSourceFork,
			ConcurrencyKey: "fork:" + x.groupID + ":" + childID, SessionID: RunAgentSessionID(f.scope, childID),
		}, childID, f.now)
		if err != nil {
			f.t.Fatalf("build child: %v", err)
		}
		children = append(children, child)
		edges = append(edges, &RunDependency{
			ID: "branch-" + string(rune('1'+i)), Scope: f.scope, GroupID: x.groupID, SourceRunID: source.ID, TargetRunID: childID,
			Kind: RunDependencyKindRun, State: RunDependencyStateRunning, Required: true, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
		})
		sessions = append(sessions, CreateRunAgentSessionTxRequest{
			Scope: f.scope, OwnerRunID: childID, AgentID: source.AssignedAgentID, ConversationID: "c1",
			ParentSessionID: x.session.ID, ForkSequence: x.assistant.Sequence - 1,
			InitialEntries: []*SessionEntry{goalEntry("goal-"+childID, childID)}, Now: f.now,
		})
	}
	sealed := f.now
	group := &RunDependencyGroup{
		ID: x.groupID, Scope: f.scope, SourceRunID: source.ID, Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		ExpectedCount: len(edges), Status: RunDependencyGroupWaiting, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now, SealedAt: &sealed,
		IdempotencyKey: "fork:" + x.groupID,
	}
	updated := cloneAgentRun(source)
	updated.Status = AgentRunStatusWaitingForDependency
	updated.WakeCondition = &WakeCondition{Type: "run_dependencies", Reference: x.groupID}
	updated.Revision++
	updated.UpdatedAt = f.now
	updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	count := len(source.PendingInterventions)
	return RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, TargetRuns: children, SourceRun: updated, ExpectedSourceRevision: source.Revision,
		Event: &ActivityEvent{
			ID: uuid.NewString(), Scope: f.scope, RunID: source.ID, AgentID: source.AssignedAgentID, EventType: "run.forked",
			Severity: ActivitySeverityInfo, Summary: "Forked 2 concurrent child Runs", Actor: ActivityActor{Type: "worker", ID: conformanceWorker},
			Visibility: ActivityVisibilityScope, CorrelationID: x.groupID, CreatedAt: f.now,
		},
		ToolTransition: &ToolEntryTransitionRequest{
			Scope: f.scope, EntryID: "t1", RunID: source.ID, Lease: f.lease(), From: ToolEntryPending, To: ToolEntryAwaitingChild,
			Wait: &ToolEntryWait{GroupID: x.groupID, ChildRunIDs: append([]string(nil), x.childIDs...)}, Now: f.now,
		},
		ChildSessions:            sessions,
		RequireInterventionCount: &count,
	}
}

// assertNothingWritten checks that a refused create left no trace.
func (x *forkTxFixture) assertNothingWritten(f *sessionConformance, store dependencySessionStore, activityBefore int) {
	f.t.Helper()
	if group, err := store.GetRunDependencyGroup(f.ctx, f.scope, x.groupID); err != nil || group != nil {
		f.t.Fatalf("group after refusal = %+v, %v", group, err)
	}
	for _, childID := range x.childIDs {
		if child, err := store.GetAgentRun(f.ctx, f.scope, childID); child != nil || (err != nil && !errors.Is(err, ErrRunNotFound)) {
			f.t.Fatalf("child %s after refusal = %+v, %v", childID, child, err)
		}
		if _, err := store.GetAgentSession(f.ctx, f.scope, RunAgentSessionID(f.scope, childID)); !errors.Is(err, ErrAgentSessionNotFound) {
			f.t.Fatalf("child session %s after refusal: %v", childID, err)
		}
	}
	if entry := f.entry("t1"); entry.State != ToolEntryPending || entry.Tool.Wait != nil {
		f.t.Fatalf("tool entry after refusal = %s %+v", entry.State, entry.Tool.Wait)
	}
	if run := f.run(x.source.ID); run.Status != AgentRunStatusRunning || run.LeaseOwner != conformanceWorker {
		f.t.Fatalf("source after refusal = %s lease %q", run.Status, run.LeaseOwner)
	}
	if got := f.activityCount(x.source.ID); got != activityBefore {
		f.t.Fatalf("activity after refusal = %d, want %d", got, activityBefore)
	}
}

func TestForkCreateTransitionsEntryAndCreatesSessionsAtomically(t *testing.T) {
	forEachDependencySessionStore(t, func(f *sessionConformance, store dependencySessionStore) {
		x := newForkTxFixture(f)
		before := f.activityCount(x.source.ID)

		// A failing tool transition (wrong From) rolls back the whole create.
		bad := x.record(f)
		bad.ToolTransition.From = ToolEntryRunning
		_, err := store.CreateRunDependencyGroup(f.ctx, bad)
		expectErr(f.t, "stale tool state", err, ErrToolEntryStateConflict)
		x.assertNothingWritten(f, store, before)

		// A failing child session (fork point past the parent leaf) too.
		bad = x.record(f)
		bad.ChildSessions[1].ForkSequence = f.session(x.session.ID).NextSequence + 5
		_, err = store.CreateRunDependencyGroup(f.ctx, bad)
		expectErr(f.t, "fork past leaf", err, ErrInvalidAgentSession)
		x.assertNothingWritten(f, store, before)

		// Another worker's lease is refused before anything is written.
		bad = x.record(f)
		bad.ToolTransition.Lease = AgentRunLeaseGuard{WorkerID: "other-worker", Now: f.now}
		_, err = store.CreateRunDependencyGroup(f.ctx, bad)
		expectErr(f.t, "foreign lease", err, ErrLeaseLost)
		x.assertNothingWritten(f, store, before)

		// A cross-Agent child cannot fork the source session.
		bad = x.record(f)
		bad.ChildSessions[0].AgentID = "agent-b"
		_, err = store.CreateRunDependencyGroup(f.ctx, bad)
		expectErr(f.t, "cross-Agent child session", err, ErrInvalidRunDependency)

		record := x.record(f)
		result, err := store.CreateRunDependencyGroup(f.ctx, record)
		if err != nil {
			f.t.Fatalf("create: %v", err)
		}
		if result.Replayed || result.ToolEntry == nil || result.ToolEntry.State != ToolEntryAwaitingChild ||
			result.ToolEntry.Tool.Wait == nil || result.ToolEntry.Tool.Wait.GroupID != x.groupID || len(result.ToolEntry.Tool.Wait.ChildRunIDs) != 2 {
			f.t.Fatalf("create result entry = %+v", result.ToolEntry)
		}
		if len(result.ChildSessions) != 2 {
			f.t.Fatalf("child sessions = %d", len(result.ChildSessions))
		}
		if entry := f.entry("t1"); entry.State != ToolEntryAwaitingChild || entry.Tool.Wait.GroupID != x.groupID {
			f.t.Fatalf("stored tool entry = %s %+v", entry.State, entry.Tool.Wait)
		}
		source := f.run(x.source.ID)
		if source.Status != AgentRunStatusWaitingForDependency || source.LeaseOwner != "" || source.SessionID != x.session.ID {
			f.t.Fatalf("source = %s lease %q session %q", source.Status, source.LeaseOwner, source.SessionID)
		}
		for i, childID := range x.childIDs {
			session := result.ChildSessions[i]
			if session.ID != RunAgentSessionID(f.scope, childID) || session.ParentSessionID != x.session.ID ||
				session.ForkEntryID != "u1" || session.ForkSequence != x.assistant.Sequence-1 || session.Depth != 1 || session.OwnerRunID != childID {
				f.t.Fatalf("child session %d = %+v", i, session)
			}
			if child := f.run(childID); child.SessionID != session.ID || child.Status != AgentRunStatusQueued || child.ParentRunID != x.source.ID {
				f.t.Fatalf("child run %s = session %q status %s", childID, child.SessionID, child.Status)
			}
			// The child sees the conversation up to, not including, the
			// forking assistant entry: no dangling call.
			if ids := pathIDs(f.path(session.ID)); ids != "u1,goal-"+childID {
				f.t.Fatalf("child path = %s", ids)
			}
		}
		if got := f.activityCount(x.source.ID); got != before+1 {
			f.t.Fatalf("activity after create = %d, want %d", got, before+1)
		}

		// A replay (crash re-settle) writes nothing and reports the state.
		replay, err := store.CreateRunDependencyGroup(f.ctx, x.record(f))
		if err != nil || !replay.Replayed {
			f.t.Fatalf("replay = %+v, %v", replay, err)
		}
		if replay.ToolEntry == nil || replay.ToolEntry.State != ToolEntryAwaitingChild || len(replay.ChildSessions) != 2 {
			f.t.Fatalf("replay state = %+v sessions %d", replay.ToolEntry, len(replay.ChildSessions))
		}
		if got := f.activityCount(x.source.ID); got != before+1 {
			f.t.Fatalf("activity after replay = %d, want %d", got, before+1)
		}
	})
}

func TestForkCreateRefusedOnNewIntervention(t *testing.T) {
	forEachDependencySessionStore(t, func(f *sessionConformance, store dependencySessionStore) {
		x := newForkTxFixture(f)
		record := x.record(f) // planned with zero interventions
		// A message arrives between the plan and the create; the lease is kept.
		steered := f.updateRun(x.source.ID, func(run *AgentRun) {
			run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{
				ID: "steer-1", Actor: ActivityActor{Type: "user", ID: "u1"}, Instruction: "only the cheaper one", CreatedAt: f.now,
			})
		})
		before := f.activityCount(x.source.ID)
		_, err := store.CreateRunDependencyGroup(f.ctx, record)
		expectErr(f.t, "steered create", err, ErrSteeringArrived)
		x.source = steered
		x.assertNothingWritten(f, store, before)

		// Re-planned with the new count (and revision) the create goes through.
		replanned := x.record(f)
		if _, err := store.CreateRunDependencyGroup(f.ctx, replanned); err != nil {
			f.t.Fatalf("replanned create: %v", err)
		}
		if entry := f.entry("t1"); entry.State != ToolEntryAwaitingChild {
			f.t.Fatalf("tool entry after replanned create = %s", entry.State)
		}
	})
}
