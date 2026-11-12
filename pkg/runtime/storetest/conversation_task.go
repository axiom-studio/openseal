// Package storetest holds test support shared by the SQL stores: fixtures
// built only through the store interfaces in pkg/models, and the bodies of
// suites each backend runs against itself. It never opens a store; the caller
// passes one in.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// ConversationTaskStore is what the conversation-task fixtures write to.
type ConversationTaskStore interface {
	models.PortfolioStore
	models.RunActivityStore
	models.ConversationTaskStore
}

// RunUpdater replaces a stored Run with a newer revision.
type RunUpdater interface {
	UpdateAgentRunWithEvent(ctx context.Context, run *models.AgentRun, expectedRevision int64, event *models.ActivityEvent, lease *models.AgentRunLeaseGuard) (*models.ActivityEvent, error)
}

// ConversationTaskFixtureTime is the creation time of every
// ConversationTaskRecord, so records built in one test sort by ID alone.
var ConversationTaskFixtureTime = time.Now().UTC().Truncate(time.Microsecond)

// ConversationTaskRecord stores the running source Run of a conversation
// task and returns the record that admits the task: the task, its queued work
// Run and the completed source Run. Nothing of the task is stored yet.
func ConversationTaskRecord(t testing.TB, store ConversationTaskStore, suffix string) models.ConversationTaskCreateRecord {
	t.Helper()
	now := ConversationTaskFixtureTime
	expires := time.Now().UTC().Add(10 * time.Minute)
	scope := models.Scope{Kind: "tenant", ID: "task-sql"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"}
	sourceID := "source-" + suffix
	source := &models.AgentRun{ID: sourceID, Scope: scope, Kind: models.RunKindConversation, RootRunID: sourceID, Owner: owner,
		AssignedAgentID: owner.ID, Goal: "Accept conversation work", Source: models.RunSourceChat, Status: models.AgentRunStatusRunning,
		Priority: 90, AvailableAt: now, QueueEnteredAt: now, LeaseOwner: "task-worker", LeaseExpiresAt: &expires,
		LastAppliedTurn: 1, Budget: &models.BudgetPolicy{MaxTurns: 20}, Policy: map[string]interface{}{"preserve": "source-policy"},
		Context:  map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation", models.ConversationRunTriggerIDContextKey: "message-" + suffix},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAgentRun(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	taskID := models.ConversationTaskID(scope, sourceID, "task-"+suffix)
	workID := "work-" + suffix
	task := &models.ConversationTask{ID: taskID, Scope: scope, Owner: owner, ConversationID: "conversation", SourceMessageID: "message-" + suffix,
		ThreadRootID: "thread", SourceRunID: sourceID, SourceTurnID: "turn-" + suffix, SourceTurnNumber: 1,
		AuthenticatedActor: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, TargetAgentID: owner.ID,
		WorkRunID: workID, TaskKey: "task-" + suffix, Goal: "Prepare the requested result", Acknowledgment: "I have started the work.",
		RequestedBudget: &models.BudgetPolicy{MaxTurns: 5}, Revision: 1, CreatedAt: now}
	var err error
	task.RequestDigest, err = models.ConversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	work := &models.AgentRun{ID: workID, Scope: scope, Kind: models.RunKindAgentWork, RootRunID: workID, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: "task:" + taskID, Goal: task.Goal, Source: models.RunSourceChat, Status: models.AgentRunStatusQueued, Priority: source.Priority,
		AvailableAt: now, QueueEnteredAt: now, Budget: models.CloneBudgetPolicy(task.RequestedBudget), Revision: 1, CreatedAt: now, UpdatedAt: now,
		Context: map[string]interface{}{models.ConversationTaskContextKey: taskID, models.ConversationRunConversationIDContextKey: task.ConversationID,
			models.ConversationRunTriggerIDContextKey: task.SourceMessageID, models.RunReportingRootRunIDContextKey: workID,
			models.RunReportingMilestonesContextKey: []interface{}{"completed", "failed"}}}
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, workID, work.Budget); err != nil {
		t.Fatal(err)
	}
	updated.Status = models.AgentRunStatusCompleted
	updated.Context[models.RunReportingRootRunIDContextKey] = source.ID
	updated.Context[models.RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": task.Acknowledgment, "conversationTaskIds": []string{taskID}, "conversationTaskWorkRunId": workID}
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	event := &models.ActivityEvent{ID: "task-event-" + suffix, Scope: scope, RunID: sourceID, EventType: "conversation.task_started",
		Summary: "Started independent work", Actor: models.ActivityActor{Type: "user", ID: "requester"}, CorrelationID: taskID,
		Visibility: models.ActivityVisibilityScope, CreatedAt: now}
	record := models.ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated, ExpectedSourceRevision: 1, WorkerID: "task-worker", Event: event}
	if err := models.ValidateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("invalid SQL fixture: %v", err)
	}
	return record
}

// UpdateRun stores run, whose Revision is expectedRevision+1, with a
// run.updated event.
func UpdateRun(t testing.TB, store RunUpdater, run *models.AgentRun, expectedRevision int64) {
	t.Helper()
	event := &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated",
		Summary: "Update SQL task test run", Actor: models.ActivityActor{Type: "user", ID: "requester"}, Visibility: models.ActivityVisibilityScope, CreatedAt: run.UpdatedAt}
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expectedRevision, event, nil); err != nil {
		t.Fatal(err)
	}
}

// CompleteRun completes a stored Run one second after its last update.
func CompleteRun(t testing.TB, store RunUpdater, old *models.AgentRun) {
	t.Helper()
	run := models.CloneAgentRun(old)
	now := run.UpdatedAt.Add(time.Second)
	run.Status, run.CompletedAt, run.UpdatedAt, run.Revision = models.AgentRunStatusCompleted, &now, now, run.Revision+1
	event := &models.ActivityEvent{ID: "benchmark-complete-" + run.ID, Scope: run.Scope, RunID: run.ID, EventType: "run.updated", Summary: "Completed task benchmark fixture", Actor: models.ActivityActor{Type: "user", ID: "requester"}, Visibility: models.ActivityVisibilityScope, CreatedAt: now}
	if _, err := store.UpdateAgentRunWithEvent(context.Background(), run, old.Revision, event, nil); err != nil {
		t.Fatal(err)
	}
}

// ConversationReadRun returns an unsaved Run of the conversation read
// fixtures: a root of its own, created and available at `at`.
func ConversationReadRun(id string, scope models.Scope, owner models.ObjectiveOwner, kind models.RunKind, status models.AgentRunStatus, at time.Time) *models.AgentRun {
	run := &models.AgentRun{ID: id, Scope: scope, Owner: owner, Kind: kind, RootRunID: id, AssignedAgentID: owner.ID,
		Goal: "Conversation read fixture", Source: models.RunSourceChat, Status: status, Priority: 10, AvailableAt: at, QueueEnteredAt: at,
		Revision: 1, CreatedAt: at, UpdatedAt: at}
	if models.IsTerminalAgentRunStatus(status) {
		run.CompletedAt = &at
	}
	return run
}
