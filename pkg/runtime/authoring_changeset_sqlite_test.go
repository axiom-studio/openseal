package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestSQLiteWorkforceChangeSetsAreConcurrentRestartSafeAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	value := testWorkforceChangeSet(scope, "change-one")
	var created, replayed atomic.Int32
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, replay, err := store.CreateChangeSet(context.Background(), value, "intent-one", "request-one")
			if err != nil || result == nil || result.ID != value.ID {
				t.Errorf("create = %#v, replay = %t, err = %v", result, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	if created.Load() != 1 || replayed.Load() != 1 {
		t.Fatalf("created = %d, replayed = %d", created.Load(), replayed.Load())
	}
	if _, _, err := store.GetChangeSetByIdempotency(context.Background(), scope, "intent-one", "different"); !errors.Is(err, authoring.ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := store.GetChangeSet(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, value.ID); !errors.Is(err, authoring.ErrChangeSetNotFound) {
		t.Fatalf("cross-scope read = %v", err)
	}
	updated := *value
	updated.Status, updated.Revision = authoring.ChangeSetReady, 2
	updated.UpdatedAt = value.UpdatedAt.Add(time.Minute)
	updated.ApprovalDecisions = []authoring.ChangeSetApprovalDecision{{ID: "decision", EvaluationID: "evaluation", PolicyID: "production", Role: "operator", Approved: true, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, DecidedAt: updated.UpdatedAt}}
	if persisted, err := store.UpdateChangeSet(context.Background(), &updated, 1); err != nil || persisted.Revision != 2 {
		t.Fatalf("update = %#v, err = %v", persisted, err)
	}
	stale := updated
	stale.Status, stale.Revision = authoring.ChangeSetRejected, 3
	if _, err := store.UpdateChangeSet(context.Background(), &stale, 1); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale update = %v", err)
	}
	modifiedCandidate := updated
	modifiedCandidate.CandidateDigest, modifiedCandidate.Revision = "changed", 3
	if _, err := store.UpdateChangeSet(context.Background(), &modifiedCandidate, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("candidate mutation = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), scope, value.ID)
	if err != nil || restored.CandidateDigest != value.CandidateDigest || restored.Status != authoring.ChangeSetReady || restored.Revision != 2 || len(restored.ApprovalDecisions) != 1 || restored.ApprovalDecisions[0].ID != "decision" {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceApplyPersistsWholeAggregateAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil || result.Status != authoring.ChangeSetApplied || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err = store.GetDefinition(context.Background(), "agent", "1"); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(context.Background(), value.Scope, "agent-live"); err != nil || deployment.ActiveVersion != "1" {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	if teamDeployment, err := store.GetTeamDeployment(context.Background(), value.Scope, "team-live"); err != nil || len(teamDeployment.Roster) != 1 {
		t.Fatalf("team=%#v err=%v", teamDeployment, err)
	}
	objectives, err := store.ListObjectives(context.Background(), ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("objectives=%d err=%v", len(objectives), err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), value.Scope, value.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteWorkforceApplyMaterializesObjectiveOwnedScheduledRunbook(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	value := testApplicableWorkforceChangeSet()
	definition := value.Result.Candidate.Agents[0]
	objectiveRef := authoring.WorkforceObjectiveKey("agent", definition.ID, definition.ObjectiveTemplates[0].ID)
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "agent-operations", Version: "1.0.0", Name: "Agent operations",
		Entrypoints: map[string]string{"operate": "done"},
		Triggers: map[string]runbook.Trigger{"hourly": {
			Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"},
			Entrypoint: "operate", ObjectiveID: objectiveRef,
		}},
		Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
	if _, _, err = store.CreateChangeSet(ctx, value, "create-runbook", "digest-runbook"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-runbook", IdempotencyKey: "apply-runbook", CandidateDigest: value.CandidateDigest,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute),
	}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}

	stored, err := store.GetDefinition(ctx, "agent", "1")
	if err != nil || stored.Runbook == nil || stored.Runbook.Entrypoints["operate"] != "done" {
		t.Fatalf("stored Agent Runbook = %#v, err = %v", stored, err)
	}
	deployment, err := store.GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || deployment.RolloutStatus != agent.RolloutActive || deployment.ActiveVersion != stored.Version {
		t.Fatalf("active Agent deployment = %#v, err = %v", deployment, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var objective *Objective
	for _, candidate := range objectives {
		if candidate.ID == "objective:agent" {
			objective = candidate
		}
	}
	if objective == nil {
		t.Fatalf("Objective must remain an outcome without execution configuration: %#v", objectives)
	}
	activations, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(activations) != 1 || activations[0].AssignedAgentID != "agent-live" ||
		activations[0].DefinitionID != definition.Runbook.ID || activations[0].DefinitionVersion != definition.Runbook.Version ||
		activations[0].Trigger.Entrypoint != "operate" {
		t.Fatalf("materialized Runbook activation=%#v err=%v", activations, err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, ObjectiveID: objective.ID,
		Owner: objective.Owner, AssignedAgentID: "agent-live", Entrypoint: activations[0].Trigger.Entrypoint,
		Goal: objective.Goal, Source: RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ResolveCatalogTurnRunner(ctx, &resolverCatalog{
		deployment: deployment, definition: stored,
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot-runbook", Scope: deployment.Scope, DeploymentID: deployment.ID,
		},
	}, run, CatalogTurnResolverConfig{})
	if err != nil {
		t.Fatalf("resolve applied Agent Runbook entrypoint: %v", err)
	}
	if _, ok := binding.Runner.(*RunbookTurnRunner); !ok {
		t.Fatalf("scheduled entrypoint resolved runner = %T", binding.Runner)
	}
}

func TestSQLiteAtomicWorkforceApplyMaterializesConversationEndpointAndAdapterBinding(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const slackSourceIdentity = "https://github.com/axiom-studio/skills::skill-slack"
	installedSlack := slackConversationSkillDefinition()
	installedSlack.CallbackAdapters = map[string]skill.CallbackAdapter{"interactions": {
		ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Approval interactions",
		Description: "Verify signed interactive approval decisions.", Provider: "slack",
		EventTypes:  []string{capability.CallbackEventApprovalDecided},
		Credentials: []capability.CredentialRequirement{{Name: "SLACK_CONNECTION", Kind: "slack-oauth"}},
		Transport:   skill.CallbackAdapterTransport{Kind: "http", IngressEndpoint: "/callbacks/slack", IngressCredentials: []string{"SLACK_CONNECTION"}},
	}}
	installedSlack.Source = &skill.SourceProvenance{Identity: slackSourceIdentity, Format: "openseal.skill.v1"}
	if err := skill.NewCatalogWithStore(store).Register(ctx, installedSlack); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	self, _ := json.Marshal("slack-agent")
	goal, _ := json.Marshal("Respond to the triggering message.")
	definition := &agent.AgentDefinition{
		ID: "slack-agent", Version: "1.0.0", DisplayName: "Slack agent",
		Purpose: "Respond to Slack messages", SystemPrompt: "Respond helpfully.",
		Authority: agent.AuthorityPolicy{
			MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1,
			ApprovalDestinations: []agent.ApprovalDestination{{EndpointID: "channel"}},
		},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "respond", Title: "Respond", Goal: "Respond to permitted Slack messages", Priority: 1}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "respond", Version: "1.0.0", Name: "Respond",
			Entrypoints: map[string]string{"respond": "delegate"},
			Interfaces: map[string]runbook.Interface{"respond": {
				Description: "Respond to a canonical message.",
				InputSchema: authoring.CanonicalConversationTriggerInputSchema(),
			}},
			Triggers: map[string]runbook.Trigger{"on-message": {
				Kind: runbook.TriggerEvent, EventType: capability.ConversationEventMessageReceived, Entrypoint: "respond",
				ObjectiveID: authoring.WorkforceObjectiveKey("agent", "slack-agent", "respond"),
			}},
			Steps: map[string]runbook.Step{
				"delegate": {
					Kind: runbook.StepDelegate,
					Delegate: &runbook.DelegateStep{
						AgentID: runbook.Value{Literal: self}, Goal: runbook.Value{Literal: goal},
						Mode: runbook.DelegateReason, ResultPath: "/results/response",
						Budget: &runbook.BudgetAllocation{MaxTurns: 4, MaxTotalTokens: 26000}, Next: "done",
					},
				},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{
					"summary": {Ref: "/results/response/summary"},
				}}},
			},
		},
	}
	oauth := &capability.OAuth2Requirement{
		Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
		Scopes: []string{"channels:history", "chat:write"},
	}
	value := &authoring.ChangeSet{
		ID: "slack-chatbot", Scope: scope, Mode: authoring.ModeCreate,
		Prompt: "Create a Slack chatbot", PromptDigest: "prompt", CandidateDigest: "candidate",
		Result: authoring.CompileResult{Valid: true, Candidate: authoring.WorkforceCandidate{
			Activation: authoring.WorkforceActivationActive,
			Agents:     []*agent.AgentDefinition{definition},
			ConversationEndpoints: []authoring.ConversationEndpointBlueprint{{
				ID: "channel", Name: "Installed Slack channel",
				Owner:   authoring.ConversationEndpointOwner{Type: authoring.ConversationEndpointOwnerAgent, ID: definition.ID},
				SkillID: "slack", SkillVersion: "1.0.0", AdapterID: "conversations", CallbackAdapterID: "interactions",
				Mode: capability.ConversationEndpointChannel,
				Handler: authoring.ConversationHandlerBlueprint{
					Kind: authoring.ConversationHandlerRunbook, AgentDefinitionID: definition.ID,
					RunbookID: "respond", RunbookVersion: "1.0.0", Trigger: "on-message",
				},
				Policy: authoring.ConversationEndpointPolicyBlueprint{
					MessageSelection: authoring.ConversationSelectDirectOrMention,
					ReplyMode:        authoring.ConversationReplyThread, IgnoreBots: true,
				},
				CanonicalReply: true, Purposes: []authoring.ConversationEndpointPurpose{
					authoring.ConversationEndpointPurposeConversation, authoring.ConversationEndpointPurposeApprovals,
				}, ArchitectureReason: "Use a durable event Runbook and canonical delivery.",
			}},
		}},
		Catalog: authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
			"slack": {
				ID: "slack", Version: "1.0.0", SourceIdentity: slackSourceIdentity, Readiness: authoring.SkillReadinessReady,
				ConversationAdapters: []authoring.ConversationAdapterCapability{{
					ID: "conversations", ProtocolVersion: capability.ConversationAdapterProtocolV1,
					Provider: "slack", EndpointModes: []capability.ConversationEndpointMode{capability.ConversationEndpointChannel},
					InboundEventTypes: []string{capability.ConversationEventMessageReceived},
					Features: []capability.ConversationAdapterFeature{
						capability.ConversationFeatureMentions, capability.ConversationFeatureThreads,
					},
					Delivery: capability.ConversationDeliveryCapabilities{
						Operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend},
						Ordering:   capability.ConversationDeliveryOrderThread, Idempotency: capability.IdempotencyRequired,
						SupportsAcknowledgementLookup: true, SupportsRetryAfter: true,
					},
					Credentials: []authoring.SkillCredential{{
						Name: "SLACK_CONNECTION", Kind: "slack-oauth", OAuth2: oauth,
					}},
				}},
				CallbackAdapters: []authoring.CallbackAdapterCapability{{
					ID: "interactions", ProtocolVersion: capability.CallbackAdapterProtocolV1,
					Provider: "slack", EventTypes: []string{capability.CallbackEventApprovalDecided},
					Credentials: []authoring.SkillCredential{{Name: "SLACK_CONNECTION", Kind: "slack-oauth"}},
				}},
			},
		}},
		Placement: authoring.ChangeSetPlacement{
			AgentDeploymentIDs: map[string]string{"slack-agent": "slack-agent-live"},
			Objectives: map[string]authoring.ObjectivePlacement{
				authoring.WorkforceObjectiveKey("agent", "slack-agent", "respond"): {ID: "objective:slack-respond"},
			},
			CredentialReferences: map[string]map[string]capability.CredentialReference{
				"slack-agent": {"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/one/slack"}},
			},
			ConversationEndpoints: map[string]authoring.ConversationEndpointPlacement{
				"channel": {
					ID: "conversation-endpoint:slack-channel", Address: "C012345",
					CallbackRegistrationID: "callback-registration:slack-channel",
				},
			},
			Environment: "test",
		},
		Status: authoring.ChangeSetReady, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"},
		Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err = store.CreateChangeSet(ctx, value, "create-chatbot", "create-chatbot"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-chatbot", IdempotencyKey: "apply-chatbot", CandidateDigest: value.CandidateDigest,
		Activation: authoring.WorkforceActivationActive, Actor: value.Actor, AppliedAt: now.Add(time.Minute),
	}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.GetExternalConversationEndpoint(
		ctx, Scope{Kind: scope.Kind, ID: scope.ID}, "conversation-endpoint:slack-channel",
	)
	if err != nil || endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.DeploymentID != "slack-agent-live" || endpoint.Adapter.BindingRevision != 1 ||
		endpoint.Adapter.SourceIdentity != slackSourceIdentity ||
		endpoint.Handler.Kind != ExternalConversationHandlerRunbook || endpoint.Handler.Trigger != "on-message" ||
		endpoint.Handler.AssignedAgentID != "slack-agent-live" {
		t.Fatalf("materialized endpoint=%#v err=%v", endpoint, err)
	}
	callback, err := store.GetCallbackRegistration(ctx, endpoint.Scope, "callback-registration:slack-channel")
	if err != nil || callback == nil || callback.Status != CallbackRegistrationActive ||
		callback.Adapter.BindingID != endpoint.Adapter.BindingID || callback.Adapter.BindingRevision != endpoint.Adapter.BindingRevision ||
		callback.Adapter.AdapterID != "interactions" || len(callback.Subscriptions) != 1 ||
		callback.Subscriptions[0].EventType != capability.CallbackEventApprovalDecided ||
		callback.Subscriptions[0].Consumer != "approvals" || callback.Subscriptions[0].TargetID != endpoint.ID {
		t.Fatalf("materialized callback=%#v err=%v", callback, err)
	}
	resolver, err := NewCatalogExternalConversationRunbookResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := NewExternalConversationRunbookEventDispatcher(store, resolver)
	conversation := &Conversation{ID: "conversation-one"}
	message := &ChannelMessage{ID: "message-one"}
	event := EventEnvelope{
		ID: "event-one", Scope: endpoint.Scope, Type: capability.ConversationEventMessageReceived,
		Source: "conversation-adapter:slack", Subject: conversation.ID, OccurredAt: now.Add(2 * time.Minute),
		Attributes: map[string]interface{}{"endpointId": endpoint.ID}, Payload: map[string]interface{}{},
	}
	dispatched, err := dispatcher.DispatchExternalConversationRunbook(
		ctx, endpoint.Handler, ExternalConversationDispatchRequest{
			Endpoint: endpoint, Conversation: conversation, Message: message, Event: event,
			IdempotencyKey: "dispatch-materialized-chatbot",
		},
	)
	if err != nil || dispatched == nil {
		t.Fatalf("dispatch materialized endpoint=%#v err=%v", dispatched, err)
	}
	run, err := store.GetAgentRun(ctx, endpoint.Scope, dispatched.RunID)
	if err != nil || run.AssignedAgentID != "slack-agent-live" || run.Entrypoint != "respond" ||
		run.Context["endpointId"] != endpoint.ID || run.Context["triggerMessageId"] != message.ID {
		t.Fatalf("materialized conversation Run=%#v err=%v", run, err)
	}
	bindings, err := store.ListSkillBindings(ctx, scope, "slack-agent-live")
	if err != nil || len(bindings) != 1 || len(bindings[0].AllowedActions) != 0 ||
		bindings[0].SourceIdentity != slackSourceIdentity ||
		len(bindings[0].EnabledConversationAdapters) != 1 || len(bindings[0].EnabledCallbackAdapters) != 1 ||
		bindings[0].EnabledConversationAdapters[0] != "conversations" ||
		bindings[0].EnabledCallbackAdapters[0] != "interactions" ||
		bindings[0].Credentials["SLACK_CONNECTION"].ID != "connection://tenant/one/slack" {
		t.Fatalf("adapter-only binding=%#v err=%v", bindings, err)
	}
	stored, err := store.GetDefinition(ctx, "slack-agent", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Channels) != 1 || stored.Channels[0].EndpointID != endpoint.ID ||
		stored.Channels[0].Trigger != "on-message" || stored.Channels[0].MessageSelection != "direct_or_mentions" ||
		stored.Channels[0].ReplyMode != "thread" || !stored.Channels[0].IgnoreBots ||
		len(stored.Channels[0].Purposes) != 2 || stored.Channels[0].Purposes[0] != "conversation" || stored.Channels[0].Purposes[1] != "approvals" {
		t.Fatalf("materialized Agent workflow channel=%#v", stored.Channels)
	}
	var delegatedDeployment string
	if err := json.Unmarshal(stored.Runbook.Steps["delegate"].Delegate.AgentID.Literal, &delegatedDeployment); err != nil ||
		delegatedDeployment != "slack-agent-live" {
		t.Fatalf("delegated deployment=%q err=%v", delegatedDeployment, err)
	}
	if len(result.ApplyReceipt.Resources) != 7 {
		t.Fatalf("applied resources=%#v", result.ApplyReceipt.Resources)
	}

	amend := cloneRuntimeChangeSet(value)
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest =
		"slack-chatbot-remove-endpoint", value.ID, authoring.ModeAmend, "candidate-remove-endpoint"
	amend.Result.Candidate.Agents[0].Version = "2.0.0"
	amend.Result.Candidate.Agents[0].Authority.ApprovalDestinations = nil
	amend.Result.Candidate.ConversationEndpoints = nil
	amend.Placement.AgentExpectedRevisions = map[string]int64{"slack-agent": 1}
	amend.Placement.Objectives[authoring.WorkforceObjectiveKey("agent", "slack-agent", "respond")] = authoring.ObjectivePlacement{ID: "objective:slack-respond", ExpectedRevision: 1}
	amend.Placement.ConversationEndpoints = map[string]authoring.ConversationEndpointPlacement{}
	amend.Status, amend.Revision, amend.ApplyReceipt = authoring.ChangeSetReady, 2, nil
	amend.UpdatedAt = applied.UpdatedAt.Add(time.Minute)
	if _, _, err = store.CreateChangeSet(ctx, amend, "remove-chatbot-endpoint", "remove-chatbot-endpoint"); err != nil {
		t.Fatal(err)
	}
	removed := cloneRuntimeChangeSet(amend)
	removed.Status, removed.Revision = authoring.ChangeSetApplied, 3
	removed.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-remove-chatbot-endpoint", IdempotencyKey: "apply-remove-chatbot-endpoint",
		CandidateDigest: amend.CandidateDigest, Activation: authoring.WorkforceActivationActive,
		Actor: amend.Actor, AppliedAt: amend.UpdatedAt.Add(time.Minute),
	}
	removed.UpdatedAt = removed.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, removed, 2); err != nil {
		t.Fatal(err)
	}
	endpoint, err = store.GetExternalConversationEndpoint(
		ctx, Scope{Kind: scope.Kind, ID: scope.ID}, "conversation-endpoint:slack-channel",
	)
	if err != nil || endpoint.Status != ExternalConversationEndpointRetired || endpoint.Revision != 2 {
		t.Fatalf("retired endpoint=%#v err=%v", endpoint, err)
	}
	callback, err = store.GetCallbackRegistration(ctx, endpoint.Scope, "callback-registration:slack-channel")
	if err != nil || callback.Status != CallbackRegistrationRetired || callback.Revision != 2 {
		t.Fatalf("retired callback=%#v err=%v", callback, err)
	}
	bindings, err = store.ListSkillBindings(ctx, scope, "slack-agent-live")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("removed adapter binding=%#v err=%v", bindings, err)
	}
}

