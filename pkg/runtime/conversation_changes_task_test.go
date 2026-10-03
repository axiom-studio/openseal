package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func conversationTaskChangeStore(t *testing.T, backend string) KernelStore {
	t.Helper()
	if backend == "memory" {
		return NewMemoryStore()
	}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "task-changes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func settleConversationTaskForChanges(t *testing.T, store KernelStore, conversation *Conversation, message *ChannelMessage) (*AgentRun, *AgentTurn) {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(store.(ConversationStore), store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule task source: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: conversation.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim task source: %#v %v", claimed, err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: conversation.Scope, RunID: claimed.ID, WorkerID: "worker", DefinitionID: "agent", DefinitionVersion: "1",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: taskProposalFixture()}, nil
	}))
	if err != nil || result == nil || result.Turn == nil || result.Turn.RequestedTask == nil {
		t.Fatalf("settle task proposal: %#v %v", result, err)
	}
	return result.Run, result.Turn
}

func changeRunByID(runs []*AgentRun, id string) *AgentRun {
	for _, run := range runs {
		if run.ID == id {
			return run
		}
	}
	return nil
}

func TestConversationChangesProjectIndependentTaskBeforeAndWithTerminalReport(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := conversationTaskChangeStore(t, backend)
			conversations := NewConversationService(store.(ConversationStore))
			scope := Scope{Kind: "tenant", ID: "task-stream"}
			conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Task stream", IdempotencyKey: "task-stream",
			})
			if err != nil {
				t.Fatal(err)
			}
			message := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion, "Review the release in the background", "task-question")
			source, turn := settleConversationTaskForChanges(t, store, conversation, message)
			changes, err := NewConversationChangeService(store.(ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			viewer := &ConversationViewer{Participant: message.Sender}
			initial, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: viewer})
			if err != nil || changeRunByID(initial.Runs, source.ID) == nil {
				t.Fatalf("foreground projection missing: %#v %v", initial, err)
			}
			admitted, err := NewConversationTaskService(store.(ConversationTaskKernelStore)).Start(t.Context(), taskStartRequest(source, turn))
			if err != nil {
				t.Fatal(err)
			}
			// Task admission has committed but the terminal reporting outbox has not
			// posted either acknowledgment or final result yet.
			started, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, Viewer: viewer})
			if err != nil || !started.RunsChanged || len(started.Messages) > 1 || len(started.Messages) == 1 && started.Messages[0].Content != admitted.Task.Acknowledgment {
				t.Fatalf("admission projection: %#v %v", started, err)
			}
			work := changeRunByID(started.Runs, admitted.WorkRun.ID)
			proof := changeRunByID(started.Runs, source.ID)
			if work == nil || work.Status != AgentRunStatusQueued || work.ParentRunID != "" || work.RootRunID != work.ID ||
				proof == nil || proof.Status != AgentRunStatusCompleted || proof.Output["conversationTaskWorkRunId"] != work.ID {
				t.Fatalf("task/source proof missing: %#v", started.Runs)
			}
			other := &ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "another-user"}}
			foreign, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: other})
			if err != nil || changeRunByID(foreign.Runs, work.ID) != nil {
				t.Fatalf("task crossed authenticated actor: %#v %v", foreign, err)
			}
			running, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), scope, work.ID, RunTransitionRequest{ExpectedRevision: work.Revision, Status: AgentRunStatusRunning})
			if err != nil {
				t.Fatal(err)
			}
			terminal, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), scope, work.ID, RunTransitionRequest{
				ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "The release review found two fixes."},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := projectTerminalRunReporting(t.Context(), store.(ConversationStore), terminal); err != nil {
				t.Fatal(err)
			}
			finished, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: started.Cursor, Viewer: viewer})
			if err != nil || !finished.RunsChanged || len(finished.Messages) < 1 || len(finished.Messages) > 2 ||
				changeRunByID(finished.Runs, work.ID) == nil || changeRunByID(finished.Runs, work.ID).Status != AgentRunStatusCompleted || changeRunByID(finished.Runs, source.ID) == nil {
				t.Fatalf("terminal report lacked current work proof: %#v %v", finished, err)
			}
			acknowledgments, finalReports := 0, 0
			for _, reported := range append(started.Messages, finished.Messages...) {
				switch reported.Content {
				case admitted.Task.Acknowledgment:
					acknowledgments++
				case terminal.Output["summary"]:
					finalReports++
				default:
					t.Fatalf("unexpected task report: %#v", reported)
				}
			}
			if acknowledgments != 1 || finalReports != 1 {
				t.Fatalf("acknowledgment/final report did not arrive once: ack=%d final=%d", acknowledgments, finalReports)
			}
			stable, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: finished.Cursor, Viewer: viewer})
			if err != nil || stable.HasChanges || stable.Cursor != finished.Cursor {
				t.Fatalf("unchanged task projection resent: %#v %v", stable, err)
			}
			forged := cloneAgentRun(terminal)
			forged.ID, forged.RootRunID, forged.ParentRunID = "context-only-work", "context-only-work", ""
			forged.Status, forged.Revision, forged.Output = AgentRunStatusRunning, 1, nil
			forged.ConcurrencyKey = "task:invented"
			if err := store.CreateAgentRun(t.Context(), forged); err != nil {
				t.Fatal(err)
			}
			afterForgery, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: finished.Cursor, Viewer: viewer})
			if err != nil || afterForgery.HasChanges || changeRunByID(afterForgery.Runs, forged.ID) != nil {
				t.Fatalf("copied task context became stream authority: %#v %v", afterForgery, err)
			}
		})
	}
}

