package runtime

import (
	"context"
	"testing"
)

func TestLocalConversationThreadScopesSupersessionAndPreservesIndependentWork(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "local-threads"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "local-threads",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(key, root string, sender ConversationParticipantType) *ChannelMessage {
		t.Helper()
		latest, err := service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		intent := MessageIntentQuestion
		if sender == ConversationParticipantService {
			intent = MessageIntentSystem
		}
		result, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
			Sender: ConversationParticipant{Type: sender, ID: "user"}, Intent: intent, Content: key,
			RequiresResponse: sender == ConversationParticipantUser,
			Audience:         ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: root, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Message
	}
	rootOne := post("call-one", "", ConversationParticipantService)
	rootTwo := post("call-two", "", ConversationParticipantService)
	scheduler := mustConversationRunScheduler(t, store)
	schedule := func(message *ChannelMessage) *AgentRun {
		t.Helper()
		result, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
		if err != nil || result == nil || result.Run == nil {
			t.Fatalf("schedule message: %#v %v", result, err)
		}
		return result.Run
	}
	ordinary := schedule(post("ordinary request", "", ConversationParticipantUser))
	one := schedule(post("first call request", rootOne.ID, ConversationParticipantUser))
	two := schedule(post("other call request", rootTwo.ID, ConversationParticipantUser))
	independent, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Kind: RunKindAgentWork, Owner: conversation.Owner, AssignedAgentID: "agent", Goal: "Continue independent work",
		Source: RunSourceChat, ConcurrencyKey: "task:independent", Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, "threadRootMessageId": rootOne.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	newOne := schedule(post("interrupt first call reply", rootOne.ID, ConversationParticipantUser))
	for _, expected := range []struct {
		run    *AgentRun
		status AgentRunStatus
	}{
		{ordinary, AgentRunStatusQueued}, {one, AgentRunStatusCanceled}, {two, AgentRunStatusQueued},
		{newOne, AgentRunStatusQueued}, {independent, AgentRunStatusQueued},
	} {
		current, err := store.GetAgentRun(t.Context(), scope, expected.run.ID)
		if err != nil || current.Status != expected.status {
			t.Fatalf("wrong thread was interrupted: %#v expected=%s err=%v", current, expected.status, err)
		}
	}
	if one.Context["threadRootMessageId"] != rootOne.ID || one.ConcurrencyKey != conversation.ID+":thread:"+rootOne.ID || ordinary.ConcurrencyKey != conversation.ID {
		t.Fatal("local roots did not become canonical foreground execution context")
	}
	schedule(post("replace ordinary request", "", ConversationParticipantUser))
	current, err := store.GetAgentRun(t.Context(), scope, ordinary.ID)
	if err != nil || current.Status != AgentRunStatusCanceled {
		t.Fatal("ordinary unthreaded prompts stopped superseding their conversation lane")
	}
	current, err = store.GetAgentRun(t.Context(), scope, newOne.ID)
	if err != nil || current.Status != AgentRunStatusQueued {
		t.Fatal("ordinary unthreaded prompt canceled another call's reply")
	}
}

func TestLocalConversationThreadUsesCanonicalTriggerBeforeHostedModel(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "local-thread-authority"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "thread-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	root := postConversationRunTestMessage(t, service, conversation, ConversationParticipantService, MessageIntentSystem, "Call root", "call-root")
	latest, err := service.GetConversation(t.Context(), scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion,
		Content: "Review the release", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: root.ID, IdempotencyKey: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, posted.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{
		AgentTurns: TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
			return &TurnRunnerBinding{DeploymentID: "agent", Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				modelCalls++
				if input.ForegroundConversation == nil || input.ForegroundConversation.Context["threadRootMessageId"] != root.ID {
					t.Fatal("canonical local root was lost before hosted reasoning")
				}
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, nil
			})}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run, Turn: &AgentTurn{ID: "turn"}}); err != nil || modelCalls != 1 {
		t.Fatalf("canonical rooted foreground could not reason: calls=%d err=%v", modelCalls, err)
	}
	forged := cloneAgentRun(scheduled.Run)
	forged.Context["threadRootMessageId"] = "forged-root"
	forged.ConcurrencyKey = conversation.ID + ":thread:forged-root"
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: forged, Turn: &AgentTurn{ID: "other-turn"}}); err == nil || modelCalls != 1 {
		t.Fatal("mutable run metadata overrode the canonical local message root")
	}
}
