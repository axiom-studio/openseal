package authoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

const slackChatbotPrompt = "I want a Slack agent that listens to my message in the channel that it is installed in and then responds back to them."

const orchestratedSlackChatbotPrompt = "Create a Slack chatbot that listens for messages, triages them, asks for approval when needed, and then responds."

type compositionCaptureGenerator struct {
	request GenerateRequest
	payload []byte
}

func (g *compositionCaptureGenerator) Generate(_ context.Context, request GenerateRequest) ([]byte, error) {
	g.request = request
	return g.payload, nil
}

func slackChatbotCandidate() WorkforceCandidate {
	agentID := "slack-response-agent"
	return WorkforceCandidate{
		Activation: WorkforceActivationActive,
		Agents: []*agent.AgentDefinition{{
			ID: agentID, Version: "1.0.0", DisplayName: "Slack response agent",
			Purpose:      "Respond helpfully to permitted Slack messages.",
			SystemPrompt: "Answer the triggering message helpfully and concisely.",
			Authority: agent.AuthorityPolicy{
				MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1,
			},
			Runbook: &runbook.Definition{
				APIVersion: runbook.APIVersion,
				ID:         "respond-to-message",
				Version:    "1.0.0",
				Name:       "Respond to message",
				Entrypoints: map[string]string{
					"respond": "reason",
				},
				Interfaces: map[string]runbook.Interface{
					"respond": {
						Description:  "Respond to one canonical conversation message.",
						InputSchema:  CanonicalConversationTriggerInputSchema(),
						OutputSchema: CanonicalConversationReplyOutputSchema(),
					},
				},
				Triggers: map[string]runbook.Trigger{
					"on-message": {
						Kind: runbook.TriggerEvent, EventType: capability.ConversationEventMessageReceived,
						Entrypoint: "respond",
					},
				},
				Steps: map[string]runbook.Step{
					"reason": {
						Kind: runbook.StepDelegate,
						Delegate: &runbook.DelegateStep{
							AgentID: literalRunbookValue(agentID),
							Goal:    literalRunbookValue("Respond helpfully to the triggering conversation message."),
							Context: map[string]runbook.Value{
								"conversationId":   {Ref: "/input/conversationId"},
								"triggerMessageId": {Ref: "/input/triggerMessageId"},
							},
							Mode: runbook.DelegateReason, ResultPath: "/results/response",
							Budget: &runbook.BudgetAllocation{MaxTurns: 4, MaxTotalTokens: 26000},
							Next:   "done",
						},
					},
					"done": {
						Kind: runbook.StepEnd,
						End: &runbook.EndStep{Outputs: map[string]runbook.Value{
							"reply": {Ref: "/results/response/summary"},
						}},
					},
				},
			},
		}},
		ConversationEndpoints: []ConversationEndpointBlueprint{{
			ID: "slack-channel", Name: "Installed Slack channel",
			Owner:   ConversationEndpointOwner{Type: ConversationEndpointOwnerAgent, ID: agentID},
			SkillID: "slack", SkillVersion: "1.0.0", AdapterID: "conversations",
			Mode: capability.ConversationEndpointChannel,
			Handler: ConversationHandlerBlueprint{
				Kind: ConversationHandlerRunbook, AgentDefinitionID: agentID,
				RunbookID: "respond-to-message", RunbookVersion: "1.0.0", Trigger: "on-message",
			},
			Policy: ConversationEndpointPolicyBlueprint{
				MessageSelection: ConversationSelectDirectOrMention,
				ReplyMode:        ConversationReplyThread,
				IgnoreBots:       true,
			},
			CanonicalReply:     true,
			ArchitectureReason: "A durable event Runbook invokes bounded Agent reasoning and replies through the Slack conversation adapter.",
		}},
	}
}