func TestConversationTaskChangesInheritSourceThreadVisibility(t *testing.T) {
	store := NewMemoryStore()
	conversations := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "task-thread-privacy"}
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Private task", IdempotencyKey: "private-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "private-root"}, Intent: MessageIntentSystem, Content: "Private thread",
		Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "agent"}}}, IdempotencyKey: "private-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	question, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: root.Conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion, Content: "Review the release",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: root.Message.ID, RequiresResponse: true, IdempotencyKey: "private-question",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, turn := settleConversationTaskForChanges(t, store, conversation, question.Message)
	admitted, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn))
	if err != nil {
		t.Fatal(err)
	}
	viewer := &ConversationViewer{Participant: question.Message.Sender}
	if !CanViewChannelMessage(question.Message, *viewer) {
		t.Fatal("fixture source itself should be visible to its sender")
	}
	changes, err := NewConversationChangeService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: viewer})
	if err != nil || len(initial.Runs) != 0 || len(initial.Activity) != 0 || len(initial.Messages) != 0 {
		t.Fatalf("source thread privacy did not protect detached work: %#v %v", initial, err)
	}
	if _, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), scope, admitted.WorkRun.ID, RunTransitionRequest{
		ExpectedRevision: admitted.WorkRun.Revision, Status: AgentRunStatusRunning, Summary: "Private task started",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, Viewer: viewer})
	if err != nil || after.HasChanges || after.Cursor != initial.Cursor || len(after.Runs) != 0 || len(after.Activity) != 0 {
		t.Fatalf("hidden task state leaked via stream digest: %#v %v", after, err)
	}
}

func TestConversationTaskTerminalWindowCountsVisibleRuns(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, message := settledWorkerTaskFixture(t, store)
	admitted, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn))
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), source.Scope, admitted.WorkRun.ID, RunTransitionRequest{ExpectedRevision: admitted.WorkRun.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), source.Scope, running.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	conversations := NewConversationService(store)
	latest, err := conversations.GetConversation(t.Context(), source.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: source.Scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "private"}, Intent: MessageIntentSystem, Content: "Private work",
		Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "agent"}}}, IdempotencyKey: "hidden-window",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	for i := 0; i <= conversationRunProjectionLimit; i++ {
		id := fmt.Sprintf("private-root-%03d", i)
		status := AgentRunStatusCompleted
		if i == 0 {
			status = AgentRunStatusRunning
		}
		if err := store.CreateAgentRun(t.Context(), &AgentRun{
			ID: id, Scope: source.Scope, Owner: conversation.Owner, Kind: RunKindConversation, AssignedAgentID: "agent", Goal: "Private work", Source: RunSourceChat,
			RootRunID: id, ConcurrencyKey: conversation.ID, Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now,
			Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: hidden.Message.ID},
		}); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := NewConversationChangeService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &ConversationViewer{Participant: message.Sender}
	initial, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: source.Scope, ConversationID: conversation.ID, Viewer: viewer})
	if err != nil || changeRunByID(initial.Runs, admitted.WorkRun.ID) == nil || len(initial.Runs) != 2 {
		t.Fatalf("hidden terminal history displaced visible task: %#v %v", initial, err)
	}
	if _, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), source.Scope, "private-root-000", RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	after, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: source.Scope, ConversationID: conversation.ID, Cursor: initial.Cursor, Viewer: viewer})
	if err != nil || after.HasChanges || after.Cursor != initial.Cursor {
		t.Fatalf("hidden terminal transition changed visible task projection: %#v %v", after, err)
	}
}

func TestConversationChangesIncludeCanonicalLocalRootedForeground(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := conversationTaskChangeStore(t, backend)
			conversations := NewConversationService(store.(ConversationStore))
			scope := Scope{Kind: "tenant", ID: "rooted-stream"}
			conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Rooted stream", IdempotencyKey: "rooted-stream",
			})
			if err != nil {
				t.Fatal(err)
			}
			root := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantService, MessageIntentSystem, "Call root", "call-root")
			latest, err := conversations.GetConversation(t.Context(), scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			question, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion, Content: "Review the release",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: root.ID, RequiresResponse: true, IdempotencyKey: "rooted-question",
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewConversationRunScheduler(store.(ConversationStore), store, ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, question.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			forged := cloneAgentRun(scheduled.Run)
			forged.ID, forged.RootRunID = "forged-rooted-run", "forged-rooted-run"
			forged.Context["threadRootMessageId"] = "forged-thread"
			forged.ConcurrencyKey = conversation.ID + ":thread:forged-thread"
			if err := store.CreateAgentRun(t.Context(), forged); err != nil {
				t.Fatal(err)
			}
			changes, err := NewConversationChangeService(store.(ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: &ConversationViewer{Participant: question.Message.Sender}})
			if err != nil || changeRunByID(projected.Runs, scheduled.Run.ID) == nil || changeRunByID(projected.Runs, forged.ID) != nil {
				t.Fatalf("canonical rooted foreground projection: %#v %v", projected, err)
			}
		})
	}
}