func TestSQLiteWorkforceApplyMaterializesExecutableSkillBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "research", Version: "1.0.0", Name: "Research", Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
		BindingConfigSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"sourceId"}, "properties": map[string]interface{}{"sourceId": map[string]interface{}{"type": "integer", "minimum": 1}}},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true, BindingConfigSchema: map[string]interface{}{"type": "object"}},
	}}
	value.Placement.BindingConfigs = map[string]map[string]map[string]interface{}{"agent": {"research": {"sourceId": 17}}}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := catalog.ListModelPrompts(context.Background(), value.Scope, "agent-live")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "research" {
		t.Fatalf("prompts=%#v error=%v", prompts, err)
	}
	bindings, err := store.ListSkillBindings(context.Background(), value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].Config["sourceId"] != float64(17) {
		t.Fatalf("binding config=%#v error=%v", bindings, err)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "skill_binding" && resource.ID == "workforce:agent-live:research"
	}
	if !found {
		t.Fatalf("receipt resources=%#v", result.ApplyReceipt.Resources)
	}
}

func TestWorkforceAuthoringRejectsSkillRequirementsWithoutAuthorityBeforeApply(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "skill-slack", VersionConstraint: "1.0.0"}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"skill-slack"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"skill-slack": {
			ID: "skill-slack", Version: "1.0.0", Actions: []string{"slack-send-message"},
			MaximumRisk: capability.RiskLevelExternal,
		},
	}}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		"agent": {"skill-slack": capability.NewSkillIdentity("skill-slack", "1.0.0", "")},
	}

	issues, err := store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Code != "skill_binding_materialization_failed" ||
		!strings.Contains(issues[0].Message, "must enable a prompt or explicitly allow actions") {
		t.Fatalf("readiness issues = %#v", issues)
	}
	if _, err := materializeWorkforceSkillBindings(value, value.Result.Candidate.Agents[0], "agent-live", false); err == nil {
		t.Fatal("empty Skill authority materialized")
	}
}

