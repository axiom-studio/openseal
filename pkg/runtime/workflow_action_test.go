package runtime

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func workflowActionFixture(t *testing.T, store KernelStore, subjectEvidence ...bool) (*AgentRun, *skill.Catalog, *skill.BoundAction, map[string]interface{}) {
	return workflowActionDefinitionFixture(t, store, RunbookManagementSkill(), len(subjectEvidence) > 0 && subjectEvidence[0])
}

func workflowActionDefinitionFixture(t *testing.T, store KernelStore, routinesDefinition *skill.Definition, subjectEvidence bool) (*AgentRun, *skill.Catalog, *skill.BoundAction, map[string]interface{}) {
	t.Helper()
	run, catalog, scheduledBound, _ := scheduledTaskFixture(t, store, routinesDefinition)
	routines := *scheduledBound.Binding
	routines.AllowedActions = append(routines.AllowedActions, RunbookActionCreateWorkflow, RunbookActionWorkflowSources, RunbookActionInspectWorkflows)
	if _, err := catalog.UpsertBinding(t.Context(), skill.UpsertBindingRequest{Binding: &routines, ExpectedRevision: routines.Revision, Actor: skill.BindingActor{Type: "test", ID: "operator"}, Reason: "Enable workflow actions"}); err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{ID: "test-events", Version: "1.0.0", Name: "Test event connector", Actions: map[string]skill.Action{}, ConversationAdapters: map[string]skill.ConversationAdapter{"messages": {
		ProtocolVersion: skill.ConversationAdapterProtocolV1, Name: "Verified messages", Description: "Normalize authenticated provider messages", Provider: "test",
		EndpointModes: []capability.ConversationEndpointMode{capability.ConversationEndpointDirect}, InboundEventTypes: []string{capability.ConversationEventMessageReceived},
		Delivery:  capability.ConversationDeliveryCapabilities{Operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, Ordering: capability.ConversationDeliveryOrderConversation, Idempotency: skill.IdempotencyRequired},
		Transport: skill.ConversationAdapterTransport{Kind: "plugin", IngressEndpoint: "test.ingress", DeliveryEndpoint: "test.deliver"},
	}}}
	if subjectEvidence {
		definition.Transport = skill.TransportReference{Kind: "plugin", Endpoint: "test.actions"}
		for _, name := range []string{"send_message", "read_conversation", "unrelated"} {
			definition.Actions[name] = skill.Action{Name: name, Description: "Return a provider conversation receipt", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
				InputSchema:  map[string]interface{}{"type": "object"},
				OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"receipt": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"conversationId": map[string]interface{}{"type": "string"}}}}},
			}
		}
		adapter := definition.ConversationAdapters["messages"]
		adapter.SubjectEvidence = []capability.ConversationSubjectEvidence{{Action: "read_conversation", SubjectPath: "receipt.conversationId"}, {Action: "send_message", SubjectPath: "receipt.conversationId"}}
		definition.ConversationAdapters["messages"] = adapter
	}
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "event-account", Scope: routines.Scope, DeploymentID: run.AssignedAgentID, SkillID: definition.ID, SkillVersion: definition.Version, Revision: 1, MaximumRisk: skill.RiskLevelRead, EnabledConversationAdapters: []string{"messages"}}
	if subjectEvidence {
		binding.AllowedActions = []string{"send_message", "read_conversation", "unrelated"}
	}
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	gatewayStore, ok := store.(ExternalConversationGatewayStore)
	if !ok {
		t.Fatal("gateway store missing")
	}
	createActiveExternalConversationGateway(t, t.Context(), NewExternalConversationGatewayService(gatewayStore), CreateExternalConversationGatewayRequest{ID: "event-gateway", Name: "Verified installation", Gateway: ExternalConversationIngressGateway{Scope: run.Scope, DeploymentID: run.AssignedAgentID, Provider: "test", InstallationID: "installation-one", ApplicationID: "application-one", Adapter: ExternalConversationAdapterReference{BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "messages", SkillID: definition.ID, SkillVersion: definition.Version}}, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Install reviewed ingress"})
	bound, err := catalog.Resolve(t.Context(), routines.Scope, run.AssignedAgentID, routines.SkillID, routines.SkillVersion, RunbookActionCreateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Collect a reply", "goal": "When the reply arrives, summarize the supplied event and report it to the originating user. On timeout report which response is missing.", "bindingId": binding.ID, "adapterId": "messages", "adapterKind": "conversation", "eventType": capability.ConversationEventMessageReceived, "subject": "direct-conversation", "attributes": map[string]interface{}{"externalParticipantId": "person-one"}, "deadline": run.CreatedAt.Add(time.Hour).Format(time.RFC3339Nano)}
	return run, catalog, bound, args
}

func dispatchWorkflowForTest(t *testing.T, store KernelStore, catalog *skill.Catalog, run *AgentRun, bound *skill.BoundAction, args map[string]interface{}, callID string, teamCatalogs ...WorkflowTeamCatalog) (map[string]interface{}, error) {
	t.Helper()
	d, err := NewRunbookActionDispatcher(store.(runbookActionStore), nil)
	if err != nil {
		t.Fatal(err)
	}
	d.SetWorkflowCatalog(catalog)
	if len(teamCatalogs) > 0 {
		d.SetWorkflowTeams(teamCatalogs[0])
	}
	result, err := d.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: bound, Call: &ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: bound.Binding.DeploymentID, IdempotencyKey: callID}, Arguments: args})
	if err == nil {
		if err := catalog.ValidateOutput(t.Context(), bound, result); err != nil {
			t.Fatalf("output contract: %v", err)
		}
	}
	return result, err
}

