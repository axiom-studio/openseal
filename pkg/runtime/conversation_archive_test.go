package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestConversationArchiveImportIsProvenanceLinkedAndCrashResumable(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "conversation-archive.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			service := conversation.NewConversationService(store.(models.ConversationStore))
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "archive"}
			seedTestSpace(t, store.(models.SpaceStore), scope)
			start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
			request := conversation.ImportConversationArchiveRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "operations"},
				Title:          "Imported launch history",
				Source:         conversation.ConversationArchiveSource{System: "legacy-system", ResourceType: "team-chat", ResourceID: "conversation-42"},
				IdempotencyKey: "legacy-system-team-chat-conversation-42",
				Messages: []conversation.ConversationArchiveMessage{
					{
						RecordID: "message-1", Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "23"},
						Intent: models.MessageIntentQuestion, Content: "Is the launch ready?",
						Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: true, CreatedAt: start,
					},
					{
						RecordID: "message-2", Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "37"},
						SenderDisplayName: "Release accountant",
						Intent:            models.MessageIntentAnswer, Content: "The signed evidence is attached.",
						Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToRecordID: "message-1", CreatedAt: start.Add(time.Minute),
					},
				},
			}

			partial := request
			partial.Messages = partial.Messages[:1]
			first, err := service.ImportArchive(ctx, partial)
			if err != nil || first.ImportedMessages != 1 || first.ReplayedMessages != 0 || first.Replayed ||
				!first.Conversation.CreatedAt.Equal(start) || first.Conversation.Origin == nil ||
				first.Conversation.Origin.ID != "legacy-system:team-chat:conversation-42" {
				t.Fatalf("partial import = %#v, %v", first, err)
			}
			resumed, err := service.ImportArchive(ctx, request)
			if err != nil || resumed.ImportedMessages != 1 || resumed.ReplayedMessages != 1 || resumed.Replayed ||
				len(resumed.Messages) != 2 || resumed.Conversation.LastSequence != 2 {
				t.Fatalf("resumed import = %#v, %v", resumed, err)
			}
			answer := resumed.Messages[1]
			if !answer.Historical || answer.SenderDisplayName != "Release accountant" || !answer.CreatedAt.Equal(start.Add(time.Minute)) ||
				answer.ReplyToMessageID != resumed.Messages[0].ID ||
				answer.ThreadRootID != resumed.Messages[0].ID || len(answer.References) != 1 ||
				answer.References[0].Kind != models.ConversationReferenceExternalSource ||
				answer.References[0].ID != "legacy-system:team-chat:conversation-42:message-2" {
				t.Fatalf("imported answer = %#v", answer)
			}

			replayed, err := service.ImportArchive(ctx, request)
			if err != nil || !replayed.Replayed || replayed.ImportedMessages != 0 || replayed.ReplayedMessages != 2 ||
				len(replayed.Messages) != 2 {
				t.Fatalf("replayed import = %#v, %v", replayed, err)
			}
			scheduler, err := conversationrun.NewConversationRunScheduler(store.(models.ConversationStore), store, conversationrun.ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			reconciled, err := scheduler.ReconcileScope(ctx, scope)
			if err != nil || reconciled.Scheduled != 0 || reconciled.Skipped != 2 {
				t.Fatalf("historical reconciliation = %#v, %v", reconciled, err)
			}
			runs, err := store.ListAgentRuns(ctx, models.AgentRunFilter{Scope: scope, Kind: models.RunKindConversation})
			if err != nil || len(runs) != 0 {
				t.Fatalf("historical Runs = %#v, %v", runs, err)
			}
		})
	}
}

func TestConversationArchiveImportRejectsUnknownReplyAndDuplicateRecords(t *testing.T) {
	store := kerneltest.NewStore(t)
	seedTestSpace(t, store, models.Scope{Kind: "tenant", ID: "archive"})
	service := conversation.NewConversationService(store)
	base := conversation.ImportConversationArchiveRequest{
		Scope: models.Scope{Kind: "tenant", ID: "archive"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "operations"},
		Title: "Imported history", Source: conversation.ConversationArchiveSource{System: "source", ResourceType: "chat", ResourceID: "1"},
		IdempotencyKey: "archive-1", Messages: []conversation.ConversationArchiveMessage{{
			RecordID: "message-1", Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "23"},
			Intent: models.MessageIntentUpdate, Content: "Status", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			CreatedAt: time.Now().UTC(), ReplyToRecordID: "missing",
		}},
	}
	if _, err := service.ImportArchive(t.Context(), base); err == nil {
		t.Fatal("unknown reply source record was accepted")
	}
	base.Messages[0].ReplyToRecordID = ""
	base.Messages = append(base.Messages, base.Messages[0])
	if _, err := service.ImportArchive(t.Context(), base); err == nil {
		t.Fatal("duplicate source record was accepted")
	}
}