func slackChatbotCatalog() CapabilityCatalog {
	oauth2 := &capability.OAuth2Requirement{
		Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
		Scopes: []string{"channels:history", "chat:write"},
	}
	return CapabilityCatalog{
		RuntimeComposition: CanonicalRuntimeCompositionCapability(),
		Skills: map[string]SkillCapability{
			"slack": {
				ID: "slack", Version: "1.0.0", Name: "Slack", Readiness: SkillReadinessReady,
				ConversationAdapters: []ConversationAdapterCapability{{
					ID: "conversations", ProtocolVersion: capability.ConversationAdapterProtocolV1,
					Provider: "slack", EndpointModes: []capability.ConversationEndpointMode{
						capability.ConversationEndpointChannel,
					},
					InboundEventTypes: []string{capability.ConversationEventMessageReceived},
					Features: []capability.ConversationAdapterFeature{
						capability.ConversationFeatureMentions, capability.ConversationFeatureThreads,
					},
					Delivery: capability.ConversationDeliveryCapabilities{
						Operations: []capability.ConversationDeliveryOperation{
							capability.ConversationDeliveryMessageSend,
						},
						Ordering:                      capability.ConversationDeliveryOrderThread,
						Idempotency:                   capability.IdempotencyRequired,
						SupportsAcknowledgementLookup: true,
						SupportsRetryAfter:            true,
					},
					Credentials: []SkillCredential{{
						Name: "SLACK_CONNECTION", Kind: "slack-oauth", OAuth2: oauth2,
					}},
				}},
			},
		},
		AvailableCredentialGrants: map[string][]capability.OAuth2GrantSummary{
			"SLACK_CONNECTION": {{
				Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
				Scopes: []string{"channels:history", "chat:write"},
			}},
		},
	}
}

func directChatbotCandidate(skillID string, mode capability.ConversationEndpointMode, replyMode ConversationReplyMode) WorkforceCandidate {
	candidate := slackChatbotCandidate()
	candidate.Agents[0].Runbook = nil
	candidate.ConversationEndpoints[0].SkillID = skillID
	candidate.ConversationEndpoints[0].Mode = mode
	candidate.ConversationEndpoints[0].Handler = ConversationHandlerBlueprint{
		Kind:              ConversationHandlerAgent,
		AgentDefinitionID: candidate.Agents[0].ID,
	}
	candidate.ConversationEndpoints[0].Policy.ReplyMode = replyMode
	candidate.ConversationEndpoints[0].ArchitectureReason = "A direct Agent handler is the smallest executable pattern for one-turn replies."
	return candidate
}

func webChatbotCatalog() CapabilityCatalog {
	return CapabilityCatalog{
		RuntimeComposition: CanonicalRuntimeCompositionCapability(),
		Skills: map[string]SkillCapability{
			"webchat": {
				ID: "webchat", Version: "1.0.0", Name: "Web chat", Readiness: SkillReadinessReady,
				ConversationAdapters: []ConversationAdapterCapability{{
					ID: "conversations", ProtocolVersion: capability.ConversationAdapterProtocolV1,
					Provider: "webchat", EndpointModes: []capability.ConversationEndpointMode{
						capability.ConversationEndpointDirect,
					},
					InboundEventTypes: []string{capability.ConversationEventMessageReceived},
					Features:          []capability.ConversationAdapterFeature{capability.ConversationFeatureAttachments},
					Delivery: capability.ConversationDeliveryCapabilities{
						Operations:  []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend},
						Ordering:    capability.ConversationDeliveryOrderConversation,
						Idempotency: capability.IdempotencyRequired,
					},
				}},
			},
		},
	}
}

func literalRunbookValue(value string) runbook.Value {
	raw, _ := json.Marshal(value)
	return runbook.Value{Literal: raw}
}

