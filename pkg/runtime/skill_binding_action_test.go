package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestGovernedSkillBindingActionMaterializesApprovedUpsertAndDisable(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	deploymentID := "research-agent"
	catalog := skillActionCatalog(t, ctx, scope, deploymentID)
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "Skill access requires review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	validator, err := NewSkillBindingActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, policy, validator)
	run := createClaimedSkillActionRun(t, ctx, store, scope, deploymentID, "worker-upsert")
	arguments := map[string]interface{}{
		"bindingId": "reddit", "expectedRevision": 0,
		"skillId": "reddit.reader", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{"read"}, "enablePrompt": true, "maximumRisk": "read",
		"accessReferences": map[string]interface{}{"reddit": map[string]interface{}{"kind": "reddit-oauth", "id": "credential://tenant-a/reddit"}},
		"config":           map[string]interface{}{"subreddits": []interface{}{"kubernetes", "devops"}},
	}
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-upsert", DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionUpsertBinding,
		Arguments: arguments, IdempotencyKey: "message-1:add-reddit", Summary: "Enable Reddit research",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal lifecycle = %#v", proposal)
	}
	encodedPreview, _ := json.Marshal(proposal.Approval.ProposedAction)
	if !strings.Contains(string(encodedPreview), "credential://tenant-a/reddit") || strings.Contains(string(encodedPreview), "client_secret") {
		t.Fatalf("approval preview is not opaque and secret-safe: %s", encodedPreview)
	}
	if proposal.Approval.ProposedAction["deploymentId"] != deploymentID || proposal.Approval.ProposedAction["resourceType"] != "skill_binding" {
		t.Fatalf("typed approval preview = %#v", proposal.Approval.ProposedAction)
	}
	approveSkillAction(t, ctx, store, scope, proposal)
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewActionWorker(store, catalog, nil, dispatcher)
	executed, err := worker.RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["operation"] != SkillActionUpsertBinding || executed.Call.Output["replayed"] != false {
		t.Fatalf("upsert execution status=%s error=%q output=%#v", executed.Call.Status, executed.Call.Error, executed.Call.Output)
	}
	binding, err := catalog.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID, "reddit")
	if err != nil || binding == nil || binding.Revision != 1 || binding.Disabled || binding.Credentials["reddit"].ID != "credential://tenant-a/reddit" {
		t.Fatalf("materialized binding = %#v, %v", binding, err)
	}
	if len(binding.Lifecycle) != 1 || binding.Lifecycle[0].Actor.Type != "user" || binding.Lifecycle[0].Actor.ID != "operator" {
		t.Fatalf("binding audit = %#v", binding.Lifecycle)
	}

	disableRun, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker-disable", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || disableRun == nil || disableRun.ID != run.ID {
		t.Fatalf("reclaim after upsert = %#v, %v", disableRun, err)
	}
	disableProposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: disableRun.ID, WorkerID: "worker-disable", DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionDisableBinding,
		Arguments:      map[string]interface{}{"bindingId": "reddit", "expectedRevision": 1},
		IdempotencyKey: "message-2:disable-reddit", Summary: "Disable Reddit research",
	})
	if err != nil {
		t.Fatal(err)
	}
	approveSkillAction(t, ctx, store, scope, disableProposal)
	executed, err = worker.RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil || executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["operation"] != SkillActionDisableBinding {
		t.Fatalf("disable execution = %#v, %v", executed, err)
	}
	disabled, _ := catalog.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID, "reddit")
	if disabled == nil || !disabled.Disabled || disabled.Revision != 2 || len(disabled.Lifecycle) != 2 || disabled.Lifecycle[1].Action != skill.BindingLifecycleDisabled {
		t.Fatalf("disabled binding = %#v", disabled)
	}

	reenableRun, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker-reenable", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || reenableRun == nil || reenableRun.ID != run.ID {
		t.Fatalf("reclaim after disable = %#v, %v", reenableRun, err)
	}
	reenable := cloneMap(arguments)
	reenable["expectedRevision"] = 2
	reenable["config"] = map[string]interface{}{"subreddits": []interface{}{"kubernetes"}}
	reenableProposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: reenableRun.ID, WorkerID: "worker-reenable", DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionUpsertBinding,
		Arguments: reenable, IdempotencyKey: "message-3:reenable-reddit", Summary: "Re-enable Reddit research",
	})
	if err != nil {
		t.Fatal(err)
	}
	approveSkillAction(t, ctx, store, scope, reenableProposal)
	executed, err = worker.RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil || executed.Call.Status != ActionCallStatusSucceeded {
		t.Fatalf("re-enable execution = %#v, %v", executed, err)
	}
	enabled, _ := catalog.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID, "reddit")
	if enabled == nil || enabled.Disabled || enabled.Revision != 3 || len(enabled.Lifecycle) != 3 || enabled.Lifecycle[2].Action != skill.BindingLifecycleEnabled {
		t.Fatalf("re-enabled binding = %#v", enabled)
	}
}