func TestWorkforceReadinessBlocksRunbookWithoutReachableApprovalRoute(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const source = "https://skills.example::delivery"
	installed := &skill.Definition{
		ID: "delivery", Version: "1.0.0", Name: "Delivery",
		Source:    &skill.SourceProvenance{Identity: source, Format: "openseal.skill.v1"},
		Transport: skill.TransportReference{Kind: "builtin", Endpoint: "delivery"},
		Actions: map[string]skill.Action{"publish": {
			Name: "publish", Description: "Publish externally.", InputSchema: map[string]interface{}{"type": "object"},
			Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, ExternalOperationPolicy: skill.ExternalOperationRequired,
		}},
	}
	if err := skill.NewCatalogWithStore(store).Register(ctx, installed); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationActive
	definition := value.Result.Candidate.Agents[0]
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	definition.Authority.RequireApprovalAt = capability.RiskLevelExternal
	definition.SkillRequirements = []agent.SkillRequirement{{SkillID: "delivery", VersionConstraint: "1.0.0", RequiredActions: []string{"publish"}}}
	definition.Authority.AllowedSkillIDs = []string{"delivery"}
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "publish", Version: "1", Name: "Publish",
		Entrypoints: map[string]string{"manual": "publish"},
		Steps: map[string]runbook.Step{
			"publish": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "delivery", SkillVersion: "1.0.0", Action: "publish", ResultPath: "/result", Next: "done"}},
			"done":    {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"delivery": {ID: "delivery", Version: "1.0.0", SourceIdentity: source, Actions: []string{"publish"}, MaximumRisk: capability.RiskLevelExternal},
	}}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"delivery": capability.NewSkillIdentity("delivery", "1.0.0", source)},
	}

	issues, err := store.ValidateChangeSetReadiness(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Code != "runbook_activation_action_approval_unreachable" ||
		issues[0].Path != "agents.agent.runbook.steps.publish.action" {
		t.Fatalf("readiness issues = %#v", issues)
	}
	definition.Authority.ApprovalDestinations = []agent.ApprovalDestination{{EndpointID: "approval-channel"}}
	issues, err = store.ValidateChangeSetReadiness(ctx, value)
	if err != nil || len(issues) != 0 {
		t.Fatalf("approved readiness = %#v, %v", issues, err)
	}
}

func TestWorkforceAuthoringReusesCanonicalManagementBindingIdentity(t *testing.T) {
	value := testApplicableWorkforceChangeSet()
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: AgentManagementSkillID, VersionConstraint: AgentManagementSkillVersion,
		RequiredActions: []string{AgentActionAmendBehavior},
	}}
	definition.Authority.AllowedSkillIDs = []string{AgentManagementSkillID}
	definition.Authority.MaximumRisk = capability.RiskLevelWrite
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		AgentManagementSkillID: {
			ID: AgentManagementSkillID, Version: AgentManagementSkillVersion,
			Actions: []string{AgentActionAmendBehavior}, MaximumRisk: capability.RiskLevelWrite,
		},
	}}

	bindings, err := materializeWorkforceSkillBindings(value, definition, "agent-live", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ID != "bundled:agents" {
		t.Fatalf("management bindings = %#v", bindings)
	}
}

func TestWorkforceApplyConvergesDuplicateManagementBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}

	created := testApplicableWorkforceChangeSet()
	if _, _, err := store.CreateChangeSet(context.Background(), created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest,
		Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute),
	}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err := store.ApplyChangeSet(context.Background(), first, 2); err != nil {
		t.Fatal(err)
	}

	value := testApplicableWorkforceChangeSet()
	value.ID, value.ParentID, value.Mode, value.CandidateDigest = "amend-management", created.ID, authoring.ModeAmend, "candidate-management"
	value.Result.Candidate.Agents[0].Version = "2"
	value.Result.Candidate.Team.Version = "2"
	value.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	value.Placement.TeamExpectedRevision = 1
	for key, placement := range value.Placement.Objectives {
		placement.ExpectedRevision = 1
		value.Placement.Objectives[key] = placement
	}
	value.UpdatedAt = first.UpdatedAt.Add(time.Minute)
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: AgentManagementSkillID, VersionConstraint: AgentManagementSkillVersion,
		RequiredActions: []string{AgentActionAmendBehavior},
	}}
	definition.Authority.AllowedSkillIDs = []string{AgentManagementSkillID}
	definition.Authority.MaximumRisk = capability.RiskLevelWrite
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		AgentManagementSkillID: {
			ID: AgentManagementSkillID, Version: AgentManagementSkillVersion,
			Actions: []string{AgentActionAmendBehavior}, MaximumRisk: capability.RiskLevelWrite,
		},
	}}
	for _, id := range []string{"bundled:agents", "workforce:agent-live:" + AgentManagementSkillID} {
		if err := catalog.Bind(context.Background(), &skill.Binding{
			ID: id, Scope: value.Scope, DeploymentID: "agent-live",
			SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
			AllowedActions: []string{AgentActionAmendBehavior}, MaximumRisk: capability.RiskLevelWrite, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: value.CandidateDigest,
		Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute),
	}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err := store.ApplyChangeSet(context.Background(), applied, 2); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListSkillBindings(context.Background(), value.Scope, "agent-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ID != "bundled:agents" || bindings[0].Revision != 2 {
		t.Fatalf("converged bindings = %#v", bindings)
	}
}

func TestInactiveWorkforceBindingDefersExactExecutionCredentialUntilActivation(t *testing.T) {
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
	}}
	definition.Authority.AllowedSkillIDs = []string{"posture"}
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"posture": {
			ID: "posture", Version: "1.0.0", Actions: []string{"execute"},
			MaximumRisk: capability.RiskLevelExternal,
			Credentials: []authoring.SkillCredential{{
				Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
			}},
		},
	}}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"posture": capability.NewSkillIdentity("posture", "1.0.0", "")},
	}

	inactive, err := materializeWorkforceSkillBindings(value, definition, "agent-live", false)
	if err != nil || len(inactive) != 1 || !inactive[0].Disabled || len(inactive[0].Credentials) != 0 {
		t.Fatalf("inactive deferred binding=%#v error=%v", inactive, err)
	}
	if _, err = materializeWorkforceSkillBindings(value, definition, "agent-live", true); err == nil ||
		!strings.Contains(err.Error(), "TOOLWEB_API_KEY") {
		t.Fatalf("active missing exact credential error=%v", err)
	}

	reference := capability.CredentialReference{Kind: "environment-secret", ID: "credential://toolweb"}
	value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		definition.ID: {"TOOLWEB_API_KEY": reference},
	}
	active, err := materializeWorkforceSkillBindings(value, definition, "agent-live", true)
	if err != nil || len(active) != 1 || active[0].Disabled ||
		active[0].Credentials["TOOLWEB_API_KEY"] != reference {
		t.Fatalf("active exact credential binding=%#v error=%v", active, err)
	}
}

