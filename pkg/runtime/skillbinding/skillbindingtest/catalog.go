package skillbindingtest

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// SkillActionCatalog registers the management Skill and the Reddit reader
// and binds deploymentID to discover, upsert and disable bindings.
func SkillActionCatalog(t *testing.T, ctx context.Context, scope models.Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, skillbinding.SkillManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, RedditSkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "skills", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
		AllowedActions: []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// RedditSkillDefinition is a reader Skill with one read action that needs a
// person's Reddit credential.
func RedditSkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader", Description: "Read configured communities.",
		Transport: skill.TransportReference{Kind: "http", Endpoint: "https://reddit.example"},
		BindingConfigSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"subreddits": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}},
		},
		Prompt: &capability.PromptModule{
			Instructions: "Use Reddit evidence.", UserInvocable: true,
			Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
		},
		Actions: map[string]skill.Action{
			"read": {
				Name: "read", Description: "Read Reddit posts.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}
}

// HostConnectorCatalog registers a connector Skill whose bindings the host
// supplies per run, as connected apps are: only runs in allowed get the
// linear binding.
func HostConnectorCatalog(t *testing.T, scope models.Scope, allowed map[string]bool) *skill.Catalog {
	t.Helper()
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "mcp", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		},
	}); err != nil {
		t.Fatal(err)
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"title": map[string]interface{}{"type": "string"}}}
	catalog.SetHostBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if skillScope.ID != scope.ID || !allowed[actor.RunID] {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@1", Services: map[string][]string{"mcp:linear": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:linear", Revision: 1, Scope: skillScope, DeploymentID: deploymentID, SkillID: "openseal.connector", SkillVersion: "1.0.0",
			AllowedActions: []string{"read", "call"}, MaximumRisk: skill.RiskLevelWrite, Slug: "linear",
			Tools: []skill.BindingTool{
				{Name: "create_issue", Description: "Create a Linear issue", Effect: skill.BindingToolEffectWrite, InputSchema: schema, Arguments: map[string]interface{}{"tool": "create_issue", "hash": "linear-create_issue"}},
				{Name: "search", Description: "Search Linear issues", Effect: skill.BindingToolEffectRead, InputSchema: schema, Arguments: map[string]interface{}{"tool": "search", "hash": "linear-search"}},
			},
		}}}, nil
	})
	return catalog
}

// MustDefinition returns version 1.0.0 of Skill id from catalog.
func MustDefinition(t *testing.T, catalog *skill.Catalog, id string) *skill.Definition {
	t.Helper()
	definition, err := catalog.GetDefinition(t.Context(), id, "1.0.0")
	if err != nil || definition == nil {
		t.Fatalf("definition %s: %v", id, err)
	}
	return definition
}

// SlackConversationSkillDefinition is a Slack skill with one conversation
// adapter, "conversations", that receives messages and approval decisions
// and sends messages with the SLACK_CONNECTION OAuth installation.
func SlackConversationSkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: "slack", Version: "1.0.0", Name: "Slack",
		Actions: map[string]skill.Action{},
		ConversationAdapters: map[string]skill.ConversationAdapter{"conversations": {
			ProtocolVersion: skill.ConversationAdapterProtocolV1,
			Name:            "Slack conversations", Description: "Receive and deliver Slack conversations.", Provider: "slack",
			EndpointModes:     []skill.ConversationEndpointMode{skill.ConversationEndpointChannel},
			InboundEventTypes: []string{skill.ConversationEventApprovalDecided, skill.ConversationEventMessageReceived},
			Features:          []skill.ConversationAdapterFeature{skill.ConversationFeatureMentions, skill.ConversationFeatureThreads},
			Credentials: []skill.CredentialRequirement{{
				Name: "SLACK_CONNECTION", Kind: "slack-oauth",
				OAuth2: &skill.OAuth2Requirement{
					Provider: "slack", Subject: skill.OAuth2SubjectInstallation,
					Scopes: []string{"channels:history", "chat:write"},
				},
			}},
			Delivery: skill.ConversationDeliveryCapabilities{
				Operations: []skill.ConversationDeliveryOperation{skill.ConversationDeliveryMessageSend},
				Ordering:   skill.ConversationDeliveryOrderThread, Idempotency: skill.IdempotencyRequired,
				SupportsAcknowledgementLookup: true, SupportsRetryAfter: true,
			},
			Transport: skill.ConversationAdapterTransport{
				Kind: "plugin", IngressEndpoint: "slack.conversation.ingress", DeliveryEndpoint: "slack.conversation.deliver",
				DeliveryCredentials: []string{"SLACK_CONNECTION"},
			},
		}},
	}
}