func TestWorkflowActionCreatesAnIdempotentWaitingRunAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store KernelStore = NewMemoryStore()
			if kind == "sqlite" {
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "workflows.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				store = s
			}
			run, catalog, bound, args := workflowActionFixture(t, store)
			if err := catalog.ValidateInput(t.Context(), bound, args); err != nil {
				t.Fatal(err)
			}
			validator, _ := NewRunbookActionValidator(store.(runbookActionStore))
			validator.SetWorkflowCatalog(catalog)
			if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err != nil {
				t.Fatal(err)
			}
			result, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "register-follow-up")
			if err != nil {
				t.Fatal(err)
			}
			work := result["run"].(*AgentRun)
			if work.Kind != RunKindAgentWork || work.Status != AgentRunStatusWaitingForEvent || work.AssignedAgentID != run.AssignedAgentID || work.ParentRunID != "" || work.ObjectiveID != "" || work.Context[conversationRunContextConversationID] != run.Context[conversationRunContextConversationID] || work.Context[runReportingContextRootRunID] != work.ID || work.Deadline != nil {
				t.Fatalf("workflow=%#v", work)
			}
			wait, err := store.(RunEventWaitStore).GetRunEventWait(t.Context(), work.Scope, work.ID, "initial")
			if err != nil || wait == nil || wait.Status != RunEventWaitPending {
				t.Fatalf("wait=%#v err=%v", wait, err)
			}
			if wait.Spec.Attributes["installationId"] != "installation-one" || wait.Spec.Attributes["applicationId"] != "application-one" || wait.Spec.Attributes["participantIsBot"] != false {
				t.Fatalf("selectors=%#v", wait.Spec.Attributes)
			}
			replay, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "register-follow-up")
			if err != nil || replay["workflowId"] != work.ID || replay["replayed"] != true {
				t.Fatalf("replay=%#v err=%v", replay, err)
			}
			changed := cloneMap(args)
			changed["subject"] = "other-conversation"
			if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, changed, "register-follow-up"); !errors.Is(err, ErrRunIdempotency) {
				t.Fatalf("changed replay error=%v", err)
			}
			inspection, _ := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionInspectWorkflows)
			status, err := dispatchWorkflowForTest(t, store, catalog, run, inspection, map[string]interface{}{"workflowId": work.ID}, "inspect")
			if err != nil || len(status["workflows"].([]*AgentRun)) != 1 {
				t.Fatalf("inspection=%#v err=%v", status, err)
			}
		})
	}
}