func TestCompilerCreatesExecutableSlackChatbotComposition(t *testing.T) {
	candidate := directChatbotCandidate("slack", capability.ConversationEndpointChannel, ConversationReplyThread)
	catalog := slackChatbotCatalog()
	if err := ValidateCapabilityCatalog(catalog); err != nil {
		t.Fatalf("catalog = %v", err)
	}
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: slackChatbotPrompt, Catalog: catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.Validation) != 0 || len(result.MissingRequirements) != 0 {
		t.Fatalf("Slack chatbot compile = %#v", result)
	}
	if len(result.Candidate.ConversationEndpoints) != 1 ||
		result.Candidate.ConversationEndpoints[0].Handler.Kind != ConversationHandlerAgent {
		t.Fatalf("conversation endpoints = %#v", result.Candidate.ConversationEndpoints)
	}
	for _, requirement := range result.Candidate.Agents[0].SkillRequirements {
		for _, action := range requirement.RequiredActions {
			if action == "slack-send-message" {
				t.Fatal("ordinary conversation reply incorrectly requires slack-send-message")
			}
		}
	}
	required := RequiredCredentialBindings(result.Candidate, catalog)
	if len(required["slack-response-agent"]) != 1 ||
		required["slack-response-agent"][0].Key != "SLACK_CONNECTION" ||
		required["slack-response-agent"][0].OAuth2 == nil {
		t.Fatalf("Slack OAuth placement requirements = %#v", required)
	}
}