func TestWorkforceReadinessAcceptsOnlyExactReviewedSkillInstallation(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	value := testApplicableWorkforceChangeSet()
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
	}}
	definition.Authority.AllowedSkillIDs = []string{"posture"}
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	const (
		source         = "https://clawhub.ai::posture"
		runtimeVersion = "1.0.0+source.abc"
		reference      = "listing:42"
		sourceDigest   = "sha256:posture"
	)
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"posture": {
			ID: "posture", Version: "1.0.0", SourceIdentity: source,
			Actions: []string{"execute"}, MaximumRisk: capability.RiskLevelExternal,
			Readiness: authoring.SkillReadinessNeedsInstallation,
			Compatibility: []authoring.SkillCompatibility{{
				Requirement: "installation", Compatible: false, Reference: reference,
			}, {
				Requirement: "source_digest", Compatible: true, Reference: sourceDigest,
			}},
		},
	}}
	identity := capability.NewSkillIdentity("posture", runtimeVersion, source)
	value.Placement.SkillSourceIdentities = map[string]map[string]string{
		definition.ID: {"posture": source},
	}
	value.Placement.SkillSourceVersions = map[string]map[string]string{
		definition.ID: {"posture": runtimeVersion},
	}
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"posture": identity},
	}
	value.Placement.PlannedSkillInstallations = []authoring.SkillInstallationIntent{{
		SkillID: "posture", Version: "1.0.0", SourceIdentity: source, SourceDigest: sourceDigest, Reference: reference,
	}}

	issues, err := store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil || len(issues) != 0 {
		t.Fatalf("exact reviewed installation readiness=%#v error=%v", issues, err)
	}
	value.Placement.PlannedSkillInstallations[0].Reference = "listing:forged"
	issues, err = store.ValidateChangeSetReadiness(context.Background(), value)
	if err != nil || len(issues) != 1 || issues[0].Code != "skill_binding_definition_unavailable" {
		t.Fatalf("forged installation readiness=%#v error=%v", issues, err)
	}
}

func TestWorkforceReadinessDefersInstalledSkillCredentialUntilActivation(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const source = "https://clawhub.ai::posture"
	installed := &skill.Definition{
		ID: "posture", Version: "1.0.0", Name: "Posture",
		Source:    &skill.SourceProvenance{Identity: source, Format: "openclaw.skill.v1"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: "execute"},
		Actions: map[string]skill.Action{"execute": {
			Name: "execute", Description: "Inspect posture", Risk: skill.RiskLevelExternal,
			SideEffect: skill.SideEffectExternal, InputSchema: map[string]interface{}{"type": "object"},
			Credentials: []skill.CredentialRequirement{{Name: "TOOLWEB_API_KEY", Kind: "environment-secret"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported,
		}},
	}
	if err := skill.NewCatalogWithStore(store).Register(ctx, installed); err != nil {
		t.Fatal(err)
	}

	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	definition := value.Result.Candidate.Agents[0]
	definition.SkillRequirements = []agent.SkillRequirement{{
		SkillID: "posture", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"},
	}}
	definition.Authority.AllowedSkillIDs = []string{"posture"}
	definition.Authority.MaximumRisk = capability.RiskLevelExternal
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"posture": {
			ID: "posture", Version: "1.0.0", SourceIdentity: source,
			Actions: []string{"execute"}, MaximumRisk: capability.RiskLevelExternal,
			Credentials: []authoring.SkillCredential{{
				Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
			}},
		},
	}}
	identity := capability.NewSkillIdentity(installed.ID, installed.Version, source)
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		definition.ID: {"posture": identity},
	}

	issues, err := store.ValidateChangeSetReadiness(ctx, value)
	if err != nil || len(issues) != 0 {
		t.Fatalf("inactive deferred readiness=%#v error=%v", issues, err)
	}
	value.Result.Candidate.Activation = authoring.WorkforceActivationActive
	issues, err = store.ValidateChangeSetReadiness(ctx, value)
	if err != nil || len(issues) != 1 || issues[0].Code != "skill_binding_materialization_failed" ||
		!strings.Contains(issues[0].Message, "TOOLWEB_API_KEY") {
		t.Fatalf("active exact credential readiness=%#v error=%v", issues, err)
	}
}

func TestSkillBindingStoresRejectMalformedAuthority(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	binding := &skill.Binding{
		ID: "invalid", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: "skill-slack", SkillVersion: "1.0.0", MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}
	if err := store.SaveSkillBinding(context.Background(), binding, 0); err == nil ||
		!strings.Contains(err.Error(), "must enable a prompt or explicitly allow actions") {
		t.Fatalf("save malformed binding = %v", err)
	}
}

func TestSQLiteWorkforceEvaluationValidatesExactSkillAuthorityBeforeReady(t *testing.T) {
	for _, test := range []struct {
		name        string
		definition  *skill.Definition
		catalogID   string
		action      string
		catalogRisk capability.RiskLevel
		credentials []authoring.SkillCredential
		references  map[string]capability.CredentialReference
		wantStatus  authoring.ChangeSetStatus
		wantCode    string
	}{
		{
			name:      "external summarize action is blocked by read-only Agent authority",
			catalogID: "clawhub-summarize", action: "execute", catalogRisk: capability.RiskLevelExternal,
			definition: &skill.Definition{
				ID: "summarize", Version: "1.0.0+source.aaaa", Name: "Summarize",
				Source:    &skill.SourceProvenance{Identity: "https://clawhub.ai::@alice/summarize", Format: "openclaw.skill.v1"},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: "summarize"},
				Actions:   map[string]skill.Action{"execute": {Name: "execute", Description: "Summarize evidence", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, InputSchema: map[string]interface{}{"type": "object"}, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported}},
			},
			wantStatus: authoring.ChangeSetBlocked, wantCode: "skill_binding_action_risk_exceeded",
		},
		{
			name:      "read-only Reddit action reaches ready",
			catalogID: "clawhub-reddit", action: "read", catalogRisk: capability.RiskLevelRead,
			definition: &skill.Definition{
				ID: "reddit.reader", Version: "2.0.0+source.bbbb", Name: "Reddit Reader",
				Source:    &skill.SourceProvenance{Identity: "https://clawhub.ai::@alice/reddit", Format: "openclaw.skill.v1"},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: "reddit_read"},
				Actions:   map[string]skill.Action{"read": {Name: "read", Description: "Read Reddit posts", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, InputSchema: map[string]interface{}{"type": "object"}, Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}}, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported}},
			},
			credentials: []authoring.SkillCredential{{Name: "reddit", Kind: "reddit-oauth", Actions: []string{"read"}}},
			references:  map[string]capability.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://tenant/reddit"}},
			wantStatus:  authoring.ChangeSetReady,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := skill.NewCatalogWithStore(store).Register(ctx, test.definition); err != nil {
				t.Fatal(err)
			}
			value := testApplicableWorkforceChangeSet()
			value.Status, value.Revision = authoring.ChangeSetReview, 1
			value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: test.catalogID, VersionConstraint: "1.0.0", RequiredActions: []string{test.action}}}
			value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{test.catalogID}
			value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
				test.catalogID: {ID: test.catalogID, Version: "1.0.0", Actions: []string{test.action}, MaximumRisk: test.catalogRisk, Credentials: test.credentials},
			}}
			identity := capability.NewSkillIdentity(test.definition.ID, test.definition.Version, test.definition.Source.Identity)
			value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{"agent": {test.catalogID: identity}}
			if test.references != nil {
				value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{"agent": test.references}
			}
			if _, _, err := store.CreateChangeSet(ctx, value, "create-"+test.name, "digest"); err != nil {
				t.Fatal(err)
			}
			compiler, err := authoring.NewCompiler(&countedWorkforceGenerator{})
			if err != nil {
				t.Fatal(err)
			}
			service, err := authoring.NewChangeSetService(compiler, store)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := service.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
				Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: value.Revision, CandidateDigest: value.CandidateDigest,
				Allowed: true, Actor: authoring.ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow",
			})
			if err != nil || result.Status != test.wantStatus {
				t.Fatalf("evaluation = %#v, err = %v", result, err)
			}
			if test.wantCode == "" {
				if len(result.Result.Validation) != 0 || !result.Result.Valid {
					t.Fatalf("valid Reddit readiness = %#v", result.Result)
				}
				return
			}
			if len(result.Result.Validation) != 1 || result.Result.Validation[0].Code != test.wantCode ||
				!strings.Contains(result.Result.Validation[0].Message, "requires external risk") || result.Result.Valid ||
				result.Lifecycle[len(result.Lifecycle)-1].Reason != "binding_validation_failed" {
				t.Fatalf("blocked readiness = %#v", result)
			}
		})
	}
}

