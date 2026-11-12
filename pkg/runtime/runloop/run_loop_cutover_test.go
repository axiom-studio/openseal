package runloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
)

type cutoverTestStore interface {
	models.RunLoopCutoverStore
	models.ConversationStore
	models.AgentRunScheduleStore
}

func cutoverStores() []struct {
	name string
	open func(*testing.T) cutoverTestStore
} {
	stores := []struct {
		name string
		open func(*testing.T) cutoverTestStore
	}{
		{name: "sqlite", open: func(t *testing.T) cutoverTestStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "cutover.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		stores = append(stores, struct {
			name string
			open func(*testing.T) cutoverTestStore
		}{name: "postgres", open: func(t *testing.T) cutoverTestStore {
			store, _ := kerneltest.OpenPostgres(t, "openseal_cutover_")
			return store
		}})
	}
	return stores
}

var cutoverScope = models.Scope{Kind: "tenant", ID: "cutover"}

// asOldRun rewrites a stored Run as one the old engine created: no Engine
// marker, then mutate.
func asOldRun(t *testing.T, store cutoverTestStore, runID string, mutate func(*models.AgentRun)) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, cutoverScope, runID)
	if err != nil || current == nil {
		t.Fatalf("load %s: %#v, %v", runID, current, err)
	}
	run := models.CloneAgentRun(current)
	// Old-engine Runs carry no engine marker and no session binding.
	run.Engine = ""
	run.SessionID = ""
	if mutate != nil {
		mutate(run)
	}
	run.Revision = current.Revision + 1
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.test_old_engine", Severity: models.ActivitySeverityInfo, RunID: run.ID,
		Actor: models.ActivityActor{Type: "system", ID: "test"}, Summary: "Old engine Run", Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now(),
	}
	if _, err := store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil); err != nil {
		t.Fatalf("rewrite %s as old: %v", runID, err)
	}
	stored, _ := store.GetAgentRun(ctx, cutoverScope, runID)
	return stored
}

func cutoverManualRun(t *testing.T, store cutoverTestStore, key string, parentID string) *models.AgentRun {
	t.Helper()
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: cutoverScope, Owner: owner, AssignedAgentID: owner.ID, ConcurrencyKey: "agent-1:" + key, Goal: "Do " + key,
		Source: models.RunSourceManual, IdempotencyKey: key, ParentRunID: parentID, Actor: models.ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
	if created.Run.Engine != models.RunEngineLoop {
		t.Fatalf("new Run engine = %q", created.Run.Engine)
	}
	return created.Run
}

type cutoverLane struct {
	conversation *models.Conversation
	trigger      *models.ChannelMessage
	run          *models.AgentRun
}

// cutoverConversationRun opens a conversation, posts a request that needs an
// answer and schedules its conversation Run.
func cutoverConversationRun(t *testing.T, store cutoverTestStore, key string) cutoverLane {
	t.Helper()
	return cutoverConversationRunFor(t, store, key, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "operations"})
}

// cutoverAgentConversationRun is cutoverConversationRun in an Agent's
// channel: the Run has an acting Agent and a lane session.
func cutoverAgentConversationRun(t *testing.T, store cutoverTestStore, key string) cutoverLane {
	t.Helper()
	return cutoverConversationRunFor(t, store, key, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"})
}

func cutoverConversationRunFor(t *testing.T, store cutoverTestStore, key string, owner models.ObjectiveOwner) cutoverLane {
	t.Helper()
	ctx := context.Background()
	service := runtimeconversation.NewService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), cutoverScope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: cutoverScope, Owner: owner,
		Title: "Operations " + key, IdempotencyKey: "channel-" + key,
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: cutoverScope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"},
		Intent: models.MessageIntentQuestion, Content: "Check the canary " + key, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "trigger-" + key,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, cutoverScope, conversation.ID, posted.Message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule %s: %#v, %v", key, scheduled, err)
	}
	return cutoverLane{conversation: conversation, trigger: posted.Message, run: scheduled.Run}
}

