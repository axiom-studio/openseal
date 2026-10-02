package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestExternalCommentaryUsesSharedMessagesWithoutAnotherModelCall(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			fixture := newRunProgressWorkerFixture(t, provider, []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator})
			worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
				t.Fatal("public commentary triggered a second model call")
				return "", nil
			}), RunProgressAcknowledgementWorkerConfig{PageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			if updates, err := worker.ProcessCommentaryScope(t.Context(), fixture.endpoint.Scope); err != nil || len(updates) != 0 {
				t.Fatalf("empty run synthesized an update: %#v %v", updates, err)
			}
			activity := NewRunActivityService(fixture.store, fixture.store)
			for i, eventType := range []string{"turn.commentary", "turn.progress", "turn.commentary", "turn.commentary"} {
				visibility := ActivityVisibilityScope
				if i == 2 {
					visibility = ActivityVisibilityPrivate
				}
				_, err := activity.AppendActivity(t.Context(), &ActivityEvent{Scope: fixture.endpoint.Scope, RunID: fixture.run.ID, EventType: eventType, Summary: []string{"I’m checking the pull request.", "internal tool loaded", "PRIVATE_SECRET", "Found the issue—fixing it now."}[i], Actor: ActivityActor{Type: "worker", ID: "test"}, Visibility: visibility})
				if err != nil {
					t.Fatal(err)
				}
			}
			if updates, err := worker.ProcessCommentaryScope(t.Context(), fixture.endpoint.Scope); err != nil || len(updates) != 2 {
				t.Fatalf("updates withheld until completion: %#v %v", updates, err)
			}
			// Even a quick completed run must keep its public updates before the answer.
			if _, _, err := activity.TransitionRun(t.Context(), fixture.endpoint.Scope, fixture.run.ID, RunTransitionRequest{ExpectedRevision: fixture.run.Revision, Status: AgentRunStatusCompleted, Summary: "Done", EventType: "run.completed", Actor: ActivityActor{Type: "worker", ID: "test"}}); err != nil {
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
			outbox, err := fixture.store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: fixture.endpoint.Scope, EndpointID: fixture.endpoint.ID, Limit: 100})
			if err != nil || len(outbox) != 2 {
				t.Fatalf("reconciliation duplicated updates: %#v %v", outbox, err)
			}
			messages, err := fixture.store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 100})
			if err != nil || len(messages) != 3 || messages[1].Content != "I’m checking the pull request." || messages[2].Content != "Found the issue—fixing it now." {
				t.Fatalf("public wording/order changed or private activity leaked: %#v %v", messages, err)
			}
		})
	}
}