func TestWorkflowActionRejectsForgedSelectorsAndBackgroundRecursion(t *testing.T) {
	store := NewMemoryStore()
	run, catalog, bound, args := workflowActionFixture(t, store)
	validator, _ := NewRunbookActionValidator(store)
	validator.SetWorkflowCatalog(catalog)
	for _, change := range []string{"foreign-source", "foreign-binding", "foreign-scope", "background", "bot-message", "no-correlation", "authority-selector", "unknown-event", "deadline", "unbounded-deadline", "credential", "unknown-input", "missing-message", "wrong-installation"} {
		t.Run(change, func(t *testing.T) {
			candidate := cloneAgentRun(run)
			a := cloneMap(args)
			a["attributes"] = cloneMap(args["attributes"].(map[string]interface{}))
			b := *bound
			binding := *bound.Binding
			b.Binding = &binding
			switch change {
			case "foreign-source":
				a["bindingId"] = "other-agent-account"
			case "foreign-binding":
				binding.DeploymentID = "other-agent"
			case "foreign-scope":
				candidate.Scope.ID = "other-tenant"
			case "background":
				candidate.Kind = RunKindAgentWork
			case "bot-message":
				a["attributes"].(map[string]interface{})["participantIsBot"] = true
			case "no-correlation":
				a["attributes"] = map[string]interface{}{}
			case "authority-selector":
				a["attributes"].(map[string]interface{})["deploymentId"] = "other-agent"
			case "unknown-event":
				a["eventType"] = "gmail.message.received"
			case "deadline":
				a["deadline"] = run.CreatedAt.Add(-time.Second).Format(time.RFC3339Nano)
			case "unbounded-deadline":
				a["deadline"] = run.CreatedAt.Add(31 * 24 * time.Hour).Format(time.RFC3339Nano)
			case "credential":
				a["attributes"].(map[string]interface{})["accessToken"] = "secret-value"
			case "unknown-input":
				a["deploymentId"] = "other-agent"
			case "missing-message":
				candidate.Context[conversationRunContextTriggerID] = "missing"
			case "wrong-installation":
				a["attributes"].(map[string]interface{})["installationId"] = "unrelated-installation"
			}
			if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: candidate, Bound: &b, Arguments: a}); err == nil {
				t.Fatal("invalid workflow was accepted")
			}
		})
	}
}

func TestWorkflowSourceDiscoveryReportsMissingIngressAndCreationRejectsIt(t *testing.T) {
	store := NewMemoryStore()
	run, catalog, bound, args := workflowActionFixture(t, store)
	gateway, _ := store.GetExternalConversationGateway(t.Context(), run.Scope, "event-gateway")
	paused := ExternalConversationGatewayPaused
	if _, err := NewExternalConversationGatewayService(store).Update(t.Context(), run.Scope, gateway.ID, UpdateExternalConversationGatewayRequest{ExpectedRevision: gateway.Revision, Status: &paused, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Pause ingress"}); err != nil {
		t.Fatal(err)
	}
	sourcesBound, _ := catalog.Resolve(t.Context(), bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionWorkflowSources)
	result, err := dispatchWorkflowForTest(t, store, catalog, run, sourcesBound, map[string]interface{}{}, "list-event-sources")
	if err != nil {
		t.Fatal(err)
	}
	sources := result["sources"].([]*WorkflowEventSource)
	if len(sources) != 1 || sources[0].Available || sources[0].UnavailableReason == "" {
		t.Fatalf("sources=%#v", sources)
	}
	if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "not-installed"); err == nil || !strings.Contains(err.Error(), "ingress") {
		t.Fatalf("creation error=%v", err)
	}
}