func TestCompilerDerivesConversationRequirementsBeforeProviderGeneration(t *testing.T) {
	candidate := directChatbotCandidate("slack", capability.ConversationEndpointChannel, ConversationReplyThread)
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	generator := &compositionCaptureGenerator{payload: payload}
	compiler, err := NewCompiler(generator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: slackChatbotPrompt, Catalog: slackChatbotCatalog(),
		CompositionRequirements: &RuntimeCompositionRequirements{Conversation: &ConversationCompositionRequirement{
			EventType: "caller.injected", HandlerKinds: []ConversationHandlerKind{ConversationHandlerRunbook},
			ArchitectureRule: ConversationArchitectureOrchestrated,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	requirement := generator.request.CompositionRequirements
	if requirement == nil || requirement.Conversation == nil ||
		requirement.Conversation.EventType != capability.ConversationEventMessageReceived ||
		!requirement.Conversation.CanonicalReply ||
		requirement.Conversation.ArchitectureRule != ConversationArchitectureDirect ||
		len(requirement.Conversation.HandlerKinds) != 2 ||
		requirement.Conversation.HandlerKinds[0] != ConversationHandlerAgent ||
		requirement.Conversation.HandlerKinds[1] != ConversationHandlerTeam {
		t.Fatalf("derived composition requirement = %#v", requirement)
	}
}

func TestReactiveConversationIntentRejectsProseOnlyAndWrongArchitecture(t *testing.T) {
	candidate := slackChatbotCandidate()
	candidate.ConversationEndpoints = nil
	issues := validateConversationComposition(&candidate, GenerateRequest{
		Prompt: slackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if !hasValidationCode(issues, "reactive_conversation_endpoint_missing") {
		t.Fatalf("prose-only issues = %#v", issues)
	}

	candidate = directChatbotCandidate("slack", capability.ConversationEndpointChannel, ConversationReplyThread)
	issues = validateConversationComposition(&candidate, GenerateRequest{
		Prompt: slackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if hasValidationCode(issues, "conversation_architecture_mismatch") {
		t.Fatalf("simple direct-handler issues = %#v", issues)
	}

	issues = validateConversationComposition(&candidate, GenerateRequest{
		Prompt: orchestratedSlackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if !hasValidationCode(issues, "conversation_architecture_mismatch") {
		t.Fatalf("orchestrated direct-handler issues = %#v", issues)
	}

	candidate = slackChatbotCandidate()
	issues = validateConversationComposition(&candidate, GenerateRequest{
		Prompt: orchestratedSlackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if hasValidationCode(issues, "conversation_architecture_mismatch") {
		t.Fatalf("orchestrated Runbook issues = %#v", issues)
	}

	candidate = slackChatbotCandidate()
	candidate.Agents[0].Runbook.Interfaces["respond"] = runbook.Interface{
		Description: "An incomplete trigger contract.",
		InputSchema: map[string]interface{}{"type": "object"},
	}
	issues = validateConversationComposition(&candidate, GenerateRequest{
		Prompt: slackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if !hasValidationCode(issues, "conversation_trigger_input_contract_mismatch") {
		t.Fatalf("trigger-contract issues = %#v", issues)
	}

	candidate = slackChatbotCandidate()
	delete(candidate.Agents[0].Runbook.Steps["done"].End.Outputs, "reply")
	issues = validateConversationEndpointBlueprints(&candidate)
	if !hasValidationCode(issues, "conversation_canonical_reply_missing") {
		t.Fatalf("canonical-reply issues = %#v", issues)
	}
}

func TestCompilerComposesEquivalentWebChatAdapterWithoutProviderLogic(t *testing.T) {
	candidate := directChatbotCandidate("webchat", capability.ConversationEndpointDirect, ConversationReplyProviderDefault)
	candidate.ConversationEndpoints[0].Name = "Embedded web chat"
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode:    ModeCreate,
		Prompt:  "Create a web chatbot that listens for customer messages and responds.",
		Catalog: webChatbotCatalog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := result.Candidate.ConversationEndpoints[0]
	if !result.Valid || endpoint.SkillID != "webchat" || endpoint.Mode != capability.ConversationEndpointDirect ||
		endpoint.Handler.Kind != ConversationHandlerAgent || endpoint.Policy.ReplyMode != ConversationReplyProviderDefault {
		t.Fatalf("web-chat composition = %#v, result=%#v", endpoint, result)
	}
}

func TestCompilerRepairsConversationArchitectureToDerivedRequirement(t *testing.T) {
	directPayload, err := json.Marshal(GenerationResponse{Candidate: directChatbotCandidate(
		"slack", capability.ConversationEndpointChannel, ConversationReplyThread,
	)})
	if err != nil {
		t.Fatal(err)
	}
	runbookPayload, err := json.Marshal(GenerationResponse{Candidate: slackChatbotCandidate()})
	if err != nil {
		t.Fatal(err)
	}
	generator := &repairingGenerator{generated: directPayload, repaired: runbookPayload}
	compiler, err := NewCompiler(generator)
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: orchestratedSlackChatbotPrompt, Catalog: slackChatbotCatalog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || generator.repairs != 1 ||
		result.Candidate.ConversationEndpoints[0].Handler.Kind != ConversationHandlerRunbook {
		t.Fatalf("repaired composition = %#v, repairs=%d", result, generator.repairs)
	}
}

func TestChangeSetPreservesReactiveCompositionAndCreatesEndpointPlacement(t *testing.T) {
	payload, err := json.Marshal(GenerationResponse{Candidate: slackChatbotCandidate()})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	if err != nil {
		t.Fatal(err)
	}
	changeSet, replayed, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope:  capability.ScopeReference{Kind: "tenant", ID: "one"},
		Prompt: orchestratedSlackChatbotPrompt, Catalog: slackChatbotCatalog(),
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "slack-chatbot",
	})
	if err != nil || replayed {
		t.Fatalf("create replayed=%t err=%v", replayed, err)
	}
	endpoint := changeSet.Result.Candidate.ConversationEndpoints[0]
	if endpoint.Owner.ID != "tenant/one/slack-response-agent" ||
		endpoint.Handler.AgentDefinitionID != "tenant/one/slack-response-agent" {
		t.Fatalf("canonical endpoint owner/handler = %#v", endpoint)
	}
	var delegatedAgentID string
	if err := json.Unmarshal(
		changeSet.Result.Candidate.Agents[0].Runbook.Steps["reason"].Delegate.AgentID.Literal,
		&delegatedAgentID,
	); err != nil || delegatedAgentID != "tenant/one/slack-response-agent" {
		t.Fatalf("canonical delegated Agent id = %q, %v", delegatedAgentID, err)
	}
	placement := changeSet.Placement.ConversationEndpoints[endpoint.ID]
	if placement.ID == "" || placement.ExpectedRevision != 0 {
		t.Fatalf("endpoint placement = %#v", placement)
	}
	required := changeSet.RequiredCredentialBindings[endpoint.Owner.ID]
	if len(required) != 1 || required[0].Key != "SLACK_CONNECTION" || required[0].OAuth2 == nil {
		t.Fatalf("endpoint credential contract = %#v", changeSet.RequiredCredentialBindings)
	}
	if !changeSet.Result.Valid || len(changeSet.Result.Validation) != 0 ||
		len(changeSet.Result.MissingRequirements) != 0 {
		t.Fatalf("durable chatbot candidate = %#v", changeSet.Result)
	}
}
