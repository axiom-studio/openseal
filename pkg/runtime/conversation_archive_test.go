package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationArchiveImportIsProvenanceLinkedAndCrashResumable(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore(50) }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "conversation-archive.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			service := NewConversationService(store.(ConversationStore))
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "archive"}
			start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
			request := ImportConversationArchiveRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
				Title:          "Imported launch history",
				Source:         ConversationArchiveSource{System: "cortex", ResourceType: "team-chat", ResourceID: "conversation-42"},
				IdempotencyKey: "cortex-team-chat-conversation-42",
				Messages: []ConversationArchiveMessage{
					{
						RecordID: "message-1", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "23"},
						Intent: MessageIntentQuestion, Content: "Is the launch ready?",
						Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, CreatedAt: start,
					},
					{
						RecordID: "message-2", Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "37"},
						Intent: MessageIntentAnswer, Content: "The signed evidence is attached.",
						Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToRecordID: "message-1", CreatedAt: start.Add(time.Minute),
					},
				},
			}

			partial := request
			partial.Messages = partial.Messages[:1]
			first, err := service.ImportArchive(ctx, partial)
			if err != nil || first.ImportedMessages != 1 || first.ReplayedMessages != 0 || first.Replayed ||
				!first.Conversation.CreatedAt.Equal(start) {
				t.Fatalf("partial import = %#v, %v", first, err)
			}
			resumed, err := service.ImportArchive(ctx, request)
			if err != nil || resumed.ImportedMessages != 1 || resumed.ReplayedMessages != 1 || resumed.Replayed ||
				len(resumed.Messages) != 2 || resumed.Conversation.LastSequence != 2 {
				t.Fatalf("resumed import = %#v, %v", resumed, err)
			}
			answer := resumed.Messages[1]
			if !answer.Historical || !answer.CreatedAt.Equal(start.Add(time.Minute)) || answer.ReplyToMessageID != resumed.Messages[0].ID ||
				answer.ThreadRootID != resumed.Messages[0].ID || len(answer.References) != 1 ||
				answer.References[0].Kind != ConversationReferenceExternalSource ||
				answer.References[0].ID != "cortex:team-chat:conversation-42:message-2" {
				t.Fatalf("imported answer = %#v", answer)
			}

			replayed, err := service.ImportArchive(ctx, request)
			if err != nil || !replayed.Replayed || replayed.ImportedMessages != 0 || replayed.ReplayedMessages != 2 ||
				len(replayed.Messages) != 2 {
				t.Fatalf("replayed import = %#v, %v", replayed, err)
			}
			scheduler, err := NewConversationRunScheduler(store.(ConversationStore), store, ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			reconciled, err := scheduler.ReconcileScope(ctx, scope)
			if err != nil || reconciled.Scheduled != 0 || reconciled.Skipped != 2 {
				t.Fatalf("historical reconciliation = %#v, %v", reconciled, err)
			}
			runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
			if err != nil || len(runs) != 0 {
				t.Fatalf("historical Runs = %#v, %v", runs, err)
			}
		})
	}
}

func TestConversationArchiveImportRejectsUnknownReplyAndDuplicateRecords(t *testing.T) {
	service := NewConversationService(NewMemoryStore(20))
	base := ImportConversationArchiveRequest{
		Scope: Scope{Kind: "tenant", ID: "archive"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Imported history", Source: ConversationArchiveSource{System: "source", ResourceType: "chat", ResourceID: "1"},
		IdempotencyKey: "archive-1", Messages: []ConversationArchiveMessage{{
			RecordID: "message-1", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "23"},
			Intent: MessageIntentUpdate, Content: "Status", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
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