func TestSkillDiscoveryIsReadOnlySelfScopedPaginatedAndCredentialFree(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	deploymentID := "research-agent"
	catalog := skillActionCatalog(t, ctx, scope, deploymentID)
	var received skill.DiscoveryRequest
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		received = request
		return &skill.DiscoveryPage{
			Items: []skill.DiscoveryCandidate{{
				ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader", Description: "Read configured communities.",
				Actions:             []skill.DiscoveryAction{{Name: "read", Description: "Read Reddit posts.", Risk: skill.RiskLevelRead}},
				Credentials:         []skill.DiscoveryCredential{{Name: "reddit", Kind: "reddit-oauth", Configured: true}},
				BindingConfigSchema: map[string]interface{}{"type": "object", "required": []interface{}{"community"}, "properties": map[string]interface{}{"community": map[string]interface{}{"type": "string"}}},
				MaximumRisk:         skill.RiskLevelRead, Readiness: skill.DiscoveryReadinessBindable,
				Compatibility: []skill.DiscoveryCompatibility{{Requirement: "reddit research", Compatible: true, Evidence: "native read action"}},
			}},
			NextCursor: "page-2",
		}, nil
	})
	validator, err := NewSkillBindingActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
	run := createClaimedSkillActionRun(t, ctx, store, scope, deploymentID, "worker-discover")
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker-discover", DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionDiscoverBinding,
		Arguments: map[string]interface{}{
			"query": "reddit research", "requiredActions": []interface{}{"read"}, "maximumRisk": "read", "limit": 4,
		},
		Summary: "Find a Reddit research Skill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval != nil || proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("read-only discovery proposal = %#v", proposal)
	}
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["nextCursor"] != "page-2" {
		t.Fatalf("discovery execution status=%s error=%q output=%#v", executed.Call.Status, executed.Call.Error, executed.Call.Output)
	}
	if received.Scope != (skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}) || received.DeploymentID != deploymentID || received.Limit != 4 || len(received.RequiredActions) != 1 || received.RequiredActions[0] != "read" {
		t.Fatalf("discovery authority was not derived from the Run: %#v", received)
	}
	encoded, _ := json.Marshal(executed.Call.Output)
	if strings.Contains(string(encoded), "credential://") || strings.Contains(string(encoded), "credentialId") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("discovery exposed credential identity or material: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"bindingConfigSchema":{"properties":{"community"`) {
		t.Fatalf("discovery omitted reviewed binding configuration constraints: %s", encoded)
	}
}

func TestSkillDiscoveryRejectsInvalidProviderPages(t *testing.T) {
	request, err := skill.NormalizeDiscoveryRequest(skill.DiscoveryRequest{
		Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", Query: "research", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate := skill.DiscoveryCandidate{ID: "reader", Version: "1", Name: "Reader", Readiness: skill.DiscoveryReadinessBindable}
	if _, err := skill.NormalizeDiscoveryPage(request, &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{duplicate, duplicate}}); err == nil {
		t.Fatal("duplicate exact Skill identities were accepted")
	}
	if _, err := skill.NormalizeDiscoveryPage(request, &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{
		{ID: "reader", Version: "1", Name: "Reader", Readiness: "invented"},
	}}); err == nil {
		t.Fatal("invented readiness was accepted")
	}
	if _, err := skill.NormalizeDiscoveryPage(request, &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{
		ID: "reader", Version: "1", Name: "Reader", Readiness: skill.DiscoveryReadinessBindable,
		BindingConfigSchema: map[string]interface{}{"$ref": "https://attacker.invalid/schema.json"},
	}}}); err == nil {
		t.Fatal("external binding configuration schema was accepted")
	}
}