func TestSQLiteWorkforceApplyRequiresExactSourceForCollidingSkills(t *testing.T) {
	for _, test := range []struct {
		name           string
		sourceIdentity string
		wantAmbiguous  bool
	}{
		{name: "exact", sourceIdentity: "clawhub::@alice/research"},
		{name: "ambiguous", wantAmbiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			catalog := skill.NewCatalogWithStore(store)
			for _, identity := range []string{"clawhub::@alice/research", "clawhub::@bob/research"} {
				if err := catalog.Register(ctx, &skill.Definition{
					ID: "research", Version: "1.0.0", Name: "Research",
					Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1"},
					Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
				}); err != nil {
					t.Fatal(err)
				}
			}
			value := testApplicableWorkforceChangeSet()
			value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
			value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
			value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
				"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
			}}
			if test.sourceIdentity != "" {
				value.Placement.SkillSourceIdentities = map[string]map[string]string{"agent": {"research": test.sourceIdentity}}
				value.Placement.SkillSourceVersions = map[string]map[string]string{"agent": {"research": "1.0.0"}}
			}
			if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
				t.Fatal(err)
			}
			applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
			result, err := store.ApplyChangeSet(ctx, applied, 2)
			if test.wantAmbiguous {
				if !errors.Is(err, skill.ErrDefinitionAmbiguous) {
					t.Fatalf("ambiguous apply = %#v, %v", result, err)
				}
				if _, err := store.GetDefinition(ctx, "agent", "1"); !errors.Is(err, agent.ErrDefinitionNotFound) {
					t.Fatalf("ambiguous apply leaked Agent state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := store.ListSkillBindings(ctx, value.Scope, "agent-live")
			if err != nil || len(bindings) != 1 || bindings[0].SourceIdentity != test.sourceIdentity {
				t.Fatalf("exact workforce binding = %#v, %v", bindings, err)
			}
			prompts, err := catalog.ListModelPrompts(ctx, value.Scope, "agent-live")
			if err != nil || len(prompts) != 1 || prompts[0].BindingID != bindings[0].ID {
				t.Fatalf("exact model prompts = %#v, %v", prompts, err)
			}
			encoded, _ := json.Marshal(prompts)
			if strings.Contains(string(encoded), test.sourceIdentity) {
				t.Fatalf("model prompt leaked source identity: %s", encoded)
			}
		})
	}
}

func TestSQLiteWorkforceReadinessBlocksExistingAgentDeploymentIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	occupier := testApplicableWorkforceChangeSet()
	occupier.ID = "occupier"
	if _, _, err = store.CreateChangeSet(ctx, occupier, "occupier", "occupier"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(occupier, "occupier-receipt", "occupier-apply", occupier.UpdatedAt.Add(time.Minute)),
		occupier.Revision,
	); err != nil {
		t.Fatal(err)
	}

	candidate := testAgentDeploymentIdentityCollisionChangeSet("collision-review")
	candidate.Status, candidate.Revision = authoring.ChangeSetReview, 1
	if _, _, err = store.CreateChangeSet(ctx, candidate, "collision-review", "collision-review"); err != nil {
		t.Fatal(err)
	}
	compiler, err := authoring.NewCompiler(&countedWorkforceGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := service.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
		Scope: candidate.Scope, ChangeSetID: candidate.ID, ExpectedRevision: candidate.Revision,
		CandidateDigest: candidate.CandidateDigest, Allowed: true,
		Actor: authoring.ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow-collision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != authoring.ChangeSetBlocked || result.Result.Valid || len(result.Result.Validation) != 1 {
		t.Fatalf("colliding readiness result = %#v", result)
	}
	issue := result.Result.Validation[0]
	if issue.Code != "agent_deployment_identity_conflict" ||
		issue.Path != "placement.agentDeploymentIds.agent-collision" ||
		!strings.Contains(issue.Message, `"Analyst Agent"`) ||
		!strings.Contains(issue.Message, `"agent-live"`) ||
		!strings.Contains(issue.Message, "amend the existing Agent") {
		t.Fatalf("colliding readiness issue = %#v", issue)
	}
}

func TestSQLiteAtomicWorkforceApplyMapsAgentDeploymentIdentityRaceWithoutPartialState(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	candidate := testAgentDeploymentIdentityCollisionChangeSet("collision-race")
	if _, _, err = store.CreateChangeSet(ctx, candidate, "collision-race", "collision-race"); err != nil {
		t.Fatal(err)
	}
	occupier := testApplicableWorkforceChangeSet()
	occupier.ID = "race-occupier"
	if _, _, err = store.CreateChangeSet(ctx, occupier, "race-occupier", "race-occupier"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(occupier, "occupier-receipt", "occupier-apply", occupier.UpdatedAt.Add(time.Minute)),
		occupier.Revision,
	); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyChangeSet(
		ctx,
		appliedRuntimeChangeSet(candidate, "collision-receipt", "collision-apply", candidate.UpdatedAt.Add(2*time.Minute)),
		candidate.Revision,
	)
	if !errors.Is(err, authoring.ErrChangeSetPlacementConflict) {
		t.Fatalf("colliding apply error = %v", err)
	}
	if message := strings.ToLower(err.Error()); strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "agent_deployments_pkey") ||
		strings.Contains(message, "sql") {
		t.Fatalf("colliding apply leaked storage detail: %v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent-collision", "1"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("colliding apply leaked Agent definition: %v", err)
	}
	if _, err = store.GetTeamDefinition(ctx, "team-collision-race", "1"); !errors.Is(err, team.ErrDefinitionNotFound) {
		t.Fatalf("colliding apply leaked Team definition: %v", err)
	}
	current, err := store.GetChangeSet(ctx, candidate.Scope, candidate.ID)
	if err != nil || current.Status != authoring.ChangeSetReady || current.ApplyReceipt != nil {
		t.Fatalf("colliding ChangeSet = %#v, err = %v", current, err)
	}
}

func TestSQLiteWorkforceApplyBindsImmutableSourceVersionBehindDeclaredContract(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	const (
		identity         = "https://clawhub.ai::@alice/research"
		immutableVersion = "1.0.0+source.0123456789ab"
	)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "research", Version: immutableVersion, Name: "Research",
		Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1", ResolvedVersion: "1.0.0"},
		Prompt: &skill.PromptModule{Instructions: "Preserve immutable evidence."},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", VersionConstraint: "1.0.0", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
	}}
	value.Placement.SkillSourceIdentities = map[string]map[string]string{"agent": {"research": identity}}
	value.Placement.SkillSourceVersions = map[string]map[string]string{"agent": {"research": immutableVersion}}
	if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
	if _, err := store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListSkillBindings(ctx, value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].SkillVersion != immutableVersion || bindings[0].SourceIdentity != identity {
		t.Fatalf("immutable source binding = %#v, %v", bindings, err)
	}
	prompts, err := catalog.ListModelPrompts(ctx, value.Scope, "agent-live")
	if err != nil || len(prompts) != 1 || prompts[0].Version != immutableVersion {
		t.Fatalf("immutable source prompt = %#v, %v", prompts, err)
	}
}

func TestSQLiteWorkforceApplyResolvesCatalogAliasIntoExactTeamGrantAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const (
		catalogID        = "clawhub-aHR0cHM6Ly9jbGF3aHViLmFpL0BhbGljZS9zdW1tYXJpemU"
		definitionID     = "summarize"
		immutableVersion = "1.0.0+source.0123456789ab"
		sourceIdentity   = "https://clawhub.ai::@alice/summarize"
	)
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: definitionID, Version: immutableVersion, Name: "Summarize",
		Source:    &skill.SourceProvenance{Identity: sourceIdentity, Format: "openclaw.skill.v1", ResolvedVersion: "1.0.0"},
		Prompt:    &skill.PromptModule{Instructions: "Summarize evidence without changing it."},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: "process"},
		Actions: map[string]skill.Action{"execute": {
			Name: "execute", Description: "Summarize evidence", InputSchema: map[string]interface{}{"type": "object"},
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: catalogID, VersionConstraint: "1.0.0", RequiredActions: []string{"execute"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{catalogID}
	value.Result.Candidate.Team.Roles[0].RequiredSkillIDs = []string{catalogID}
	value.Result.Candidate.Team.Roles[0].SkillGrants = []team.RoleSkillGrant{{
		SkillID: catalogID, SkillVersion: "1.0.0", AllowedActions: []string{"execute"}, EnablePrompt: true, MaximumRisk: capability.RiskLevelRead,
	}}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		catalogID: {ID: catalogID, Version: "1.0.0", Actions: []string{"execute"}, PromptAvailable: true, MaximumRisk: capability.RiskLevelRead},
	}}
	identity := capability.NewSkillIdentity(definitionID, immutableVersion, sourceIdentity)
	value.Placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{"agent": {catalogID: identity}}
	if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
	if _, err := store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	agents := agent.NewRegistryWithStore(restored)
	teams := team.NewRegistryWithStore(restored, agents)
	storedAgent, err := agents.GetDefinition(ctx, "agent", "1")
	if err != nil || len(storedAgent.SkillRequirements) != 1 || storedAgent.SkillRequirements[0].SkillID != definitionID ||
		len(storedAgent.Authority.AllowedSkillIDs) != 1 || storedAgent.Authority.AllowedSkillIDs[0] != definitionID {
		t.Fatalf("resolved Agent definition = %#v, %v", storedAgent, err)
	}
	storedTeam, err := teams.GetDefinition(ctx, "team", "1")
	if err != nil || len(storedTeam.Roles[0].SkillGrants) != 1 {
		t.Fatalf("resolved Team definition = %#v, %v", storedTeam, err)
	}
	grant := storedTeam.Roles[0].SkillGrants[0]
	if grant.CatalogID != catalogID || !grant.ExactIdentity().Equal(identity) || storedTeam.Roles[0].RequiredSkillIDs[0] != definitionID {
		t.Fatalf("exact persisted Team grant = %#v", grant)
	}
	bindings, err := restored.ListSkillBindings(ctx, value.Scope, "agent-live")
	if err != nil || len(bindings) != 1 || bindings[0].EnablePrompt || !capability.NewSkillIdentity(bindings[0].SkillID, bindings[0].SkillVersion, bindings[0].SourceIdentity).Equal(identity) {
		t.Fatalf("exact persisted binding = %#v, %v", bindings, err)
	}
	teamBindings, err := restored.ListSkillBindings(ctx, value.Scope, "team-live")
	if err != nil || len(teamBindings) != 1 || teamBindings[0].DeploymentID != "team-live" ||
		!capability.NewSkillIdentity(teamBindings[0].SkillID, teamBindings[0].SkillVersion, teamBindings[0].SourceIdentity).Equal(identity) ||
		len(teamBindings[0].AllowedActions) != 1 || teamBindings[0].AllowedActions[0] != "execute" || !teamBindings[0].EnablePrompt {
		t.Fatalf("exact persisted Team binding = %#v, %v", teamBindings, err)
	}
	foundTeamBinding := false
	for _, resource := range applied.ApplyReceipt.Resources {
		foundTeamBinding = foundTeamBinding || resource.Kind == "skill_binding" && resource.ID == "workforce:team-live:"+catalogID
	}
	if !foundTeamBinding {
		t.Fatalf("Team binding missing from apply receipt: %#v", applied.ApplyReceipt.Resources)
	}
}

