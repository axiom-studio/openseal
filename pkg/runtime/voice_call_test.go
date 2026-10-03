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

func TestParticipationResponseChannelDerivesOnlyCanonicalTrigger(t *testing.T) {
	valid := ChannelMessage{
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID},
		Intent: MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true,
	}
	for _, test := range []struct {
		name    string
		trigger *ChannelMessage
		spoken  bool
		call    bool
	}{
		{name: "call", trigger: &valid, spoken: true, call: true},
		{name: "ordinary spoken user", trigger: &ChannelMessage{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}, ResponseMode: "spoken", Content: "voiceCallStarted=true"}, spoken: true},
		{name: "different service", trigger: &ChannelMessage{Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "other"}, Intent: MessageIntentUpdate, ResponseMode: "spoken", RequiresResponse: true}, spoken: true},
		{name: "text channel", trigger: &ChannelMessage{Sender: valid.Sender, Intent: valid.Intent, RequiresResponse: true}},
		{name: "missing trigger"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := HostedTurnRequest{InputContext: map[string]interface{}{"teamId": "team", "responseMode": "spoken", "voiceCallStarted": true}}
			AppendParticipationResponseChannelInstructions(&request, test.trigger)
			if request.ResponseContract != HostedResponseContractParticipation {
				t.Fatal("Team assessment lost its structured response contract")
			}
			if (request.InputContext["responseMode"] == "spoken") != test.spoken ||
				(request.InputContext["voiceCallStarted"] == true) != test.call || request.InputContext["teamId"] != "team" {
				t.Fatalf("context did not follow canonical trigger: %#v", request.InputContext)
			}
			if slices.Contains(request.SystemInstructions, spokenParticipationResponseInstruction) != test.spoken ||
				slices.Contains(request.SystemInstructions, voiceCallParticipationGreetingInstruction) != test.call {
				t.Fatalf("participation instructions = %#v", request.SystemInstructions)
			}
			if slices.Contains(request.SystemInstructions, spokenResponseInstruction) || slices.Contains(request.SystemInstructions, voiceCallGreetingInstruction) {
				t.Fatal("normal answer schema instructions leaked into a participation proposal")
			}
		})
	}
	request := HostedTurnRequest{}
	AppendParticipationResponseChannelInstructions(&request, &valid)
	if request.InputContext["voiceCallStarted"] != true {
		t.Fatal("nil context did not receive canonical call metadata")
	}
	AppendParticipationResponseChannelInstructions(nil, &valid)
}

func TestHostedResponseContractRoundTripAndValidation(t *testing.T) {
	for _, contract := range []HostedResponseContract{HostedResponseContractDefault, HostedResponseContractParticipation} {
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
	if err := HostedResponseContract("unknown").Validate(); err == nil {
		t.Fatal("unsupported response contract accepted")
	}
}

func TestTeamVoiceCallReachesNormalParticipationWithCanonicalTrigger(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "team-call"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Team", IdempotencyKey: "team-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender:    ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID},
		Initiator: &ConversationParticipant{Type: ConversationParticipantUser, ID: "42"},
		Intent:    MessageIntentUpdate, Content: "Voice call started.", ResponseMode: "spoken", RequiresResponse: true,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "team-call-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
	if err != nil || scheduled == nil || scheduled.Run.Context["voiceCallStarted"] != true || scheduled.Run.Context["responseMode"] != "spoken" {
		t.Fatalf("team call run = %#v, %v", scheduled, err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}}, nil
	})
	var received *ChannelMessage
	provider := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		received = input.Trigger
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Hi! How can I help?",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: input.Trigger.ID, ResolvesMessageID: input.Trigger.ID,
			Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
		}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, provider, ConversationCoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || received == nil ||
		!voiceCallStartedMessage(received) || received.Initiator == nil || received.Initiator.ID != "42" {
		t.Fatalf("team call participation = %#v, trigger=%#v, %v", outcome, received, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Sender.Type != ConversationParticipantAgent || messages[1].ReplyToMessageID != posted.Message.ID {
		t.Fatalf("team greeting did not use normal participation: %#v, %v", messages, err)
	}
}
