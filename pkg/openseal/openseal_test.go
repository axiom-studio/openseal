package openseal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestPublicFacadeExposesCanonicalActivityCapability(t *testing.T) {
	capability := ActivityCapability()
	if capability.ID != ActivityCapabilityID || capability.Version != ActivityCapabilityVersion || !capability.Supports(KernelOperationList) {
		t.Fatalf("activity capability = %#v", capability)
	}
}

func TestRunTerminalFinalizationIsAutomaticWhenActionsCanExecute(t *testing.T) {
	store := runtime.NewMemoryStore()
	engine := &Engine{
		store:  store,
		skills: skill.NewCatalog(),
		actionPoolSpecs: []actionWorkerSpec{{
			dispatcher: ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{}, nil
			}),
		}},
	}
	finalizer, err := engine.newRunTerminalFinalizer()
	if err != nil || finalizer == nil {
		t.Fatalf("automatic Run terminal finalizer = %#v, %v", finalizer, err)
	}

	engine.actionPoolSpecs = nil
	finalizer, err = engine.newRunTerminalFinalizer()
	if err != nil || finalizer != nil {
		t.Fatalf("non-executing engine finalizer = %#v, %v", finalizer, err)
	}
}

func TestPublicFacadeExposesSourcePolicyLifecycleCapability(t *testing.T) {
	capability := SourcePoliciesCapability()
	if capability.ID != SourcePoliciesCapabilityID || capability.Version != SourcePoliciesCapabilityVersion ||
		!capability.Supports(KernelOperationListVersions) || !capability.Supports(KernelOperationActivate) || !capability.Supports(KernelOperationRevoke) {
		t.Fatalf("source policy capability = %#v", capability)
	}
}

func TestPublicFacadeExposesRunbookScheduleReconciliation(t *testing.T) {
	capability := RunbooksCapability()
	if capability.ID != RunbooksCapabilityID || capability.Version != RunbooksCapabilityVersion || !capability.Supports(KernelOperationReconcile) || !capability.Supports(KernelOperationExecute) {
		t.Fatalf("objective schedule capability = %#v", capability)
	}
	request := ReconcileRunbookSchedulesRequest{Scope: Scope{Kind: "tenant", ID: "operations"}, Limit: 50}
	if request.Scope.ID != "operations" || request.Limit != 50 {
		t.Fatalf("objective schedule request = %#v", request)
	}
	schedule := RunbookSchedule{Cron: "0 0 9 * * *", Timezone: "UTC"}
	if RunbookTriggerSchedule != RunbookTriggerKind("schedule") || schedule.Validate() != nil {
		t.Fatalf("Runbook schedule facade = %#v, %q", schedule, RunbookTriggerSchedule)
	}
}

func TestPublicFacadeExposesRunbookActivationErrors(t *testing.T) {
	if !errors.Is(ErrRunbookActivationNotFound, runtime.ErrRunbookActivationNotFound) ||
		!errors.Is(ErrRunbookActivationRevision, runtime.ErrRunbookActivationRevision) ||
		!errors.Is(ErrRunbookActivationIdempotency, runtime.ErrRunbookActivationIdempotency) {
		t.Fatal("Runbook activation errors are not exposed through the public facade")
	}
}

func TestPublicFacadeExposesEventSourceSubscriptions(t *testing.T) {
	capability := EventSourceSubscriptionsCapability()
	if capability.ID != EventSourceSubscriptionsCapabilityID || capability.Version != EventSourceSubscriptionsCapabilityVersion ||
		!capability.Supports(KernelOperationReportHealth) || !capability.Supports(KernelOperationGetCheckpoint) || !capability.Supports(KernelOperationAdvanceCheckpoint) {
		t.Fatalf("event source subscription capability = %#v", capability)
	}
}