func TestSQLiteAtomicWorkforceApplyMaterializesProjectAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registerProjectSourceSkill(t, store)
	value := testProjectWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, value, "create-project", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-project", IdempotencyKey: "apply-project", CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil || len(result.ApplyReceipt.Resources) != 9 || result.ApplyReceipt.Activation != authoring.WorkforceActivationActive {
		t.Fatalf("apply result=%#v err=%v", result, err)
	}
	project, err := store.GetProject(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.ProjectID)
	if err != nil || project.Status != ProjectStatusActive || project.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-live"}) || project.Revision != 1 ||
		len(project.ObjectiveRefs) != 1 || project.ObjectiveRefs[0] != "objective:team" || len(project.SourceMonitors) != 1 ||
		project.SourceMonitors[0].AssignedAgentID != "agent-live" || project.SourceMonitors[0].ObjectiveID != "objective:team" ||
		len(project.Milestones) != 1 || len(project.Hypotheses) != 1 || len(project.Deliverables) != 1 {
		t.Fatalf("Project=%#v err=%v", project, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: project.Scope})
	if err != nil {
		t.Fatal(err)
	}
	var monitorObjective *Objective
	for _, objective := range objectives {
		if objective.ID == "objective:team" {
			monitorObjective = objective
		}
	}
	if monitorObjective == nil {
		t.Fatalf("materialized monitor Objective must contain only its outcome: %#v", monitorObjective)
	}
	runbooks, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: project.Scope, ObjectiveID: monitorObjective.ID})
	if err != nil || len(runbooks) != 1 || runbooks[0].AssignedAgentID != "agent-live" || runbooks[0].Input["projectId"] != project.ID || runbooks[0].Input["sourceMonitorId"] != "community-listening" {
		t.Fatalf("materialized monitor Runbook=%#v err=%v", runbooks, err)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "project" && resource.ID == project.ID && resource.Revision == 1
	}
	if !found {
		t.Fatalf("Project missing from receipt: %#v", result.ApplyReceipt.Resources)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetProject(ctx, project.Scope, project.ID)
	if err != nil || restored.Revision != 1 || restored.SourceMonitors[0] != project.SourceMonitors[0] || restored.CreationFingerprint == "" || restored.IdempotencyKeyHash == "" {
		t.Fatalf("restored Project=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceApplyHonorsInactiveCommitmentWithoutScheduling(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registerProjectSourceSkill(t, store)
	value := testProjectWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	value.Result.Commitments.Activation = authoring.ActivationCommitmentInactive
	value.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		"agent": {"MODEL_PROVIDER": {Kind: "credential", ID: "model-one"}},
	}
	if _, _, err = store.CreateChangeSet(ctx, value, "create-inactive", "digest-inactive"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-inactive", IdempotencyKey: "apply-inactive", CandidateDigest: value.CandidateDigest, Activation: authoring.WorkforceActivationInactive, Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil || result.ApplyReceipt.Activation != authoring.WorkforceActivationInactive {
		t.Fatalf("inactive apply result=%#v err=%v", result, err)
	}
	agents := agent.NewRegistryWithStore(store)
	teams := team.NewRegistryWithStore(store, agents)
	agentDeployment, err := agents.GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutPending || agentDeployment.Activation == nil ||
		agentDeployment.Activation.ChangeSetID != value.ID {
		t.Fatalf("inactive Agent deployment=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := teams.GetDeployment(ctx, value.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentDraft || teamDeployment.Activation == nil ||
		teamDeployment.Activation.ChangeSetID != value.ID {
		t.Fatalf("inactive Team deployment=%#v err=%v", teamDeployment, err)
	}
	if activations, err := agents.ListActivations(ctx, value.Scope, agentDeployment.ID); err != nil || len(activations) != 0 {
		t.Fatalf("inactive Agent activations=%#v err=%v", activations, err)
	}
	if activations, err := teams.ListActivations(ctx, value.Scope, teamDeployment.ID); err != nil || len(activations) != 0 {
		t.Fatalf("inactive Team activations=%#v err=%v", activations, err)
	}
	bindings, err := store.ListSkillBindings(ctx, value.Scope, agentDeployment.ID)
	if err != nil || len(bindings) != 1 || !bindings[0].Disabled {
		t.Fatalf("inactive Skill bindings=%#v err=%v", bindings, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("inactive Objectives=%#v err=%v", objectives, err)
	}
	for _, objective := range objectives {
		if objective.Status != ObjectiveStatusDraft {
			t.Fatalf("Objective %s status=%s", objective.ID, objective.Status)
		}
	}
	project, err := store.GetProject(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.ProjectID)
	if err != nil || project.Status != ProjectStatusDraft {
		t.Fatalf("inactive Project=%#v err=%v", project, err)
	}
	runbooks, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: project.Scope, ObjectiveID: "objective:team"})
	if err != nil || len(runbooks) != 1 || runbooks[0].Status != RunbookActivationPaused {
		t.Fatalf("inactive Runbooks=%#v err=%v", runbooks, err)
	}
	schedule, err := NewRunbookScheduler(store).ReconcileScope(ctx, project.Scope, 10)
	if err != nil || schedule.Examined != 0 || schedule.Scheduled != 0 {
		t.Fatalf("inactive schedule=%#v err=%v", schedule, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: project.Scope})
	if err != nil || len(runs) != 0 {
		t.Fatalf("inactive Runs=%#v err=%v", runs, err)
	}

	compiler, err := authoring.NewCompiler(testAuthoringGenerator(t))
	if err != nil {
		t.Fatal(err)
	}
	changeSets, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog := result.Catalog
	catalog.SourcePolicies = map[string]authoring.SourcePolicyCapability{
		"approved-communities": {
			Reference: "approved-communities",
			Sources:   []authoring.SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5,
		},
	}
	catalog.AgentCredentialRequirements = []authoring.AgentCredentialRequirement{{
		BindingKey: "MODEL_PROVIDER", DisplayName: "Model provider",
		Prompt: "Choose the model provider this Agent may use.", RequiredForActivation: true,
	}}
	activation, _, err := changeSets.PrepareActivation(ctx, authoring.PrepareChangeSetActivationRequest{
		Scope: result.Scope, ChangeSetID: result.ID, ExpectedRevision: result.Revision, CandidateDigest: result.CandidateDigest,
		Catalog: catalog, Reason: "Start the reviewed workforce", Actor: result.Actor, IdempotencyKey: "prepare-activation",
	})
	if err != nil || activation.Status != authoring.ChangeSetReview ||
		activation.Placement.AgentExpectedRevisions["agent"] != agentDeployment.Revision ||
		activation.Placement.TeamExpectedRevision != teamDeployment.Revision {
		t.Fatalf("activation ChangeSet=%#v err=%v", activation, err)
	}
	reviewed, _, err := changeSets.SubmitEvaluation(ctx, authoring.SubmitChangeSetEvaluationRequest{
		Scope: activation.Scope, ChangeSetID: activation.ID, ExpectedRevision: activation.Revision, CandidateDigest: activation.CandidateDigest,
		Allowed: true, Actor: authoring.ChangeSetActor{Type: "policy_evaluator", ID: "test"}, IdempotencyKey: "evaluate-activation",
	})
	if err != nil || reviewed.Status != authoring.ChangeSetReady {
		t.Fatalf("reviewed activation=%#v err=%v", reviewed, err)
	}
	activated, _, err := changeSets.Apply(ctx, authoring.ApplyChangeSetRequest{
		Scope: reviewed.Scope, ChangeSetID: reviewed.ID, ExpectedRevision: reviewed.Revision, CandidateDigest: reviewed.CandidateDigest,
		Reason: "Activate reviewed workforce", Actor: reviewed.Actor, IdempotencyKey: "apply-activation",
	})
	if err != nil || activated.ApplyReceipt == nil || activated.ApplyReceipt.Activation != authoring.WorkforceActivationActive {
		t.Fatalf("activated ChangeSet=%#v err=%v", activated, err)
	}
	agentDeployment, err = agents.GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutActive || agentDeployment.Revision != 2 ||
		agentDeployment.Credentials["MODEL_PROVIDER"].ID != "model-one" || agentDeployment.Activation != nil {
		t.Fatalf("activated Agent=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err = teams.GetDeployment(ctx, value.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentActive || teamDeployment.Revision != 2 || teamDeployment.Activation != nil {
		t.Fatalf("activated Team=%#v err=%v", teamDeployment, err)
	}
	bindings, err = store.ListSkillBindings(ctx, value.Scope, agentDeployment.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Disabled {
		t.Fatalf("activated Skill bindings=%#v err=%v", bindings, err)
	}
	objectives, err = store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("activated Objectives=%#v err=%v", objectives, err)
	}
	for _, objective := range objectives {
		if objective.Status != ObjectiveStatusActive || objective.Revision != 2 {
			t.Fatalf("activated Objective %s=%#v", objective.ID, objective)
		}
	}
	project, err = store.GetProject(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.ProjectID)
	if err != nil || project.Status != ProjectStatusActive || project.Revision != 2 {
		t.Fatalf("activated Project=%#v err=%v", project, err)
	}
}

func TestSQLiteStartupRecoversLegacyWorkforceActivationContinuations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registerProjectSourceSkill(t, store)
	value := testProjectWorkforceChangeSet()
	value.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	value.Result.Commitments.Activation = authoring.ActivationCommitmentInactive
	if _, _, err = store.CreateChangeSet(ctx, value, "create-legacy-inactive", "digest-legacy-inactive"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{
		ID: "receipt-legacy-inactive", IdempotencyKey: "apply-legacy-inactive",
		CandidateDigest: value.CandidateDigest, Activation: authoring.WorkforceActivationInactive,
		Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute),
	}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		if _, err = store.db.Exec(`UPDATE ` + table + ` SET payload=json_remove(payload, '$.activation')`); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	agentDeployment, err := agent.NewRegistryWithStore(restarted).GetDeployment(ctx, value.Scope, "agent-live")
	if err != nil || agentDeployment.Activation == nil || agentDeployment.Activation.ChangeSetID != value.ID {
		t.Fatalf("recovered Agent continuation=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := team.NewRegistryWithStore(restarted, agent.NewRegistryWithStore(restarted)).GetDeployment(
		ctx, value.Scope, "team-live",
	)
	if err != nil || teamDeployment.Activation == nil || teamDeployment.Activation.ChangeSetID != value.ID {
		t.Fatalf("recovered Team continuation=%#v err=%v", teamDeployment, err)
	}
}

func TestSQLiteAtomicWorkforceProjectAmendUsesCASWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registerProjectSourceSkill(t, store)
	created := testProjectWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-project", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	stale := projectAmendChangeSet(created, "amend-stale", 99)
	if _, _, err = store.CreateChangeSet(ctx, stale, "amend-stale", "amend-stale"); err != nil {
		t.Fatal(err)
	}
	staleApply := appliedRuntimeChangeSet(stale, "receipt-stale", "apply-stale", first.UpdatedAt.Add(time.Minute))
	if _, err = store.ApplyChangeSet(ctx, staleApply, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale Project apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("stale Project apply leaked Agent definition: %v", err)
	}
	current, err := store.GetProject(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.ProjectID)
	if err != nil || current.Revision != 1 || current.Title != "Research program" {
		t.Fatalf("Project after stale apply=%#v err=%v", current, err)
	}

	amend := projectAmendChangeSet(created, "amend-valid", 1)
	amend.Result.Candidate.Activation = authoring.WorkforceActivationInactive
	amend.Result.Commitments.Activation = authoring.ActivationCommitmentInactive
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend-valid", "amend-valid"); err != nil {
		t.Fatal(err)
	}
	validApply := appliedRuntimeChangeSet(amend, "receipt-amend", "apply-amend", first.UpdatedAt.Add(2*time.Minute))
	if _, err = store.ApplyChangeSet(ctx, validApply, 2); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetProject(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.ProjectID)
	if err != nil || updated.Revision != 2 || updated.Title != "Research program v2" || updated.Status != ProjectStatusDraft || !updated.CreatedAt.Equal(current.CreatedAt) || updated.IdempotencyKeyHash != current.IdempotencyKeyHash {
		t.Fatalf("amended Project=%#v err=%v", updated, err)
	}
	agents := agent.NewRegistryWithStore(store)
	teams := team.NewRegistryWithStore(store, agents)
	agentDeployment, err := agents.GetDeployment(ctx, created.Scope, "agent-live")
	if err != nil || agentDeployment.RolloutStatus != agent.RolloutPaused || agentDeployment.Revision != 2 {
		t.Fatalf("inactive amended Agent=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := teams.GetDeployment(ctx, created.Scope, "team-live")
	if err != nil || teamDeployment.Status != team.DeploymentPaused || teamDeployment.Revision != 2 {
		t.Fatalf("inactive amended Team=%#v err=%v", teamDeployment, err)
	}
	if activations, err := agents.ListActivations(ctx, created.Scope, agentDeployment.ID); err != nil || len(activations) != 1 {
		t.Fatalf("inactive amended Agent activations=%#v err=%v", activations, err)
	}
	if activations, err := teams.ListActivations(ctx, created.Scope, teamDeployment.ID); err != nil || len(activations) != 1 {
		t.Fatalf("inactive amended Team activations=%#v err=%v", activations, err)
	}
}

func TestSQLiteAtomicWorkforceApplyConcurrentRetryHasOneReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	ctx := context.Background()
	ready := testApplicableWorkforceChangeSet()
	if _, _, err = primary.CreateChangeSet(ctx, ready, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	candidate := cloneRuntimeChangeSet(ready)
	candidate.Status = authoring.ChangeSetApplied
	candidate.Revision = 3
	candidate.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: ready.CandidateDigest, Actor: ready.Actor, AppliedAt: ready.UpdatedAt.Add(time.Minute)}
	candidate.UpdatedAt = candidate.ApplyReceipt.AppliedAt
	stores := []*SQLiteStore{primary, replica}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *SQLiteStore) {
			defer wg.Done()
			_, err := store.ApplyChangeSet(ctx, cloneRuntimeChangeSet(candidate), 2)
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	restored, err := primary.GetChangeSet(ctx, ready.Scope, ready.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceAmendRejectsStaleRevisionWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(created)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 99}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	attempt := cloneRuntimeChangeSet(amend)
	attempt.Status = authoring.ChangeSetApplied
	attempt.Revision = 3
	attempt.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: amend.UpdatedAt.Add(time.Minute)}
	attempt.UpdatedAt = attempt.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, attempt, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("partial Agent definition=%v", err)
	}
	if _, err = store.GetTeamDefinition(ctx, "team", "2"); !errors.Is(err, team.ErrDefinitionNotFound) {
		t.Fatalf("partial Team definition=%v", err)
	}
	current, err := store.GetChangeSet(ctx, amend.Scope, amend.ID)
	if err != nil || current.Status != authoring.ChangeSetReady {
		t.Fatalf("change set=%#v err=%v", current, err)
	}
}

