package portfolio_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// dependencySessionStore is a backend under test for the dependency-group
// create's session half (CTL-5a).
type dependencySessionStore interface {
	storetest.AgentSessionContractStore
	models.RunDependencyStore
}

// forEachDependencySessionStore runs fn against SQLite and, when
// OPENSEAL_TEST_POSTGRES_DSN is set, Postgres.
func forEachDependencySessionStore(t *testing.T, fn func(f *storetest.SessionFixture, store dependencySessionStore)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T) dependencySessionStore
	}{
		{"sqlite", func(t *testing.T) dependencySessionStore {
			store, _ := kerneltest.NewSQLiteFileStore(t)
			return store
		}},
		{"postgres", openDependencySessionPostgres},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			f := &storetest.SessionFixture{
				T: t, Ctx: context.Background(), Store: store,
				Scope: models.Scope{Kind: "tenant", ID: "fork-tx-" + uuid.NewString()[:8]},
				Now:   time.Now().UTC().Truncate(time.Millisecond),
			}
			fn(f, store)
		})
	}
}

func openDependencySessionPostgres(t *testing.T) dependencySessionStore {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "dependency_session_")
	dsn := pgSchema.DSN
	ctx := context.Background()
	store, err := postgres.New(ctx, dsn, postgres.WithSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

// forkTxFixture is a leased conversation Run whose last step called a
// control tool (entry "t1", still pending), plus the record that forks it
// into two same-Agent children.
type forkTxFixture struct {
	source    *models.AgentRun
	session   *models.AgentSession
	assistant *models.SessionEntry
	groupID   string
	childIDs  []string
}

func newForkTxFixture(f *storetest.SessionFixture) *forkTxFixture {
	f.T.Helper()
	source, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, source.ID, storetest.UserSessionEntry("u1", "compare the two vendors"))
	appended := f.MustAppend(session.ID, source.ID,
		storetest.AssistantSessionEntry("a1", "s1", "c1"),
		storetest.ToolSessionEntry("t1", "s1", "c1", models.ToolEntryPending))
	return &forkTxFixture{
		source:    f.Run(source.ID),
		session:   session,
		assistant: appended.Entries[0],
		groupID:   "group-" + source.ID,
		childIDs:  []string{"child-1-" + source.ID, "child-2-" + source.ID},
	}
}

// record builds the create record from the source as read at plan time.
func (x *forkTxFixture) record(f *storetest.SessionFixture) models.RunDependencyGroupCreateRecord {
	f.T.Helper()
	source := x.source
	children := make([]*models.AgentRun, 0, len(x.childIDs))
	edges := make([]*models.RunDependency, 0, len(x.childIDs))
	sessions := make([]models.CreateRunAgentSessionTxRequest, 0, len(x.childIDs))
	for i, childID := range x.childIDs {
		child, err := portfolio.BuildAgentRun(f.Ctx, f.Store, models.CreateAgentRunRequest{
			Kind: models.RunKindAgentWork, Scope: f.Scope, ParentRunID: source.ID, Owner: source.Owner,
			AssignedAgentID: source.AssignedAgentID, Goal: "branch goal", Source: models.RunSourceFork,
			ConcurrencyKey: "fork:" + x.groupID + ":" + childID, SessionID: models.RunAgentSessionID(f.Scope, childID),
		}, childID, f.Now)
		if err != nil {
			f.T.Fatalf("build child: %v", err)
		}
		children = append(children, child)
		edges = append(edges, &models.RunDependency{
			ID: "branch-" + string(rune('1'+i)), Scope: f.Scope, GroupID: x.groupID, SourceRunID: source.ID, TargetRunID: childID,
			Kind: models.RunDependencyKindRun, State: models.RunDependencyStateRunning, Required: true, Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
		})
		sessions = append(sessions, models.CreateRunAgentSessionTxRequest{
			Scope: f.Scope, OwnerRunID: childID, AgentID: source.AssignedAgentID, ConversationID: "c1",
			ParentSessionID: x.session.ID, ForkSequence: x.assistant.Sequence - 1,
			InitialEntries: []*models.SessionEntry{storetest.GoalSessionEntry("goal-"+childID, childID)}, Now: f.Now,
		})
	}
	sealed := f.Now
	group := &models.RunDependencyGroup{
		ID: x.groupID, Scope: f.Scope, SourceRunID: source.ID, Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		ExpectedCount: len(edges), Status: models.RunDependencyGroupWaiting, Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now, SealedAt: &sealed,
		IdempotencyKey: "fork:" + x.groupID,
	}
	updated := models.CloneAgentRun(source)
	updated.Status = models.AgentRunStatusWaitingForDependency
	updated.WakeCondition = &models.WakeCondition{Type: "run_dependencies", Reference: x.groupID}
	updated.Revision++
	updated.UpdatedAt = f.Now
	updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	count := len(source.PendingInterventions)
	return models.RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, TargetRuns: children, SourceRun: updated, ExpectedSourceRevision: source.Revision,
		Event: &models.ActivityEvent{
			ID: uuid.NewString(), Scope: f.Scope, RunID: source.ID, AgentID: source.AssignedAgentID, EventType: "run.forked",
			Severity: models.ActivitySeverityInfo, Summary: "Forked 2 concurrent child Runs", Actor: models.ActivityActor{Type: "worker", ID: storetest.SessionWorker},
			Visibility: models.ActivityVisibilityScope, CorrelationID: x.groupID, CreatedAt: f.Now,
		},
		ToolTransition: &models.ToolEntryTransitionRequest{
			Scope: f.Scope, EntryID: "t1", RunID: source.ID, Lease: f.Lease(), From: models.ToolEntryPending, To: models.ToolEntryAwaitingChild,
			Wait: &models.ToolEntryWait{GroupID: x.groupID, ChildRunIDs: append([]string(nil), x.childIDs...)}, Now: f.Now,
		},
		ChildSessions:            sessions,
		RequireInterventionCount: &count,
	}
}