func TestSkillBindingActionRejectsCrossAgentUnknownSourceCASAndSecretsBeforeApproval(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	catalog := skillActionCatalog(t, ctx, scope, "agent-a")
	validator, _ := NewSkillBindingActionValidator(catalog)
	management := SkillManagementSkill()
	bound, err := catalog.Resolve(ctx, skillScope, "agent-a", management.ID, management.Version, SkillActionUpsertBinding)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]interface{}{
		"bindingId": "reader", "expectedRevision": 0, "skillId": "reddit.reader", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{"read"}, "enablePrompt": false, "maximumRisk": "read",
		"accessReferences": map[string]interface{}{"reddit": map[string]interface{}{"kind": "reddit-oauth", "id": "credential://reddit"}},
	}

	foreignRun := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-b"}, AssignedAgentID: "agent-b"}
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: foreignRun, Bound: bound, Arguments: base}); err == nil {
		t.Fatal("management binding from agent-a escalated into agent-b")
	}
	store := NewMemoryStore()
	durableForeign, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: foreignRun.Owner, AssignedAgentID: foreignRun.AssignedAgentID, Goal: "Manage Skills", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewSkillBindingActionDispatcher(store, catalog, nil)
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{
		Call: &ActionCall{Scope: scope, RunID: durableForeign.ID, DeploymentID: "agent-a"}, Bound: bound, Arguments: base,
	}); err == nil {
		t.Fatal("dispatcher allowed a cross-Agent Skill mutation")
	}

	selfRun := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a"}
	unknown := cloneMap(base)
	unknown["skillId"] = "missing"
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: selfRun, Bound: bound, Arguments: unknown}); err == nil {
		t.Fatal("unknown Skill reached approval")
	}

	sourced := redditSkillDefinition()
	sourced.ID = "sourced.reader"
	sourced.Source = &capability.SourceProvenance{Identity: "clawhub://publisher/reddit@1.0.0", Format: "openclaw"}
	if err := catalog.Register(ctx, sourced); err != nil {
		t.Fatal(err)
	}
	missingSource := cloneMap(base)
	missingSource["skillId"] = sourced.ID
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: selfRun, Bound: bound, Arguments: missingSource}); err == nil || !strings.Contains(err.Error(), "sourceIdentity") {
		t.Fatalf("sourced Skill without exact source error = %v", err)
	}

	secretConfig := cloneMap(base)
	secretConfig["config"] = map[string]interface{}{"client_secret": "raw-secret"}
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: selfRun, Bound: bound, Arguments: secretConfig}); err == nil || !strings.Contains(err.Error(), "opaque credential") {
		t.Fatalf("secret config error = %v", err)
	}

	if _, err := catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{
		Binding: &skill.Binding{
			ID: "reader", Scope: skillScope, DeploymentID: "agent-a", SkillID: "reddit.reader", SkillVersion: "1.0.0",
			AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead,
			Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://reddit"}},
		},
		ExpectedRevision: 0, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "initial",
	}); err != nil {
		t.Fatal(err)
	}
	stale := cloneMap(base)
	stale["bindingId"] = "reader"
	stale["expectedRevision"] = 0
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: selfRun, Bound: bound, Arguments: stale}); !errors.Is(err, skill.ErrBindingRevisionConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
}