func TestWorkflowCallbackSourcesUseSubscribedEventsAndDeploymentFilters(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store KernelStore = NewMemoryStore()
			if kind == "sqlite" {
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "callback-workflows.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				store = s
			}
			run, catalog, bound, args := workflowActionFixture(t, store)
			definition := &skill.Definition{ID: "resource-events", Version: "1.0.0", Name: "Resource event service", Actions: map[string]skill.Action{}, CallbackAdapters: map[string]skill.CallbackAdapter{"changes": {ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Resource changes", Description: "Normalize authenticated resource change events", Provider: "test", EventTypes: []string{"resource.changed", "resource.removed"}, Credentials: []skill.CredentialRequirement{}, Transport: skill.CallbackAdapterTransport{Kind: "plugin", IngressEndpoint: "test.resource.ingress"}}}}
			if err := catalog.Register(t.Context(), definition); err != nil {
				t.Fatal(err)
			}
			binding := &skill.Binding{ID: "resource-account", Scope: bound.Binding.Scope, DeploymentID: run.AssignedAgentID, SkillID: definition.ID, SkillVersion: definition.Version, Revision: 1, MaximumRisk: skill.RiskLevelRead, EnabledCallbackAdapters: []string{"changes"}}
			if err := catalog.Bind(t.Context(), binding); err != nil {
				t.Fatal(err)
			}
			callbacks := store.(CallbackRegistrationStore)
			registry := NewCallbackRegistry(callbacks, catalog)
			registration, err := registry.Create(t.Context(), CreateCallbackRegistrationRequest{ID: "resource-registration", Scope: run.Scope, Owner: run.Owner, DeploymentID: run.AssignedAgentID, Name: "Resource subscription", Provider: "test", Adapter: CallbackAdapterReference{SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "changes"}, Subscriptions: []CallbackSubscription{{EventType: "resource.changed", Consumer: "resources"}}, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Install reviewed callback"})
			if err != nil {
				t.Fatal(err)
			}
			active := CallbackRegistrationActive
			registration, err = registry.Update(t.Context(), run.Scope, registration.ID, UpdateCallbackRegistrationRequest{ExpectedRevision: registration.Revision, Status: &active, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Activate verified callback"})
			if err != nil {
				t.Fatal(err)
			}
			foreign := cloneCallbackRegistration(registration)
			foreign.ID = "other-agent-registration"
			foreign.IngressRoute = "other-agent-route"
			foreign.DeploymentID = "other-agent"
			foreign.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}
			if err := callbacks.CreateCallbackRegistration(t.Context(), foreign); err != nil {
				t.Fatal(err)
			}
			values, err := callbacks.ListCallbackRegistrations(t.Context(), CallbackRegistrationFilter{Scope: run.Scope, DeploymentID: run.AssignedAgentID, Statuses: []CallbackRegistrationStatus{CallbackRegistrationActive}, Limit: 10})
			if err != nil || len(values) != 1 || values[0].ID != registration.ID {
				t.Fatalf("deployment filter=%#v err=%v", values, err)
			}
			if kind == "sqlite" {
				s := store.(*SQLiteStore)
				rows, err := s.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT payload FROM callback_registrations WHERE scope_kind=? AND scope_id=? AND json_extract(payload,'$.deploymentId')=? AND status=? ORDER BY updated_at DESC,id ASC LIMIT 100`, run.Scope.Kind, run.Scope.ID, run.AssignedAgentID, CallbackRegistrationActive)
				if err != nil {
					t.Fatal(err)
				}
				indexed := false
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					indexed = indexed || strings.Contains(detail, "idx_callback_registrations_deployment")
				}
				rows.Close()
				if !indexed {
					t.Fatal("deployment callback lookup does not use its index")
				}
			}
			sources, err := workflowSources(t.Context(), store.(runbookActionStore), catalog, run)
			if err != nil {
				t.Fatal(err)
			}
			var callbackSource *WorkflowEventSource
			for _, source := range sources {
				if source.AdapterKind == "callback" {
					callbackSource = source
				}
			}
			if callbackSource == nil || !callbackSource.Available || len(callbackSource.EventTypes) != 1 || callbackSource.EventTypes[0] != "resource.changed" {
				t.Fatalf("callback source=%#v", callbackSource)
			}
			a := cloneMap(args)
			a["bindingId"] = binding.ID
			a["adapterId"] = "changes"
			a["adapterKind"] = "callback"
			a["eventType"] = "resource.changed"
			a["subject"] = "resource-123"
			a["attributes"] = map[string]interface{}{"change": "closed"}
			result, err := dispatchWorkflowForTest(t, store, catalog, run, bound, a, "resource-follow-up")
			if err != nil {
				t.Fatal(err)
			}
			work := result["run"].(*AgentRun)
			if work.WakeCondition.EventWait.Source != RunEventBindingSource(run.AssignedAgentID, binding.ID, "changes") || work.WakeCondition.EventWait.Subject != "resource-123" {
				t.Fatalf("callback wait=%#v", work.WakeCondition)
			}
			a["eventType"] = "resource.removed"
			if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, a, "unsubscribed-resource-event"); err == nil {
				t.Fatal("unsubscribed callback event was accepted")
			}
		})
	}
}

func TestWorkflowCreationReplayRecoversAfterIngressIsPaused(t *testing.T) {
	store := NewMemoryStore()
	run, catalog, bound, args := workflowActionFixture(t, store)
	created, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "accepted-before-pause")
	if err != nil {
		t.Fatal(err)
	}
	gateway, _ := store.GetExternalConversationGateway(t.Context(), run.Scope, "event-gateway")
	paused := ExternalConversationGatewayPaused
	if _, err := NewExternalConversationGatewayService(store).Update(t.Context(), run.Scope, gateway.ID, UpdateExternalConversationGatewayRequest{ExpectedRevision: gateway.Revision, Status: &paused, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Pause source after accepted action"}); err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "accepted-before-pause")
	if err != nil || replayed["workflowId"] != created["workflowId"] || replayed["replayed"] != true {
		t.Fatalf("accepted action recovery=%#v err=%v", replayed, err)
	}
}

func TestWorkflowActionUsesTeamManagementAuthorityAndAssignedAgentSource(t *testing.T) {
	store := NewMemoryStore()
	sourceRun, catalog, agentBound, args := workflowActionFixture(t, store)
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations-team"}
	conversations := NewConversationService(store)
	chat, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{Scope: sourceRun.Scope, Owner: owner, Title: "Team request", IdempotencyKey: "team-workflow-chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: chat.Scope, ConversationID: chat.ID, ExpectedRevision: chat.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion, Content: "Collect this reply for the team", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "team-human-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: sourceRun.Scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: sourceRun.AssignedAgentID, Goal: "Handle team request", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: chat.ID, conversationRunContextTriggerID: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "team-workflows", Scope: agentBound.Binding.Scope, DeploymentID: owner.ID, SkillID: RunbookManagementSkillID, SkillVersion: RunbookManagementSkillVersion, AllowedActions: []string{RunbookActionCreateWorkflow}, MaximumRisk: skill.RiskLevelWrite, Revision: 1}
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(t.Context(), binding.Scope, owner.ID, binding.SkillID, binding.SkillVersion, RunbookActionCreateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	teamStore := kernelteam.NewMemoryStore()
	deployment := &kernelteam.Deployment{ID: owner.ID, Scope: binding.Scope, DefinitionID: "team-definition", ActiveVersion: "1.0.0", Status: kernelteam.DeploymentActive, Revision: 1, CreatedAt: run.CreatedAt, UpdatedAt: run.CreatedAt, Roster: []kernelteam.RosterAssignment{{ID: "member", RoleID: "researcher", AgentDeploymentID: run.AssignedAgentID}}}
	if err := teamStore.CreateTeamDeployment(t.Context(), deployment, workforce.DefinitionActivation{}); err != nil {
		t.Fatal(err)
	}
	teams := kernelteam.NewRegistryWithStore(teamStore, nil)
	if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "unverified-team-workflow"); err == nil {
		t.Fatal("Team workflow created without its current authorized roster")
	}
	result, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "team-workflow", teams)
	if err != nil {
		t.Fatal(err)
	}
	work := result["run"].(*AgentRun)
	if work.Owner != owner || work.AssignedAgentID != sourceRun.AssignedAgentID || work.Context[conversationRunContextConversationID] != chat.ID || work.WakeCondition.EventWait.Attributes["deploymentId"] != sourceRun.AssignedAgentID {
		t.Fatalf("team workflow=%#v", work)
	}
	origin, channel, human, err := conversationWorkOrigin(t.Context(), store, store, work)
	if err != nil || origin == nil || origin.ID != run.ID || channel == nil || channel.ID != chat.ID || human == nil || human.ID != posted.Message.ID {
		t.Fatalf("Team workflow origin=%#v channel=%#v human=%#v error=%v", origin, channel, human, err)
	}
	deployment.Roster = nil
	deployment.Revision++
	if err := teamStore.UpdateTeamDeployment(t.Context(), deployment, 1, workforce.DefinitionActivation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchWorkflowForTest(t, store, catalog, run, bound, args, "removed-member-workflow", teams); err == nil {
		t.Fatal("Removed Team member created a new workflow")
	}
}

func TestWorkflowSourcesRejectStaleOrForeignInstalledAdapterSnapshots(t *testing.T) {
	for _, change := range []string{"skill", "provider", "publisher", "future-revision"} {
		t.Run(change, func(t *testing.T) {
			store := NewMemoryStore()
			run, catalog, _, _ := workflowActionFixture(t, store)
			gateway, err := store.GetExternalConversationGateway(t.Context(), run.Scope, "event-gateway")
			if err != nil {
				t.Fatal(err)
			}
			previous := gateway.Revision
			gateway.Revision++
			gateway.Lifecycle = append(gateway.Lifecycle, ExternalConversationGatewayLifecycleEntry{Revision: gateway.Revision, Action: ExternalConversationGatewayUpdated, Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "Inject stale registration snapshot for availability checks", At: gateway.UpdatedAt})
			switch change {
			case "skill":
				gateway.Gateway.Adapter.SkillID = "foreign-connector"
			case "provider":
				gateway.Gateway.Provider = "foreign-provider"
			case "publisher":
				gateway.Gateway.Adapter.SourceIdentity = "oci://foreign/publisher@sha256:other"
			case "future-revision":
				gateway.Gateway.Adapter.BindingRevision++
			}
			if err := store.UpdateExternalConversationGateway(t.Context(), gateway, previous); err != nil {
				t.Fatal(err)
			}
			sources, err := workflowSources(t.Context(), store, catalog, run)
			if err != nil {
				t.Fatal(err)
			}
			if len(sources) != 1 || sources[0].Available {
				t.Fatalf("stale installed source=%#v", sources)
			}
		})
	}
}