func TestSQLiteAtomicWorkforceAmendActivatesNewVersionsTogether(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status = authoring.ChangeSetApplied
	first.Revision = 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status = authoring.ChangeSetApplied
	second.Revision = 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	agentDeployment, err := store.GetDeployment(ctx, created.Scope, "agent-live")
	if err != nil || agentDeployment.ActiveVersion != "2" || agentDeployment.PreviousVersion != "1" || agentDeployment.Revision != 2 {
		t.Fatalf("Agent deployment=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := store.GetTeamDeployment(ctx, created.Scope, "team-live")
	if err != nil || teamDeployment.ActiveVersion != "2" || teamDeployment.Revision != 2 {
		t.Fatalf("Team deployment=%#v err=%v", teamDeployment, err)
	}
	if versions, err := store.ListDefinitionVersions(ctx, "agent"); err != nil || len(versions) != 2 {
		t.Fatalf("Agent versions=%d err=%v", len(versions), err)
	}
	if versions, err := store.ListTeamDefinitionVersions(ctx, "team"); err != nil || len(versions) != 2 {
		t.Fatalf("Team versions=%d err=%v", len(versions), err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: "tenant", ID: "one"}})
	if err != nil || len(objectives) != 2 || objectives[0].Revision != 2 || objectives[1].Revision != 2 {
		t.Fatalf("objective portfolio=%#v err=%v", objectives, err)
	}
}

func TestSQLiteAtomicWorkforceAmendCreatesUnappliedResourcesAndPreservesUnrelatedBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := skill.NewCatalogWithStore(store)
	if err = catalog.Register(ctx, &skill.Definition{ID: "unrelated", Version: "1", Name: "Unrelated", Prompt: &skill.PromptModule{Instructions: "Remain bound."}}); err != nil {
		t.Fatal(err)
	}
	unrelated := &skill.Binding{ID: "workforce:other-live:unrelated", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "other-live", SkillID: "unrelated", SkillVersion: "1", EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err = catalog.Bind(ctx, unrelated); err != nil {
		t.Fatal(err)
	}

	parent := testApplicableWorkforceChangeSet()
	parent.ID = "rejected-parent"
	parent.Status = authoring.ChangeSetRejected
	if _, _, err = store.CreateChangeSet(ctx, parent, "parent", "parent"); err != nil {
		t.Fatal(err)
	}
	recovered := testApplicableWorkforceChangeSet()
	recovered.ID = "recovered"
	recovered.ParentID = parent.ID
	recovered.Mode = authoring.ModeAmend
	if _, _, err = store.CreateChangeSet(ctx, recovered, "recovered", "recovered"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(recovered)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-recovered", IdempotencyKey: "apply-recovered", CandidateDigest: recovered.CandidateDigest, Actor: recovered.Actor, AppliedAt: recovered.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, recovered.Scope, "agent-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Agent deployment=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetTeamDeployment(ctx, recovered.Scope, "team-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Team deployment=%#v err=%v", deployment, err)
	}
	if prompts, err := catalog.ListModelPrompts(ctx, unrelated.Scope, unrelated.DeploymentID); err != nil || len(prompts) != 1 {
		t.Fatalf("unrelated prompts=%#v err=%v", prompts, err)
	}
	if result.ApplyReceipt == nil || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("receipt=%#v", result.ApplyReceipt)
	}
}

