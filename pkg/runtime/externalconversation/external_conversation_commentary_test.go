package externalconversation

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestExternalCommentaryUsesSharedMessagesWithoutAnotherModelCall(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			fixture := newRunProgressWorkerFixture(t, provider, []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator})
			worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, agentrun.RunProgressAcknowledgementRequest) (string, error) {
				t.Fatal("public commentary triggered a second model call")
				return "", nil
			}), RunProgressAcknowledgementWorkerConfig{PageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			if updates, err := worker.ProcessCommentaryScope(t.Context(), fixture.endpoint.Scope); err != nil || len(updates) != 1 || updates[0].Operation != capability.ConversationDeliveryTypingIndicator {
				t.Fatalf("initial working state missing: %#v %v", updates, err)
			}
			activity := runtimeactivity.NewRunActivityService(fixture.store, fixture.store)
			for i, eventType := range []string{"turn.commentary", "turn.progress", "turn.commentary", "turn.commentary"} {
				visibility := models.ActivityVisibilityScope
				if i == 2 {
					visibility = models.ActivityVisibilityPrivate
				}
				_, err := activity.AppendActivity(t.Context(), &models.ActivityEvent{Scope: fixture.endpoint.Scope, RunID: fixture.run.ID, EventType: eventType, Summary: []string{"I’m checking the pull request.", "internal tool loaded", "PRIVATE_SECRET", "Found the issue—fixing it now."}[i], Actor: models.ActivityActor{Type: "worker", ID: "test"}, Visibility: visibility})
				if err != nil {
					t.Fatal(err)
				}
			}
			if updates, err := worker.ProcessCommentaryScope(t.Context(), fixture.endpoint.Scope); err != nil || len(updates) != 3 {
				t.Fatalf("updates withheld until completion: %#v %v", updates, err)
			}
			// Even a quick completed run must keep its public updates before the answer.
			if _, _, err := activity.TransitionRun(t.Context(), fixture.endpoint.Scope, fixture.run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: fixture.run.Revision, Status: models.AgentRunStatusCompleted, Summary: "Done", EventType: "run.completed", Actor: models.ActivityActor{Type: "worker", ID: "test"}}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				deliveries, err := worker.ProcessCommentaryScope(t.Context(), fixture.endpoint.Scope)
				if err != nil || len(deliveries) != 2 {
					t.Fatalf("commentary delivery=%#v err=%v", deliveries, err)
				}
				for _, delivery := range deliveries {
					if delivery.Operation != capability.ConversationDeliveryMessageSend || delivery.ExternalThreadID != "thread" || delivery.ExternalConversationID != "conversation" {
						t.Fatalf("wrong operation or destination: %#v", delivery)
					}
				}
			}
			outbox, err := fixture.store.ListExternalConversationDeliveries(t.Context(), models.ExternalConversationDeliveryFilter{Scope: fixture.endpoint.Scope, EndpointID: fixture.endpoint.ID, Limit: 100})
			if err != nil || len(outbox) != 4 {
				t.Fatalf("reconciliation duplicated updates: %#v %v", outbox, err)
			}
			messages, err := fixture.store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 100})
			if err != nil || len(messages) != 3 || messages[1].Content != "I’m checking the pull request." || messages[2].Content != "Found the issue—fixing it now." {
				t.Fatalf("public wording/order changed or private activity leaked: %#v %v", messages, err)
			}
		})
	}
}

func TestExternalThreadStatusKeepsQueuedFollowupWorking(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "webchat", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator})
	_, err := agentrun.NewRunCommandService(fixture.store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: fixture.run.Scope, Kind: models.RunKindConversation, Owner: fixture.run.Owner, AssignedAgentID: fixture.run.AssignedAgentID,
		Goal: "Continue in the same thread", Source: models.RunSourceChat, ConcurrencyKey: "thread-a", IdempotencyKey: "thread-followup", Actor: models.ActivityActor{Type: "service", ID: "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := *fixture.run
	completed.ConcurrencyKey = "thread-a"
	busy, err := externalThreadHasActiveWork(t.Context(), fixture.store, &completed)
	if err != nil || !busy {
		t.Fatalf("queued followup lost working indicator: %v %v", busy, err)
	}
	completed.ConcurrencyKey = "thread-b"
	busy, err = externalThreadHasActiveWork(t.Context(), fixture.store, &completed)
	if err != nil || busy {
		t.Fatalf("another thread prevented clearing: %v %v", busy, err)
	}
}