func claimCutoverRun(t *testing.T, store cutoverTestStore, run *models.AgentRun) *models.AgentRun {
	t.Helper()
	runID := run.ID
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{
		Scope: cutoverScope, Kind: run.Kind, WorkerID: "old-worker", Now: time.Now(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("claim = %#v, %v (want %s)", claimed, err, runID)
	}
	return claimed
}

func cutoverMessages(t *testing.T, store cutoverTestStore, conversationID string) []*models.ChannelMessage {
	t.Helper()
	messages, err := runtimeconversation.NewService(store).ListChannelMessages(context.Background(), models.ChannelMessageFilter{Scope: cutoverScope, ConversationID: conversationID})
	if err != nil {
		t.Fatal(err)
	}
	var cut []*models.ChannelMessage
	for _, message := range messages {
		if message.Intent == models.MessageIntentSystem {
			cut = append(cut, message)
		}
	}
	return cut
}

func runCutover(t *testing.T, store cutoverTestStore) CutoverReport {
	t.Helper()
	report, err := Cutover(context.Background(), store, runtimeconversation.NewService(store))
	if err != nil {
		t.Fatalf("cutover: %v", err)
	}
	return report
}

func requireRunStatus(t *testing.T, store cutoverTestStore, runID string, status models.AgentRunStatus) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), cutoverScope, runID)
	if err != nil || run == nil || run.Status != status {
		t.Fatalf("Run %s = %#v, %v; want %s", runID, run, err, status)
	}
	return run
}

func TestCutoverCancelsOldInFlightRunsOncePerLane(t *testing.T) {
	for _, fixture := range cutoverStores() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.open(t)
			// Lane A: a chat Run the old engine was running, with a running
			// step and an old-engine child.
			laneA := cutoverConversationRun(t, store, "a")
			claimed := claimCutoverRun(t, store, laneA.run)
			asOldRun(t, store, claimed.ID, nil)
			step, err := activity.NewAgentTurnService(store, store).BeginTurn(ctx, activity.BeginAgentTurnRequest{
				Scope: cutoverScope, RunID: claimed.ID, Producer: models.StepProducerModel, WorkerID: "old-worker", LeaseDuration: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			child := cutoverManualRun(t, store, "child-a", claimed.ID)
			asOldRun(t, store, child.ID, nil)
			// Lane B: a chat Run that already applied an old Turn and was
			// requeued.
			laneB := cutoverConversationRun(t, store, "b")
			asOldRun(t, store, laneB.run.ID, func(run *models.AgentRun) { run.LastAppliedTurn = 1 })
			// A Run the new engine created is not touched.
			fresh := cutoverManualRun(t, store, "loop-run", "")

			report := runCutover(t, store)
			if report.Canceled != 2 || report.Messages != 2 || report.Adopted != 0 || report.Skipped != 1 {
				t.Fatalf("report = %#v", report)
			}
			for _, id := range []string{laneA.run.ID, laneB.run.ID} {
				canceled := requireRunStatus(t, store, id, models.AgentRunStatusCanceled)
				if canceled.FailureExplanation == nil || *canceled.FailureExplanation != *runLoopCutoverExplanation() {
					t.Fatalf("canceled Run %s explanation = %#v", id, canceled.FailureExplanation)
				}
			}
			requireRunStatus(t, store, child.ID, models.AgentRunStatusCanceled)
			if got := requireRunStatus(t, store, fresh.ID, models.AgentRunStatusQueued); got.Revision != fresh.Revision {
				t.Fatalf("loop Run was rewritten: %#v", got)
			}
			// The old running step is closed by the cancel.
			closed, err := store.GetAgentTurn(ctx, cutoverScope, step.ID)
			if err != nil || closed.Status != models.AgentTurnStatusCanceled || closed.FailureCode != agentrun.StepFailureRunEnded {
				t.Fatalf("old step = %#v, %v", closed, err)
			}
			for _, lane := range []cutoverLane{laneA, laneB} {
				messages := cutoverMessages(t, store, lane.conversation.ID)
				if len(messages) != 1 {
					t.Fatalf("lane %s messages = %#v", lane.conversation.ID, messages)
				}
				message := messages[0]
				if message.Content != CutoverMessage || message.ResolvesMessageID != lane.trigger.ID || message.ReplyToMessageID != lane.trigger.ID ||
					message.Sender != (models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "openseal.conversation"}) ||
					len(message.References) != 1 || message.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: lane.run.ID}) {
					t.Fatalf("cutover message = %#v", message)
				}
			}
			// The canceled-run reconciler finds both requests answered.
			scheduler, err := conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			reconciled, err := scheduler.ReconcileScope(ctx, cutoverScope)
			if err != nil || reconciled.Results != 0 {
				t.Fatalf("reconcile after cutover = %#v, %v", reconciled, err)
			}
			if messages := cutoverMessages(t, store, laneA.conversation.ID); len(messages) != 1 {
				t.Fatalf("reconciler doubled the cutover message: %#v", messages)
			}
		})
	}
}