func TestSkillManagementSkillMakesPromptOnlyAuthorityExplicit(t *testing.T) {
	action := SkillManagementSkill().Actions[SkillActionUpsertBinding]
	properties, ok := action.InputSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("upsert properties = %#v", action.InputSchema["properties"])
	}
	allowed, ok := properties["allowedActions"].(map[string]interface{})
	if !ok {
		t.Fatalf("allowedActions schema = %#v", properties["allowedActions"])
	}
	description, _ := allowed["description"].(string)
	if !strings.Contains(description, "empty array for prompt-only access") || !strings.Contains(description, "Wildcards") {
		t.Fatalf("allowedActions description does not explain prompt-only authority: %q", description)
	}
	items, ok := allowed["items"].(map[string]interface{})
	if !ok || items["pattern"] != `^[^*]+$` {
		t.Fatalf("allowedActions item schema permits wildcard authority: %#v", allowed["items"])
	}
	if _, ok := properties["enabledConversationAdapters"].(map[string]interface{}); !ok {
		t.Fatalf("conversation adapter authority is absent from binding schema: %#v", properties)
	}
	discover := SkillManagementSkill().Actions[SkillActionDiscoverBinding]
	outputProperties := discover.OutputSchema["properties"].(map[string]interface{})
	candidates := outputProperties["items"].(map[string]interface{})["items"].(map[string]interface{})
	candidateProperties := candidates["properties"].(map[string]interface{})
	if _, ok := candidateProperties["conversationAdapters"].(map[string]interface{}); !ok {
		t.Fatalf("conversation adapters are absent from discovery schema: %#v", candidateProperties)
	}
}

func TestSkillManagementMaterializesAdapterOnlyAuthority(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	deploymentID := "slack-agent"
	catalog := skill.NewCatalog()
	for _, definition := range []*skill.Definition{SkillManagementSkill(), slackConversationSkillDefinition()} {
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "skills", Scope: skillScope, DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion,
		AllowedActions: []string{SkillActionUpsertBinding}, MaximumRisk: skill.RiskLevelWrite, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, skillScope, deploymentID, SkillManagementSkillID, SkillManagementSkillVersion, SkillActionUpsertBinding)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID},
		AssignedAgentID: deploymentID, Goal: "Enable Slack conversations", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]interface{}{
		"bindingId": "slack", "expectedRevision": 0,
		"skillId": "slack", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{}, "enablePrompt": false,
		"enabledConversationAdapters": []interface{}{"conversations"}, "maximumRisk": "read",
		"accessReferences": map[string]interface{}{"SLACK_CONNECTION": map[string]interface{}{
			"kind": "slack-oauth", "id": "connection://tenant-a/slack",
		}},
	}
	validator, err := NewSkillBindingActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: arguments,
	})
	if err != nil || preview["resourceType"] != "skill_binding" {
		t.Fatalf("adapter binding preview = %#v, %v", preview, err)
	}
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{
		Call: &ActionCall{Scope: scope, RunID: run.ID, DeploymentID: deploymentID}, Bound: bound, Arguments: arguments,
	})
	if err != nil || result["operation"] != SkillActionUpsertBinding {
		t.Fatalf("adapter binding dispatch = %#v, %v", result, err)
	}
	binding, err := catalog.GetBinding(ctx, skillScope, deploymentID, "slack")
	if err != nil || binding == nil || len(binding.EnabledConversationAdapters) != 1 ||
		binding.EnabledConversationAdapters[0] != "conversations" || len(binding.AllowedActions) != 0 {
		t.Fatalf("adapter-only binding = %#v, %v", binding, err)
	}
}

func skillActionCatalog(t *testing.T, ctx context.Context, scope Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, SkillManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, redditSkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "skills", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion,
		AllowedActions: []string{SkillActionDiscoverBinding, SkillActionUpsertBinding, SkillActionDisableBinding}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func redditSkillDefinition() *skill.Definition {
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

func slackConversationSkillDefinition() *skill.Definition {
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

func createClaimedSkillActionRun(t *testing.T, ctx context.Context, store *MemoryStore, scope Scope, deploymentID, workerID string) *AgentRun {
	t.Helper()
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
		Goal: "Manage Skills", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: workerID, Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

func approveSkillAction(t *testing.T, ctx context.Context, store *MemoryStore, scope Scope, proposal *ActionProposalResult) {
	t.Helper()
	resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "approve-" + proposal.Call.ID, Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
}
