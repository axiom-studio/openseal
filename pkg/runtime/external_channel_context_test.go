package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestInstallationWideInboxKeepsChannelAndDMContextsSeparate(t *testing.T) {
	ctx := t.Context()
	store, _, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	endpoint.InstallationWide = true
	worker := &ExternalConversationInboxWorker{store: store, conversations: NewConversationService(store), now: time.Now}
	seen := make(map[string]string)
	for _, channel := range []string{"C-public", "C-private", "D-alice", "D-bob"} {
		conversation, _, err := worker.ensureConversation(ctx, endpoint, NormalizedExternalConversationEvent{ExternalConversationID: channel})
		if err != nil {
			t.Fatal(err)
		}
		if previous := seen[conversation.ID]; previous != "" {
			t.Fatalf("%s inherited %s context", channel, previous)
		}
		seen[conversation.ID] = channel
		message := postConversationRunTestMessage(t, worker.conversations, conversation, ConversationParticipantUser, MessageIntentQuestion, channel+"_SECRET", channel+"-message")
		thread, _, err := worker.ensureConversation(ctx, endpoint, NormalizedExternalConversationEvent{ExternalConversationID: channel, ExternalThreadID: "same-thread-id"})
		if err != nil || thread.ID != conversation.ID {
			t.Fatalf("thread escaped its channel context: %#v, %v", thread, err)
		}
		viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID}}
		for otherID := range seen {
			if otherID == conversation.ID {
				continue
			}
			if _, err := ReadConversationHistory(ctx, worker.conversations, endpoint.Scope, endpoint.Owner, otherID, viewer, ConversationHistoryReadRequest{MessageID: message.ID}); err == nil {
				t.Fatal("history reader exposed a message from another channel")
			}
		}
	}
}

func TestExternalChannelModelContextExcludesAgentPrivateWork(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "channel-isolation"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "nori"}
	service := NewConversationService(store)
	private, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Private DM", IdempotencyKey: "private-dm", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "same-slack-app", Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	postConversationRunTestMessage(t, service, private, ConversationParticipantUser, MessageIntentQuestion, "PRIVATE_DM_SECRET", "private-message")
	public, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Public channel", IdempotencyKey: "public-channel", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "same-slack-app", Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, public, ConversationParticipantUser, MessageIntentQuestion, "PUBLIC_CHANNEL_QUESTION", "public-message")
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "PRIVATE_OBJECTIVE", Goal: "PRIVATE_OBJECTIVE_SECRET", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, DefinitionID: "private-workflow", DefinitionVersion: "1", TriggerID: "scheduled", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "private-workflow", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}}, Status: RunbookActivationActive, IdempotencyKey: "private-workflow"})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: public.ID})
	if err != nil {
		t.Fatal(err)
	}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: owner.ID}}
	foreignSummary := &conversationSummary{Scope: scope, ConversationID: private.ID, ViewerKey: conversationViewerKey(viewer), ThroughSequence: 1, Basis: strings.Repeat("a", 64), Text: "PRIVATE_DM_SUMMARY"}
	if plan := planConversationHistory(public, trigger.ID, viewer, recent, foreignSummary); plan.Summary != nil {
		t.Fatal("channel accepted another DM's summary")
	}
	goal, err := runner.agentConversationGoal(ctx, public, trigger, recent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, "PUBLIC_CHANNEL_QUESTION") {
		t.Fatal("current channel context missing")
	}
	for _, secret := range []string{"PRIVATE_DM_SECRET", "PRIVATE_OBJECTIVE_SECRET", "private-workflow"} {
		if strings.Contains(goal, secret) {
			t.Fatalf("external channel exposed %s", secret)
		}
	}
	personal := *public
	personal.Origin = nil
	controlGoal, err := runner.agentConversationGoal(ctx, &personal, trigger, recent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(controlGoal, "PRIVATE_OBJECTIVE_SECRET") {
		t.Fatal("personal control conversation lost its existing private portfolio context")
	}
}