func TestCutoverAdoptsNeverStartedQueuedRun(t *testing.T) {
	for _, fixture := range cutoverStores() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.open(t)
			// Queued but with a step row: it ran on the old engine.
			stepped := cutoverManualRun(t, store, "stepped", "")
			claimCutoverRun(t, store, stepped)
			if _, err := activity.NewAgentTurnService(store, store).BeginTurn(ctx, activity.BeginAgentTurnRequest{
				Scope: cutoverScope, RunID: stepped.ID, Producer: models.StepProducerModel, WorkerID: "old-worker", LeaseDuration: time.Hour,
			}); err != nil {
				t.Fatal(err)
			}
			asOldRun(t, store, stepped.ID, func(run *models.AgentRun) {
				run.Status, run.LeaseOwner, run.LeaseExpiresAt = models.AgentRunStatusQueued, "", nil
			})
			never := asOldRun(t, store, cutoverManualRun(t, store, "never", "").ID, nil)
			// A queued chat Run that never started keeps its request open:
			// the loop answers it, so no message is posted.
			lane := cutoverAgentConversationRun(t, store, "queued")
			laneSessionID := lane.run.SessionID
			if laneSessionID == "" || laneSessionID != models.ConversationSessionID(cutoverScope, lane.run.ConcurrencyKey) {
				t.Fatalf("scheduled chat Run session = %q", laneSessionID)
			}
			queuedChat := asOldRun(t, store, lane.run.ID, nil)
			if queuedChat.SessionID != "" {
				t.Fatalf("old chat Run kept a session: %q", queuedChat.SessionID)
			}
			// A queued conversation task Run without a session cannot be
			// bound by the cutover: it is canceled and its request answered.
			taskLane := cutoverAgentConversationRun(t, store, "task")
			asOldRun(t, store, taskLane.run.ID, func(run *models.AgentRun) {
				run.Context[models.ConversationTaskContextKey] = "task-1"
			})
			// Not queued: a paused Run is canceled even though it never ran.
			paused := asOldRun(t, store, cutoverManualRun(t, store, "paused", "").ID, func(run *models.AgentRun) {
				run.Status, run.PausedFrom = models.AgentRunStatusPaused, models.AgentRunStatusQueued
			})

			report := runCutover(t, store)
			if report.Adopted != 2 || report.Canceled != 3 || report.Messages != 1 {
				t.Fatalf("report = %#v", report)
			}
			for _, old := range []*models.AgentRun{never, queuedChat} {
				adopted := requireRunStatus(t, store, old.ID, models.AgentRunStatusQueued)
				if adopted.Engine != models.RunEngineLoop || adopted.Revision != old.Revision+1 || adopted.LastAppliedTurn != 0 {
					t.Fatalf("adopted Run = %#v", adopted)
				}
			}
			if got := requireRunStatus(t, store, queuedChat.ID, models.AgentRunStatusQueued); got.SessionID != laneSessionID {
				t.Fatalf("adopted chat Run session = %q, want lane %q", got.SessionID, laneSessionID)
			}
			if got := requireRunStatus(t, store, never.ID, models.AgentRunStatusQueued); got.SessionID != "" {
				t.Fatalf("adopted manual Run session = %q", got.SessionID)
			}
			requireRunStatus(t, store, stepped.ID, models.AgentRunStatusCanceled)
			requireRunStatus(t, store, paused.ID, models.AgentRunStatusCanceled)
			requireRunStatus(t, store, taskLane.run.ID, models.AgentRunStatusCanceled)
			// The loop can open the lane session for the adopted chat Run
			// (on stores that already have the session log).
			sessions, ok := store.(models.AgentSessionStore)
			if !ok {
				return
			}
			claimed := claimCutoverRun(t, store, queuedChat)
			conversationID, _ := claimed.Context[models.ConversationRunConversationIDContextKey].(string)
			opened, err := sessions.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{
				Scope: cutoverScope, ConversationID: conversationID, AgentID: claimed.AssignedAgentID, RunID: claimed.ID,
				Lease: models.AgentRunLeaseGuard{WorkerID: claimed.LeaseOwner, Now: time.Now()}, Now: time.Now(),
			})
			if err != nil || opened.Session.ID != laneSessionID {
				t.Fatalf("open lane session = %#v, %v", opened, err)
			}
			if messages := cutoverMessages(t, store, lane.conversation.ID); len(messages) != 0 {
				t.Fatalf("adopted chat Run got a message: %#v", messages)
			}
		})
	}
}

