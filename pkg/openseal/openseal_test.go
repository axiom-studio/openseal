package openseal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestPublicFacadeExposesCanonicalActivityCapability(t *testing.T) {
	capability := ActivityCapability()
	if capability.ID != ActivityCapabilityID || capability.Version != ActivityCapabilityVersion || !capability.Supports(KernelOperationList) {
		t.Fatalf("activity capability = %#v", capability)
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
		Decisions:     []TurnDecision{{Summary: "Inspect dependencies", EvidenceRefs: []string{"artifact:health"}}},
		OutputSummary: "Inspection plan ready", ContinuationCheckpoint: map[string]interface{}{"next": "inspect"},
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
	advanced, err := engine.AdvanceAgentRun(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: autonomousRun.ID, WorkerID: "test-worker", Model: "test-model",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Bounded step complete",
			RunOutput: map[string]interface{}{"result": "ok"},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Run.Status != AgentRunStatusCompleted || advanced.Run.LastAppliedTurn != 1 || advanced.Event.TurnID != advanced.Turn.ID {
		t.Fatalf("unexpected bounded advance: %#v", advanced)
	}
}

func TestEngineRunsAutonomousAgentPortfolio(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "local", ID: "autonomous"}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DefinitionID: "test-agent", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
			}),
		}, nil
	})
	engine, err := New(
		WithStore(store),
		WithAgentRunWorkers(AgentRunWorkerConfig{
			Scope: scope, AssignedAgentID: "agent", PollInterval: 5 * time.Millisecond,
			LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, resolver),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "finish autonomously", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := engine.GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == AgentRunStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("autonomous Engine worker did not complete the run")
}

func TestEngineRunsDynamicKindScopedPortfolio(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "dynamic"}
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.Kind != RunKindConversation {
			t.Fatalf("dynamic resolver received run kind %q", run.Kind)
		}
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Conversation complete"}, nil
		})}, nil
	})
	engine, err := New(
		WithStore(store),
		WithDynamicAgentRunWorkers(DynamicAgentRunWorkerConfig{
			Kind: RunKindConversation, PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond,
			LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil }), resolver),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conversationRun, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"},
		Goal: "Coordinate channel", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentRun, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Unrelated work",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := engine.GetAgentRun(ctx, scope, conversationRun.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == AgentRunStatusCompleted {
			unrelated, getErr := engine.GetAgentRun(ctx, scope, agentRun.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if unrelated.Status != AgentRunStatusQueued {
				t.Fatalf("dynamic conversation worker claimed agent work: %#v", unrelated)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("dynamic Engine worker did not complete the conversation run")
}

func TestEngineOwnsDurableConversationRunsAndRecoversSchedulingGap(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "conversation-runtime"}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{
			Participant:   ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"},
			SemanticRoles: []string{"operator"},
		}}, nil
	})
	proposals := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer,
			Content:  "Telemetry evidence confirms all three replicas passed their checks.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: input.Trigger.ID,
			Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, HasEvidence: true, RoleRelevant: true},
		}, nil
	})
	scopes := WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })
	engine, err := New(
		WithStore(store),
		WithConversationCoordinator(participants, proposals, DefaultConversationCoordinatorConfig()),
		WithDynamicConversationRuns(ConversationRunConfig{
			Workers: DynamicAgentRunWorkerConfig{
				Concurrency: 2, PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond,
				LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			},
			Reconciler: ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond},
		}, scopes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.ConversationRunsAvailable() {
		t.Fatal("durable conversation runtime was not advertised")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	immediate, _, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"}, Title: "Immediate", IdempotencyKey: "immediate",
	})
	if err != nil {
		t.Fatal(err)
	}
	immediateMessage, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: immediate.ID, ExpectedRevision: immediate.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "What is the immediate channel state?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "immediate-question",
	})
	if err != nil || immediateMessage.Run == nil || immediateMessage.Run.Kind != RunKindConversation ||
		immediateMessage.Run.ConcurrencyKey != immediate.ID {
		t.Fatalf("immediate message scheduling = %#v, %v", immediateMessage, err)
	}

	recovered, _, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"}, Title: "Recovered", IdempotencyKey: "recovered",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bypass the Engine hook to model a process dying after the message store
	// commit and before the immediate Run scheduling call.
	committedOnly, err := runtime.NewConversationService(store).PostChannelMessage(ctx, runtime.PostChannelMessageRequest{
		Scope: scope, ConversationID: recovered.ID, ExpectedRevision: recovered.Revision,
		Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "operator"},
		Intent: runtime.MessageIntentQuestion, Content: "What is the recovered channel state?",
		Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "recovered-question",
	})
	if err != nil || committedOnly.Run != nil {
		t.Fatalf("message-only commit = %#v, %v", committedOnly, err)
	}

	engine.Start(ctx)
	defer engine.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, listErr := engine.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
		if listErr != nil {
			t.Fatal(listErr)
		}
		completed := 0
		for _, run := range runs {
			if run.Status == AgentRunStatusCompleted {
				completed++
			}
		}
		if len(runs) == 2 && completed == 2 {
			for _, conversation := range []*Conversation{immediate, recovered} {
				messages, messageErr := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
				if messageErr != nil || len(messages) != 2 || messages[1].ParticipationRoundID == "" {
					t.Fatalf("completed channel %s messages = %#v, %v", conversation.ID, messages, messageErr)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("durable conversation runtime did not complete immediate and reconciled messages")
}
