package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ConversationTaskAdmissionStore is what the admission fixtures write to.
type ConversationTaskAdmissionStore interface {
	ConversationTaskStore
	models.ConversationStore
	models.AgentRunScheduleStore
	models.AgentRunAdmissionStore
}

// ConversationTaskAdmissionRecord is ConversationTaskRecord plus the stored
// conversation and the channel message the task's actor was derived from,
// which admission reads back.
func ConversationTaskAdmissionRecord(t testing.TB, store ConversationTaskAdmissionStore, suffix string) models.ConversationTaskCreateRecord {
	t.Helper()
	record := ConversationTaskRecord(t, store, suffix)
	task := record.Task
	conversation, err := store.GetConversation(t.Context(), task.Scope, task.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation == nil {
		SeedSpace(t, store.(models.SpaceStore), task.Scope)
		conversation, _, err = store.CreateConversation(t.Context(), &models.Conversation{
			ID: task.ConversationID, Scope: task.Scope, Owner: task.Owner, Title: "Task admission",
			Status: models.ConversationStatusActive, Revision: 1, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt,
		}, "task-admission-chat")
		if err != nil {
			t.Fatal(err)
		}
	}
	message := &models.ChannelMessage{ID: task.SourceMessageID, Scope: task.Scope, ConversationID: task.ConversationID,
		Sequence: conversation.LastSequence + 1, Sender: task.AuthenticatedActor, Intent: models.MessageIntentQuestion,
		Content: "Prepare the requested background result", ThreadRootID: task.ThreadRootID,
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: task.SourceMessageID, CreatedAt: task.CreatedAt}
	next := models.CloneConversation(conversation)
	next.Revision++
	next.LastSequence = message.Sequence
	if _, err := store.CommitChannelMessage(t.Context(), models.ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: conversation.Revision, Message: message}); err != nil {
		t.Fatal(err)
	}
	return record
}

// CreateConversationTaskAdmissionTask admits the task of
// ConversationTaskAdmissionRecord.
func CreateConversationTaskAdmissionTask(t testing.TB, store ConversationTaskAdmissionStore, suffix string) *models.ConversationTaskResult {
	t.Helper()
	result, err := store.CreateConversationTask(t.Context(), ConversationTaskAdmissionRecord(t, store, suffix))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// ConversationTaskAdmissionClaim claims task work of the admission fixtures'
// agent, leaving reserve of its four slots to the foreground.
func ConversationTaskAdmissionClaim(now time.Time, reserve int) models.AgentRunClaim {
	return models.AgentRunClaim{Scope: models.Scope{Kind: "tenant", ID: "task-sql"}, Kind: models.RunKindAgentWork,
		WorkerID: "task-admission-worker", AssignedAgentID: "task-agent", Now: now,
		LeaseDuration: 5 * time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4,
		ConversationTaskForegroundReserve: reserve}
}

// SeedSpace gives the owner with no ID a default space in scope, which a
// conversation created without a space goes to. The first space an owner
// has in a scope is their default; a scope that has one keeps it.
func SeedSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(t.Context(), scope, ""); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	now := time.Now().UTC()
	space, err := store.CreateSpace(t.Context(), &models.Space{ID: "home", Scope: scope, Name: "Home", Look: models.DefaultSpaceLook,
		Access: models.SpaceAccessOwner, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}