// assertNothingWritten checks that a refused create left no trace.
func (x *forkTxFixture) assertNothingWritten(f *storetest.SessionFixture, store dependencySessionStore, activityBefore int) {
	f.T.Helper()
	if group, err := store.GetRunDependencyGroup(f.Ctx, f.Scope, x.groupID); err != nil || group != nil {
		f.T.Fatalf("group after refusal = %+v, %v", group, err)
	}
	for _, childID := range x.childIDs {
		if child, err := store.GetAgentRun(f.Ctx, f.Scope, childID); child != nil || (err != nil && !errors.Is(err, models.ErrRunNotFound)) {
			f.T.Fatalf("child %s after refusal = %+v, %v", childID, child, err)
		}
		if _, err := store.GetAgentSession(f.Ctx, f.Scope, models.RunAgentSessionID(f.Scope, childID)); !errors.Is(err, models.ErrAgentSessionNotFound) {
			f.T.Fatalf("child session %s after refusal: %v", childID, err)
		}
	}
	if entry := f.Entry("t1"); entry.State != models.ToolEntryPending || entry.Tool.Wait != nil {
		f.T.Fatalf("tool entry after refusal = %s %+v", entry.State, entry.Tool.Wait)
	}
	if run := f.Run(x.source.ID); run.Status != models.AgentRunStatusRunning || run.LeaseOwner != storetest.SessionWorker {
		f.T.Fatalf("source after refusal = %s lease %q", run.Status, run.LeaseOwner)
	}
	if got := f.ActivityCount(x.source.ID); got != activityBefore {
		f.T.Fatalf("activity after refusal = %d, want %d", got, activityBefore)
	}
}