func TestPublicFacadeExposesConversationGatewayChoices(t *testing.T) {
	choice := ConversationGatewayAdapterChoice{ID: "slack-primary", DisplayName: "Primary Slack", DeploymentID: "agent-one", Provider: "slack", SkillID: "slack", SkillVersion: "1", BindingID: "binding", BindingRevision: 2, AdapterID: "events"}
	capability := ConversationGatewaysCapability(true, []ConversationGatewayAdapterChoice{choice})
	if capability.ID != ConversationGatewaysCapabilityID || capability.Version != ConversationGatewaysCapabilityVersion || capability.Context == nil || len(capability.Context.ConversationGatewayAdapters) != 1 || capability.Context.ConversationGatewayAdapters[0] != choice {
		t.Fatalf("conversation gateway capability = %#v", capability)
	}
}

func TestEngineExposesExternalConversationEndpointLifecycle(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	definition := &SkillDefinition{
		ID: "slack", Version: "1.0.0", Name: "Slack",
		ConversationAdapters: map[string]ConversationAdapter{"conversations": {
			ProtocolVersion: ConversationAdapterProtocolV1,
			Name:            "Slack conversations", Description: "Deliver governed messages.", Provider: "slack",
			EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
			InboundEventTypes: []string{ConversationEventMessageReceived},
			Features:          []ConversationAdapterFeature{ConversationFeatureThreads},
			Delivery: ConversationDeliveryCapabilities{
				Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
				Ordering:   ConversationDeliveryOrderThread, Idempotency: SkillIdempotencyRequired,
			},
			Transport: ConversationAdapterTransport{
				Kind: "plugin", IngressEndpoint: "slack.conversation.ingress", DeliveryEndpoint: "slack.conversation.deliver",
			},
		}},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	ensureTestAgent(t, engine, scope, "agent-one")
	if err := engine.BindSkill(ctx, &SkillBinding{
		ID: "slack-primary", Scope: scope, DeploymentID: "agent-one", SkillID: definition.ID,
		SkillVersion: definition.Version, EnabledConversationAdapters: []string{"conversations"},
		MaximumRisk: SkillRiskExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := engine.CreateExternalConversationEndpoint(ctx, CreateExternalConversationEndpointRequest{
		ID: "approval-slack", Scope: Scope(scope), Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"},
		DeploymentID: "agent-one", Name: "Approval channel",
		Adapter: ExternalConversationAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: "slack-primary",
			BindingRevision: 1, AdapterID: "conversations",
		},
		Mode: ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "agent-one"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectDirectOrMention,
			ReplyMode:        ExternalConversationReplyThread, IgnoreBots: true,
		},
		Status: ExternalConversationEndpointPaused,
	})
	if err != nil {
		t.Fatal(err)
	}
	active := ExternalConversationEndpointActive
	endpoint, err = engine.UpdateExternalConversationEndpoint(ctx, endpoint.Scope, endpoint.ID, UpdateExternalConversationEndpointRequest{
		ExpectedRevision: endpoint.Revision, Status: &active,
	})
	if err != nil || endpoint.Status != active || endpoint.Revision != 2 {
		t.Fatalf("active endpoint = %#v, %v", endpoint, err)
	}
	items, err := engine.ListExternalConversationEndpoints(ctx, ExternalConversationEndpointFilter{Scope: endpoint.Scope, Limit: 10})
	if err != nil || len(items) != 1 || items[0].ID != endpoint.ID {
		t.Fatalf("endpoints = %#v, %v", items, err)
	}
}

func TestEnginePersistentStoreRestoresSkillBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	definition := &SkillDefinition{ID: "writer", Version: "1", Name: "Writer", Prompt: &SkillPromptModule{Instructions: "Write concise release notes."}}
	if err := engine.RegisterSkill(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	if err := engine.BindSkill(context.Background(), &SkillBinding{ID: "writer", Scope: scope, DeploymentID: "marketing", SkillID: "writer", SkillVersion: "1", EnablePrompt: true, MaximumRisk: SkillRiskRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(WithPersistentStore(reopened))
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := restarted.ListModelSkillPrompts(context.Background(), scope, "marketing")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "writer" {
		t.Fatalf("restored prompts = %#v, %v", prompts, err)
	}
}

func TestConversationRunsRejectMismatchedParticipationPolicy(t *testing.T) {
	for _, schedulerOptIn := range []bool{false, true} {
		participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return nil, nil
		})
		router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
			return ConversationRouteDecision{}, nil
		})
		_, err := New(
			WithStore(runtime.NewMemoryStore()),
			WithConversationCoordinator(participants, router, DefaultConversationCoordinatorConfig()),
			WithDynamicConversationRuns(ConversationRunConfig{
				Scheduler: ConversationRunSchedulerConfig{RequireParticipationOptIn: schedulerOptIn},
				Runner:    ConversationRunTurnRunnerConfig{RequireParticipationOptIn: !schedulerOptIn},
			}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{{Kind: "local", ID: "default"}}, nil })),
		)
		if err == nil || err.Error() != "conversation Run configuration: conversation scheduling and execution must use the same participation opt-in policy" {
			t.Fatalf("mismatched policy accepted or wrong error: %v", err)
		}
	}
}