func TestCutoverIdempotent(t *testing.T) {
	for _, fixture := range cutoverStores() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.open(t)
			lane := cutoverConversationRun(t, store, "again")
			asOldRun(t, store, claimCutoverRun(t, store, lane.run).ID, nil)
			queued := asOldRun(t, store, cutoverManualRun(t, store, "queued", "").ID, nil)

			// Two processes start at once: together they do the work once.
			var wg sync.WaitGroup
			reports := make([]CutoverReport, 2)
			errs := make([]error, 2)
			for i := range reports {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					reports[i], errs[i] = Cutover(ctx, store, runtimeconversation.NewService(store))
				}(i)
			}
			wg.Wait()
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("concurrent cutover: %v, %v", errs[0], errs[1])
			}
			if adopted, canceled, messages := reports[0].Adopted+reports[1].Adopted, reports[0].Canceled+reports[1].Canceled, reports[0].Messages+reports[1].Messages; adopted != 1 || canceled != 1 || messages != 1 {
				t.Fatalf("concurrent reports = %#v", reports)
			}
			canceled := requireRunStatus(t, store, lane.run.ID, models.AgentRunStatusCanceled)
			adopted := requireRunStatus(t, store, queued.ID, models.AgentRunStatusQueued)

			again := runCutover(t, store)
			if again != (CutoverReport{}) {
				t.Fatalf("second cutover = %#v", again)
			}
			if run := requireRunStatus(t, store, lane.run.ID, models.AgentRunStatusCanceled); run.Revision != canceled.Revision {
				t.Fatalf("second cutover rewrote the canceled Run: %#v", run)
			}
			if run := requireRunStatus(t, store, queued.ID, models.AgentRunStatusQueued); run.Revision != adopted.Revision {
				t.Fatalf("second cutover rewrote the adopted Run: %#v", run)
			}
			if messages := cutoverMessages(t, store, lane.conversation.ID); len(messages) != 1 {
				t.Fatalf("messages after second cutover = %#v", messages)
			}
		})
	}
}

// failingCutoverStore fails one kind of write the cutover needs.
type failingCutoverStore struct {
	cutoverTestStore
	failList   bool
	failUpdate bool
}

var errCutoverTestFailure = errors.New("store unavailable")

func (s *failingCutoverStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]models.RunLoopCutoverRef, error) {
	if s.failList {
		return nil, errCutoverTestFailure
	}
	return s.cutoverTestStore.ListRunLoopCutoverCandidates(ctx)
}

func (s *failingCutoverStore) UpdateAgentRunWithEvent(ctx context.Context, run *models.AgentRun, expectedRevision int64, event *models.ActivityEvent, lease *models.AgentRunLeaseGuard) (*models.ActivityEvent, error) {
	if s.failUpdate {
		return nil, errCutoverTestFailure
	}
	return s.cutoverTestStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
}

// failingCutoverConversations refuses to post.
type failingCutoverConversations struct {
	*runtimeconversation.Service
}

func (failingCutoverConversations) PostChannelMessage(context.Context, runtimeconversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error) {
	return nil, errCutoverTestFailure
}

func TestCutoverFailsClosed(t *testing.T) {
	for _, fixture := range cutoverStores() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.open(t)
			// The queued Run is older, so a failing write hits its adoption
			// before anything is posted.
			queued := asOldRun(t, store, cutoverManualRun(t, store, "queued", "").ID, nil)
			time.Sleep(2 * time.Millisecond)
			lane := cutoverConversationRun(t, store, "closed")
			running := asOldRun(t, store, claimCutoverRun(t, store, lane.run).ID, nil)

			if _, err := Cutover(ctx, nil, nil); err == nil {
				t.Fatal("cutover without a store succeeded")
			}
			for name, broken := range map[string]*failingCutoverStore{
				"list":   {cutoverTestStore: store, failList: true},
				"update": {cutoverTestStore: store, failUpdate: true},
			} {
				if _, err := Cutover(ctx, broken, runtimeconversation.NewService(store)); !errors.Is(err, errCutoverTestFailure) {
					t.Fatalf("%s failure: cutover err = %v", name, err)
				}
			}
			// A message that cannot be posted stops the pass before the Run
			// is canceled, so the next start posts it.
			if _, err := Cutover(ctx, store, failingCutoverConversations{runtimeconversation.NewService(store)}); !errors.Is(err, errCutoverTestFailure) {
				t.Fatalf("post failure: cutover err = %v", err)
			}
			if run := requireRunStatus(t, store, running.ID, models.AgentRunStatusRunning); run.Engine != "" {
				t.Fatalf("failed cutover marked the Run: %#v", run)
			}
			if messages := cutoverMessages(t, store, lane.conversation.ID); len(messages) != 0 {
				t.Fatalf("failed cutover posted: %#v", messages)
			}

			report := runCutover(t, store)
			if report.Canceled != 1 || report.Messages != 1 {
				t.Fatalf("recovered cutover = %#v", report)
			}
			requireRunStatus(t, store, running.ID, models.AgentRunStatusCanceled)
			if run := requireRunStatus(t, store, queued.ID, models.AgentRunStatusQueued); run.Engine != models.RunEngineLoop {
				t.Fatalf("queued Run was not adopted: %#v", run)
			}
		})
	}
}