func TestSQLiteAtomicWorkforceAmendSupportsMixedCreateAndUpdatePlacements(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-mixed", "create-mixed"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create-mixed", IdempotencyKey: "apply-create-mixed", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	amend := testApplicableWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = "mixed", created.ID, authoring.ModeAmend, "candidate-mixed"
	newAgent := &agent.AgentDefinition{ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review", SystemPrompt: "Review the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	amend.Result.Candidate.Agents = append(amend.Result.Candidate.Agents, newAgent)
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Team.Roles = append(amend.Result.Candidate.Team.Roles, team.RoleSlot{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"reviewer"}})
	amend.Result.Candidate.Assignments = append(amend.Result.Candidate.Assignments, authoring.Assignment{ID: "reviewer", RoleID: "reviewer", AgentDefinitionID: "reviewer"})
	amend.Placement.AgentDeploymentIDs["reviewer"] = "reviewer-live"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "mixed", "mixed"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status, second.Revision = authoring.ChangeSetApplied, 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-mixed", IdempotencyKey: "apply-mixed", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "agent-live"); err != nil || deployment.Revision != 2 {
		t.Fatalf("updated Agent=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "reviewer-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("new Agent=%#v err=%v", deployment, err)
	}
}

func testApplicableWorkforceChangeSet() *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agentDefinition := &agent.AgentDefinition{ID: "agent", Version: "1", DisplayName: "Agent", Purpose: "Work", SystemPrompt: "Do the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "agent-goal", Title: "Agent goal", Goal: "Finish agent work"}}}
	teamDefinition := &team.Definition{ID: "team", Version: "1", DisplayName: "Team", Purpose: "Work together", Roles: []team.RoleSlot{{ID: "worker", DisplayName: "Worker", Purpose: "Work", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"agent"}}}, Coordination: team.CoordinationPolicy{}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "team-goal", Title: "Team goal", Goal: "Finish team work"}}}
	return &authoring.ChangeSet{ID: "change", Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create", PromptDigest: "prompt", CandidateDigest: "candidate", Result: authoring.CompileResult{Candidate: authoring.WorkforceCandidate{Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition, Assignments: []authoring.Assignment{{ID: "worker", RoleID: "worker", AgentDefinitionID: "agent"}}}, Valid: true}, Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"agent": "agent-live"}, Objectives: map[string]authoring.ObjectivePlacement{authoring.WorkforceObjectiveKey("agent", "agent", "agent-goal"): {ID: "objective:agent"}, authoring.WorkforceObjectiveKey("team", "team", "team-goal"): {ID: "objective:team"}}, Environment: "test"}, Status: authoring.ChangeSetReady, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 2, CreatedAt: now, UpdatedAt: now}
}

func testAgentDeploymentIdentityCollisionChangeSet(id string) *authoring.ChangeSet {
	value := testApplicableWorkforceChangeSet()
	value.ID = id
	value.CandidateDigest = "candidate-" + id
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.ID = "agent-collision"
	agentDefinition.DisplayName = "Analyst Agent"
	teamDefinition := value.Result.Candidate.Team
	teamDefinition.ID = "team-" + id
	teamDefinition.Roles[0].RequiredDefinitionIDs = []string{agentDefinition.ID}
	value.Result.Candidate.Assignments[0].AgentDefinitionID = agentDefinition.ID
	value.Placement.TeamDeploymentID = "team-" + id + "-live"
	value.Placement.AgentDeploymentIDs = map[string]string{agentDefinition.ID: "agent-live"}
	value.Placement.Objectives = map[string]authoring.ObjectivePlacement{
		authoring.WorkforceObjectiveKey("agent", agentDefinition.ID, "agent-goal"): {
			ID: "objective:" + id + ":agent",
		},
		authoring.WorkforceObjectiveKey("team", teamDefinition.ID, "team-goal"): {
			ID: "objective:" + id + ":team",
		},
	}
	return value
}

func testProjectWorkforceChangeSet() *authoring.ChangeSet {
	value := testApplicableWorkforceChangeSet()
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: "community-source", VersionConstraint: "1.2.3", RequiredActions: []string{"observe"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{"community-source"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}, MaximumRisk: capability.RiskLevelRead},
	}}
	teamObjectiveRef := authoring.WorkforceObjectiveKey(authoring.ProjectOwnerTeam, "team", "team-goal")
	agentDefinition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "community-monitor", Version: "1.0.0", Name: "Community monitor",
		Entrypoints: map[string]string{"monitor": "observe"},
		Triggers: map[string]runbook.Trigger{"hourly": {
			Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 */1 * * *", Timezone: "UTC"}, Entrypoint: "monitor",
			ObjectiveID: teamObjectiveRef, MaximumConcurrent: 1, Budget: &runbook.BudgetAllocation{MaxTurns: 2, MaxActions: 1, MaxDurationMS: 60000},
		}},
		Steps: map[string]runbook.Step{
			"observe": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe",
				Arguments:  map[string]runbook.Value{"url": runtimeLiteral("https://community.example/feed"), "maxItems": runtimeLiteral(5), "query": runtimeLiteral("agent runtime pain points")},
				ResultPath: "/results/observe", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	value.Result.Candidate.Project = &authoring.ProjectBlueprint{
		ID: "research-program", Title: "Research program", Purpose: "Continuously understand user pain points",
		Owner:         authoring.ProjectOwnerReference{Type: authoring.ProjectOwnerTeam, DefinitionID: "team"},
		ObjectiveRefs: []string{teamObjectiveRef},
		Milestones:    []authoring.ProjectMilestoneBlueprint{{ID: "baseline", Title: "Establish baseline", ObjectiveRefs: []string{teamObjectiveRef}}},
		Hypotheses:    []authoring.ProjectHypothesisBlueprint{{ID: "setup-friction", Statement: "Setup friction limits adoption", Confidence: 0.5}},
		SourceMonitors: []authoring.ProjectSourceMonitorBlueprint{{
			ID: "community-listening", ObjectiveRef: teamObjectiveRef, AssignedAgentDefinitionID: "agent", SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe",
			SourcePolicyRef: "approved-communities", Deduplication: authoring.ProjectDeduplicateStableSourceAndContent,
		}},
		Deliverables: []authoring.ProjectDeliverableBlueprint{{ID: "cited-report", Title: "Cited report", ObjectiveRefs: []string{teamObjectiveRef}}},
		Policy:       map[string]interface{}{"outreachApproval": "required"},
	}
	value.Placement.ProjectID = "project:research"
	return value
}

func runtimeLiteral(value interface{}) runbook.Value {
	payload, _ := json.Marshal(value)
	return runbook.Value{Literal: payload}
}

func registerProjectSourceSkill(t *testing.T, store skill.CatalogStore) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "community-source", Version: "1.2.3", Name: "Community source", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://community.invalid"},
		Actions: map[string]skill.Action{"observe": {
			Name: "observe", Description: "Observe a permitted community source", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func projectAmendChangeSet(created *authoring.ChangeSet, id string, projectRevision int64) *authoring.ChangeSet {
	amend := testProjectWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = id, created.ID, authoring.ModeAmend, "candidate-"+id
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Project.Title = "Research program v2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	amend.Placement.ProjectExpectedRevision = projectRevision
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	return amend
}

func appliedRuntimeChangeSet(value *authoring.ChangeSet, receiptID, key string, at time.Time) *authoring.ChangeSet {
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision, applied.UpdatedAt = authoring.ChangeSetApplied, 3, at
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: receiptID, IdempotencyKey: key, CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: at}
	return applied
}

func cloneRuntimeChangeSet(value *authoring.ChangeSet) *authoring.ChangeSet {
	payload, _ := json.Marshal(value)
	var result authoring.ChangeSet
	_ = json.Unmarshal(payload, &result)
	return &result
}

func testWorkforceChangeSet(scope capability.ScopeReference, id string) *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &authoring.ChangeSet{
		ID: id, Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create a Team", PromptDigest: "prompt",
		CandidateDigest: "candidate", Result: authoring.CompileResult{Valid: true},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live"}, Status: authoring.ChangeSetReview,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
