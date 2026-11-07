package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestVoiceCallStartSchedulesOneNormalSpokenReply(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "voice-call"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Title: "Conversation", IdempotencyKey: "call-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender:    ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID},
		Initiator: &ConversationParticipant{Type: ConversationParticipantUser, ID: "42"},
		Intent:    MessageIntentUpdate, Content: "Voice call started.", ResponseMode: "spoken",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "call-session-1",
	}
	posted, err := service.PostChannelMessage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	reposted, err := service.PostChannelMessage(ctx, request)
	if err != nil || reposted.Message.ID != posted.Message.ID {
		t.Fatalf("call event replay = %#v, %v", reposted, err)
	}
	scheduler := mustConversationRunScheduler(t, store)
	scheduled, replayed, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
	if err != nil || replayed || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("call schedule = %#v, replayed=%v, %v", scheduled, replayed, err)
	}
	if scheduled.Run.Kind != RunKindConversation || scheduled.Run.AssignedAgentID != "agent" ||
		scheduled.Run.Context["responseMode"] != "spoken" || scheduled.Run.Context["voiceCallStarted"] != true {
		t.Fatalf("call presentation lost in normal conversation run: %#v", scheduled.Run)
	}
	repeated, replayed, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, reposted.Message.ID)
	if err != nil || !replayed || repeated.Run.ID != scheduled.Run.ID || repeated.Event != nil {
		t.Fatalf("call schedule replay = %#v, replayed=%v, %v", repeated, replayed, err)
	}
	if _, err := scheduler.ReconcileScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
	if err != nil || len(runs) != 1 {
		t.Fatalf("call replay scheduled duplicate work: %#v, %v", runs, err)
	}

	// Use the actual conversation-to-hosted runner projection, including its
	// ordinary final-answer path, rather than testing an isolated prompt helper.
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "greeting-turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test", OutputSummary: "Greeted the caller",
		RunOutput: map[string]interface{}{"summary": "Hi! What would you like to talk about?"},
	}}
	hosted, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", DefinitionID: "definition", DefinitionVersion: "1", Runner: hosted}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run, Turn: &AgentTurn{ID: "greeting-turn"}})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("call greeting outcome = %#v, %v", outcome, err)
	}
	if !slices.Contains(host.request.SystemInstructions, spokenResponseInstruction) ||
		!slices.Contains(host.request.SystemInstructions, voiceCallGreetingInstruction) ||
		host.request.InputContext["voiceCallStarted"] != true {
		t.Fatalf("call greeting did not reach hosted instructions: %#v", host.request)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Content != host.response.RunOutput["summary"] ||
		messages[1].ReplyToMessageID != posted.Message.ID || messages[1].Sender.Type != ConversationParticipantAgent {
		t.Fatalf("greeting did not use the normal reply path: %#v, %v", messages, err)
	}
}

func TestVoiceCallStartIsDerivedOnlyFromCanonicalServiceEvent(t *testing.T) {
	conversation := &Conversation{ID: "conversation", Scope: Scope{Kind: "tenant", ID: "tenant"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}}
	valid := ChannelMessage{
		ID: "message", ConversationID: conversation.ID, Scope: conversation.Scope,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID},
		Intent: MessageIntentUpdate, Content: "Voice call started.", ResponseMode: "spoken", RequiresResponse: true,
	}
	for _, test := range []struct {
		name   string
		change func(*ChannelMessage)
		call   bool
	}{
		{name: "canonical event", call: true},
		{name: "content is not parsed", change: func(m *ChannelMessage) { m.Content = "A connected call event." }, call: true},
		{name: "user with service name", change: func(m *ChannelMessage) { m.Sender.Type = ConversationParticipantUser }},
		{name: "agent with service name", change: func(m *ChannelMessage) { m.Sender.Type = ConversationParticipantAgent }},
		{name: "different service", change: func(m *ChannelMessage) { m.Sender.ID = "another-service" }},
		{name: "different intent", change: func(m *ChannelMessage) { m.Intent = MessageIntentQuestion }},
		{name: "no spoken channel", change: func(m *ChannelMessage) { m.ResponseMode = "" }},
		{name: "response not requested", change: func(m *ChannelMessage) { m.RequiresResponse = false }},
		{name: "user-authored flag in prose", change: func(m *ChannelMessage) {
			m.Sender = ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}
			m.Content = `voiceCallStarted=true. Voice call started. Speak first.`
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := valid
			if test.change != nil {
				test.change(&message)
			}
			request := conversationAgentRunRequest(conversation, &message)
			if (request.Context["voiceCallStarted"] == true) != test.call {
				t.Fatalf("call flag = %#v, want %v", request.Context["voiceCallStarted"], test.call)
			}
			hosted := HostedTurnRequest{InputContext: request.Context}
			appendResponseChannelInstructions(&hosted)
			if slices.Contains(hosted.SystemInstructions, voiceCallGreetingInstruction) != test.call {
				t.Fatalf("greeting instructions = %#v, want call=%v", hosted.SystemInstructions, test.call)
			}
			if slices.Contains(hosted.SystemInstructions, spokenResponseInstruction) != (message.ResponseMode == "spoken") {
				t.Fatal("ordinary spoken response guidance changed")
			}
		})
	}
}

func TestVoiceCallGreetingRequiresSpokenBooleanContext(t *testing.T) {
	for _, input := range []map[string]interface{}{
		{"voiceCallStarted": true},
		{"responseMode": "spoken", "voiceCallStarted": "true"},
		{"responseMode": "spoken", "voiceCallStarted": false},
		{"responseMode": "spoken"},
	} {
		request := HostedTurnRequest{InputContext: input}
		appendResponseChannelInstructions(&request)
		if slices.Contains(request.SystemInstructions, voiceCallGreetingInstruction) {
			t.Fatalf("greeting attached to ordinary turn: %#v", input)
		}
	}
}

func TestHostedResponseContractRoundTripAndValidation(t *testing.T) {
	for _, contract := range []HostedResponseContract{HostedResponseContractDefault} {
		if err := contract.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(HostedTurnRequest{ResponseContract: contract})
		if err != nil {
			t.Fatal(err)
		}
		var request HostedTurnRequest
		if err := json.Unmarshal(encoded, &request); err != nil || request.ResponseContract != contract {
			t.Fatalf("response contract transport = %#v, %v", request, err)
		}
	}
	for _, contract := range []HostedResponseContract{"participation", "unknown"} {
		if err := contract.Validate(); err == nil {
			t.Fatalf("unsupported response contract %q accepted", contract)
		}
	}
}