func TestForkCreateTransitionsEntryAndCreatesSessionsAtomically(t *testing.T) {
	forEachDependencySessionStore(t, func(f *storetest.SessionFixture, store dependencySessionStore) {
		x := newForkTxFixture(f)
		before := f.ActivityCount(x.source.ID)

		// A failing tool transition (wrong From) rolls back the whole create.
		bad := x.record(f)
		bad.ToolTransition.From = models.ToolEntryRunning
		_, err := store.CreateRunDependencyGroup(f.Ctx, bad)
		storetest.ExpectErr(f.T, "stale tool state", err, models.ErrToolEntryStateConflict)
		x.assertNothingWritten(f, store, before)

		// A failing child session (fork point past the parent leaf) too.
		bad = x.record(f)
		bad.ChildSessions[1].ForkSequence = f.Session(x.session.ID).NextSequence + 5
		_, err = store.CreateRunDependencyGroup(f.Ctx, bad)
		storetest.ExpectErr(f.T, "fork past leaf", err, models.ErrInvalidAgentSession)
		x.assertNothingWritten(f, store, before)

		// Another worker's lease is refused before anything is written.
		bad = x.record(f)
		bad.ToolTransition.Lease = models.AgentRunLeaseGuard{WorkerID: "other-worker", Now: f.Now}
		_, err = store.CreateRunDependencyGroup(f.Ctx, bad)
		storetest.ExpectErr(f.T, "foreign lease", err, models.ErrLeaseLost)
		x.assertNothingWritten(f, store, before)

		// A cross-Agent child cannot fork the source session.
		bad = x.record(f)
		bad.ChildSessions[0].AgentID = "agent-b"
		_, err = store.CreateRunDependencyGroup(f.Ctx, bad)
		storetest.ExpectErr(f.T, "cross-Agent child session", err, models.ErrInvalidRunDependency)

		record := x.record(f)
		result, err := store.CreateRunDependencyGroup(f.Ctx, record)
		if err != nil {
			f.T.Fatalf("create: %v", err)
		}
		if result.Replayed || result.ToolEntry == nil || result.ToolEntry.State != models.ToolEntryAwaitingChild ||
			result.ToolEntry.Tool.Wait == nil || result.ToolEntry.Tool.Wait.GroupID != x.groupID || len(result.ToolEntry.Tool.Wait.ChildRunIDs) != 2 {
			f.T.Fatalf("create result entry = %+v", result.ToolEntry)
		}
		if len(result.ChildSessions) != 2 {
			f.T.Fatalf("child sessions = %d", len(result.ChildSessions))
		}
		if entry := f.Entry("t1"); entry.State != models.ToolEntryAwaitingChild || entry.Tool.Wait.GroupID != x.groupID {
			f.T.Fatalf("stored tool entry = %s %+v", entry.State, entry.Tool.Wait)
		}
		source := f.Run(x.source.ID)
		if source.Status != models.AgentRunStatusWaitingForDependency || source.LeaseOwner != "" || source.SessionID != x.session.ID {
			f.T.Fatalf("source = %s lease %q session %q", source.Status, source.LeaseOwner, source.SessionID)
		}
		for i, childID := range x.childIDs {
			session := result.ChildSessions[i]
			if session.ID != models.RunAgentSessionID(f.Scope, childID) || session.ParentSessionID != x.session.ID ||
				session.ForkEntryID != "u1" || session.ForkSequence != x.assistant.Sequence-1 || session.Depth != 1 || session.OwnerRunID != childID {
				f.T.Fatalf("child session %d = %+v", i, session)
			}
			if child := f.Run(childID); child.SessionID != session.ID || child.Status != models.AgentRunStatusQueued || child.ParentRunID != x.source.ID {
				f.T.Fatalf("child run %s = session %q status %s", childID, child.SessionID, child.Status)
			}
			// The child sees the conversation up to, not including, the
			// forking assistant entry: no dangling call.
			if ids := storetest.PathEntryIDs(f.Path(session.ID)); ids != "u1,goal-"+childID {
				f.T.Fatalf("child path = %s", ids)
			}
		}
		if got := f.ActivityCount(x.source.ID); got != before+1 {
			f.T.Fatalf("activity after create = %d, want %d", got, before+1)
		}

		// A replay (crash re-settle) writes nothing and reports the state.
		replay, err := store.CreateRunDependencyGroup(f.Ctx, x.record(f))
		if err != nil || !replay.Replayed {
			f.T.Fatalf("replay = %+v, %v", replay, err)
		}
		if replay.ToolEntry == nil || replay.ToolEntry.State != models.ToolEntryAwaitingChild || len(replay.ChildSessions) != 2 {
			f.T.Fatalf("replay state = %+v sessions %d", replay.ToolEntry, len(replay.ChildSessions))
		}
		if got := f.ActivityCount(x.source.ID); got != before+1 {
			f.T.Fatalf("activity after replay = %d, want %d", got, before+1)
		}
	})
}

func TestForkCreateRefusedOnNewIntervention(t *testing.T) {
	forEachDependencySessionStore(t, func(f *storetest.SessionFixture, store dependencySessionStore) {
		x := newForkTxFixture(f)
		record := x.record(f) // planned with zero interventions
		// A message arrives between the plan and the create; the lease is kept.
		steered := f.UpdateRun(x.source.ID, func(run *models.AgentRun) {
			run.PendingInterventions = append(run.PendingInterventions, models.AgentRunIntervention{
				ID: "steer-1", Actor: models.ActivityActor{Type: "user", ID: "u1"}, Instruction: "only the cheaper one", CreatedAt: f.Now,
			})
		})
		before := f.ActivityCount(x.source.ID)
		_, err := store.CreateRunDependencyGroup(f.Ctx, record)
		storetest.ExpectErr(f.T, "steered create", err, models.ErrSteeringArrived)
		x.source = steered
		x.assertNothingWritten(f, store, before)

		// Re-planned with the new count (and revision) the create goes through.
		replanned := x.record(f)
		if _, err := store.CreateRunDependencyGroup(f.Ctx, replanned); err != nil {
			f.T.Fatalf("replanned create: %v", err)
		}
		if entry := f.Entry("t1"); entry.State != models.ToolEntryAwaitingChild {
			f.T.Fatalf("tool entry after replanned create = %s", entry.State)
		}
	})
}