func TestEngineExposesObjectivePortfolio(t *testing.T) {
	engine, err := New(WithStore(runtime.NewMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	objective, err := engine.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Operate", Goal: "Keep operating", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Inspect current health", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ObjectiveID != objective.ID || run.RootRunID != run.ID {
		t.Fatalf("unexpected run: %#v", run)
	}
	objectives, err := engine.ListObjectives(ctx, ObjectiveFilter{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(objectives) != 1 || objectives[0].ID != objective.ID {
		t.Fatalf("unexpected objectives: %#v", objectives)
	}
	run, event, err := engine.TransitionAgentRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "Health inspection started",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != AgentRunStatusRunning || event.Sequence != 2 {
		t.Fatalf("unexpected transition: run=%#v event=%#v", run, event)
	}
	turn, err := engine.BeginAgentTurn(ctx, BeginAgentTurnRequest{
		Scope: scope, RunID: run.ID, DefinitionID: "operator", DefinitionVersion: "1", Model: "test-model", WorkerID: "test-worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = engine.FinishAgentTurn(ctx, scope, turn.ID, FinishAgentTurnRequest{
		ExpectedRevision: turn.Revision, Status: AgentTurnStatusCompleted, WorkerID: "test-worker",
		OutputSummary: "Inspection plan ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	listedTurns, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedTurns) != 1 || listedTurns[0].ID != turn.ID || listedTurns[0].CompletedAt == nil {
		t.Fatalf("unexpected turns: %#v", listedTurns)
	}
	_, err = engine.AppendActivity(ctx, &ActivityEvent{
		Scope: scope, RunID: run.ID, EventType: "inspection.progress", Summary: "Checked the first subsystem",
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := engine.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Sequence != 3 || events[2].Visibility != ActivityVisibilityTeam {
		t.Fatalf("unexpected activity: %#v", events)
	}
	waiting, _, err := engine.TransitionAgentRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForEvent,
		WakeCondition: &WakeCondition{Type: "event", Reference: "health.changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	woken, err := engine.WakeAgentRuns(ctx, WakeSignal{
		ID: "health-signal", Scope: scope, Type: "event", Reference: "health.changed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(woken.Runs) != 1 || woken.Runs[0].Run.ID != waiting.ID || woken.Runs[0].Run.Status != AgentRunStatusQueued {
		t.Fatalf("unexpected wake result: %#v", woken)
	}
	autonomousRun, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: "agent-2", Goal: "Complete one bounded step", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{
		Scope: scope, WorkerID: "test-worker", AssignedAgentID: "agent-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != autonomousRun.ID || claimed.LeaseOwner != "test-worker" {
		t.Fatalf("unexpected scheduled claim: %#v", claimed)
	}
}
