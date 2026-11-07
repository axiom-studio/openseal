package openseal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
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

// loopReply is what the Engine tests' fake model answers one step with: text
// and no calls is a plain-text final; calls get ids from the step id.
type loopReply struct {
	Text  string
	Calls []ModelToolCall
	Usage TurnUsage
}

// loopStepHost is the Engine tests' StepHost. It decides each step from the
// request alone (the run, the rendered entries and the offered tools) and runs
// no host tools.
type loopStepHost struct {
	mu       sync.Mutex
	decide   func(context.Context, ModelStepRequest) (loopReply, error)
	requests []ModelStepRequest
}

func (h *loopStepHost) ExecuteModelStep(ctx context.Context, req ModelStepRequest) (ModelStepResult, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	reply, err := h.decide(ctx, req)
	if err != nil {
		return ModelStepResult{}, err
	}
	calls := append([]ModelToolCall(nil), reply.Calls...)
	stop := ModelStopEnd
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call-%s-%d", req.StepID, i+1)
		}
		stop = ModelStopToolCalls
	}
	usage := reply.Usage
	if usage == (TurnUsage{}) {
		usage = TurnUsage{InputTokens: 100, OutputTokens: 20}
	}
	result := ModelStepResult{
		APIVersion: ModelStepAPIVersion, StepID: req.StepID,
		Assistant: runtime.AssistantEntry{
			Provider: ModelStepProviderChat, Model: "fake-model", Text: reply.Text, ToolCalls: calls, StopReason: stop,
			Usage: ModelStepUsage{InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens), Attempts: 1},
		},
		Model: ModelStepModelInfo{Provider: ModelStepProviderChat, Model: "fake-model", ContextTokens: 200000},
	}
	if len(calls) > 0 {
		result.Narration = reply.Text
	}
	return result, nil
}

func (h *loopStepHost) ExecuteHostTool(context.Context, runtime.HostToolRequest) (runtime.HostToolResult, error) {
	return runtime.HostToolResult{}, errors.New("the Engine test host runs no host tools")
}

func (h *loopStepHost) requestsFor(runID string) []ModelStepRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []ModelStepRequest
	for _, req := range h.requests {
		if req.RunID == runID {
			out = append(out, req)
		}
	}
	return out
}

// loopRequestText is every text the step renders: user, note and tool
// result parts and assistant texts, in order.
func loopRequestText(req ModelStepRequest) string {
	var b strings.Builder
	parts := func(content []runtime.ContentPart) {
		for _, part := range content {
			b.WriteString(part.Text)
			b.Write(part.Value)
			b.WriteByte('\n')
		}
	}
	for _, entry := range req.Entries {
		switch {
		case entry.User != nil:
			parts(entry.User.Content)
		case entry.Note != nil:
			b.WriteString(entry.Note.Text + "\n")
		case entry.Tool != nil:
			parts(entry.Tool.Result.Content)
		case entry.Assistant != nil:
			b.WriteString(entry.Assistant.Text + "\n")
		}
	}
	return b.String()
}

func loopWaitCall(seconds int) ModelToolCall {
	return ModelToolCall{Name: "wait", Arguments: json.RawMessage(fmt.Sprintf(`{"seconds":%d}`, seconds))}
}

// loopResolver binds every claimed Run through ResolveLoopBinding with the
// Engine as its catalog, the way an embedding host does: the Engine's store,
// a claim tool dispatcher with the control tools, and host as the step host.
// The Engine is created after the resolver, so it is read through engine.
func loopResolver(engine **Engine, host StepHost) TurnRunnerResolver {
	var once sync.Once
	var config LoopBindingConfig
	var configErr error
	return TurnRunnerResolverFunc(func(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if *engine == nil {
			return nil, runtime.ErrTurnHostUnavailable
		}
		once.Do(func() { config, configErr = loopBindingConfig(*engine, host) })
		if configErr != nil {
			return nil, configErr
		}
		return ResolveLoopBinding(ctx, *engine, run, config)
	})
}

func loopBindingConfig(engine *Engine, host StepHost) (LoopBindingConfig, error) {
	store := engine.Store()
	control := &runtime.ControlExecutor{Portfolio: store, Sessions: store, Activity: runtime.NewRunActivityService(store, store)}
	var conversations runtime.ConversationStore
	if reporting, ok := store.(runtime.ConversationStore); ok {
		conversations = reporting
		control.Reporting = reporting
		control.Conversations = runtime.NewConversationService(reporting)
		control.Conversations.SetReactionProjector(engine.ReactionProjector())
	}
	tools, err := runtime.NewStepToolDispatcher(runtime.StepToolDispatcherConfig{
		Sessions: store, Turns: store, Actions: store,
		Coordinator: runtime.NewActionCoordinator(store, store, nil, nil),
		Governed:    engine.GovernedTools(), Control: control,
	})
	if err != nil {
		return LoopBindingConfig{}, err
	}
	return LoopBindingConfig{Host: host, Store: store, Conversations: conversations, Tools: tools}, nil
}

// registerLoopAgent activates a prompt-only Agent (no Skills) in scope.
func registerLoopAgent(t *testing.T, ctx context.Context, engine *Engine, scope Scope, id string) {
	t.Helper()
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: id, Version: "1", DisplayName: strings.ReplaceAll(id, "-", " "),
		Purpose:      "Answer and work from the evidence in front of you.",
		SystemPrompt: "Work only from what the session shows.",
		Authority:    AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: id, Scope: SkillScope{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "operator", "activate test Agent"); err != nil {
		t.Fatal(err)
	}
}

func waitForRunStatus(t *testing.T, ctx context.Context, engine *Engine, scope Scope, id string, expected AgentRunStatus) *AgentRun {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := engine.GetAgentRun(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == expected {
			return run
		}
		if run.Status != expected && (run.Status == AgentRunStatusCompleted || run.Status == AgentRunStatusFailed || run.Status == AgentRunStatusCanceled || run.Status == AgentRunStatusPaused) {
			t.Fatalf("run %s status=%s expected=%s error=%s explanation=%+v", id, run.Status, expected, run.Error, run.FailureExplanation)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run %s did not reach %s (status %s): %v", id, expected, run.Status, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestEngineRunsAutonomousAgentPortfolio(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "local", ID: "autonomous"}
	var engine *Engine
	host := &loopStepHost{decide: func(_ context.Context, req ModelStepRequest) (loopReply, error) {
		// Step 1 waits once; step 2 answers from the wait's result.
		if strings.Contains(loopRequestText(req), `"status":"elapsed"`) {
			return loopReply{Text: "Finished autonomously after one pause."}, nil
		}
		return loopReply{Text: "Pausing briefly before finishing.", Calls: []ModelToolCall{loopWaitCall(1)}}, nil
	}}
	var err error
	engine, err = New(WithStore(store), WithAgentRunWorkers(AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", PollInterval: 5 * time.Millisecond,
		LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	}, loopResolver(&engine, host)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "agent")
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "finish autonomously", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	done := waitForRunStatus(t, ctx, engine, scope, run.ID, AgentRunStatusCompleted)
	if done.Output["summary"] != "Finished autonomously after one pause." || done.SessionID == "" || done.LastAppliedTurn != 2 {
		t.Fatalf("completed run = %#v", done)
	}
	requests := host.requestsFor(run.ID)
	if len(requests) != 2 || !strings.Contains(loopRequestText(requests[0]), "finish autonomously") || requests[1].SessionID != done.SessionID {
		t.Fatalf("model steps = %#v", requests)
	}
	offered := map[string]bool{}
	for _, tool := range requests[0].Tools {
		offered[tool.Name] = true
	}
	if !offered["wait"] || !offered["turn_final_answer"] {
		t.Fatalf("background work was not offered wait and turn_final_answer: %#v", requests[0].Tools)
	}
	steps, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(steps) != 2 || steps[0].OutputSummary != "Pausing briefly before finishing." {
		t.Fatalf("step rows = %#v, %v", steps, err)
	}
}

func TestEngineRunsDynamicKindScopedPortfolio(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "dynamic"}
	var engine *Engine
	host := &loopStepHost{decide: func(context.Context, ModelStepRequest) (loopReply, error) {
		return loopReply{Text: "Work complete"}, nil
	}}
	var err error
	engine, err = New(
		WithStore(store),
		WithDynamicAgentRunWorkers(DynamicAgentRunWorkerConfig{
			Kind: RunKindAgentWork, AssignedAgentID: "worker", PollInterval: 5 * time.Millisecond, ReconcileInterval: 5 * time.Millisecond,
			LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil }), loopResolver(&engine, host)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "worker")
	registerLoopAgent(t, ctx, engine, scope, "other")
	assigned, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "worker"}, AssignedAgentID: "worker", Goal: "Assigned work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}, AssignedAgentID: "other", Goal: "Unrelated work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	waitForRunStatus(t, ctx, engine, scope, assigned.ID, AgentRunStatusCompleted)
	other, err := engine.GetAgentRun(ctx, scope, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != AgentRunStatusQueued || len(host.requestsFor(unrelated.ID)) != 0 {
		t.Fatalf("Agent-scoped dynamic worker ran another Agent's work: %#v", other)
	}
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
	router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
		return ConversationRouteDecision{AgentID: "agent-1", Reason: "watches the replicas"}, nil
	})
	var engine *Engine
	host := &loopStepHost{decide: func(_ context.Context, req ModelStepRequest) (loopReply, error) {
		text := loopRequestText(req)
		if !strings.Contains(text, "channel state?") {
			return loopReply{}, fmt.Errorf("member step did not see its trigger: %s", text)
		}
		return loopReply{Text: "Telemetry evidence confirms all three replicas passed their checks."}, nil
	}}
	resolver := loopResolver(&engine, host)
	scopes := WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })
	var err error
	engine, err = New(
		WithStore(store),
		WithConversationCoordinator(participants, router, DefaultConversationCoordinatorConfig()),
		WithDynamicConversationRuns(ConversationRunConfig{
			Runner: ConversationRunTurnRunnerConfig{AgentTurns: resolver},
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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "agent-1")
	registerLoopTeam(t, ctx, engine, scope, "operations", "agent-1")
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
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	for {
		runs, listErr := engine.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
		if listErr != nil {
			t.Fatal(listErr)
		}
		// Each message gets a routing Run and its member's answer Run.
		completed := 0
		for _, run := range runs {
			switch run.Status {
			case AgentRunStatusCompleted:
				completed++
			case AgentRunStatusFailed, AgentRunStatusCanceled, AgentRunStatusPaused:
				t.Fatalf("conversation run %s ended %s: %s %+v", run.ID, run.Status, run.Error, run.FailureExplanation)
			}
		}
		if len(runs) == 4 && completed == 4 && conversationsAnswered(t, ctx, engine, scope, immediate, recovered) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("durable conversation runtime did not complete immediate and reconciled messages: %d runs, %d completed", len(runs), completed)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Two member steps, one per message; the routing Runs make no model call.
	if steps := len(host.requests); steps != 2 {
		t.Fatalf("model steps = %d, want one member step per message", steps)
	}
}

func conversationsAnswered(t *testing.T, ctx context.Context, engine *Engine, scope Scope, conversations ...*Conversation) bool {
	t.Helper()
	for _, conversation := range conversations {
		messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 {
			return false
		}
		answer := messages[1]
		if answer.Sender.ID != "agent-1" || answer.RoutingReason != "watches the replicas" ||
			answer.Content != "Telemetry evidence confirms all three replicas passed their checks." || answer.ResolvesMessageID != messages[0].ID {
			t.Fatalf("completed channel %s messages = %#v", conversation.ID, messages)
		}
	}
	return true
}

// registerLoopTeam activates a Team whose roster is members.
func registerLoopTeam(t *testing.T, ctx context.Context, engine *Engine, scope Scope, id string, members ...string) {
	t.Helper()
	roles, roster := make([]TeamRoleSlot, 0, len(members)), make([]TeamRosterAssignment, 0, len(members))
	for index, member := range members {
		role := fmt.Sprintf("member-%d", index+1)
		roles = append(roles, TeamRoleSlot{ID: role, DisplayName: role, Purpose: "Answer the channel", MinimumMembers: 1})
		roster = append(roster, TeamRosterAssignment{ID: role, RoleID: role, AgentDeploymentID: member})
	}
	definition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: id, Version: "1", DisplayName: id, Purpose: "Answer the channel", Roles: roles,
		Delegation: TeamDelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 3},
		Approvals:  TeamApprovalPolicy{MaximumRisk: capability.RiskLevelWrite, ApproverRoleIDs: []string{roles[0].ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: id, Scope: SkillScope{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: TeamDeploymentActive, Roster: roster,
	}, "user", "operator", "activate test Team"); err != nil {
		t.Fatal(err)
	}
}

func TestEngineWakeAgentWorkersForScopeClaimsWithoutPolling(t *testing.T) {
	var nilEngine *Engine
	nilEngine.WakeAgentWorkersForScope(Scope{Kind: "local", ID: "x"})

	store := runtime.NewMemoryStore()
	scope := Scope{Kind: "local", ID: "wake"}
	claimed := make(chan time.Time, 1)
	var engine *Engine
	host := &loopStepHost{decide: func(context.Context, ModelStepRequest) (loopReply, error) {
		select {
		case claimed <- time.Now():
		default:
		}
		return loopReply{Text: "Done"}, nil
	}}
	var err error
	engine, err = New(
		WithStore(store),
		// A poll far longer than the test proves the wake, not the poll, claimed.
		WithAgentRunWorkers(AgentRunWorkerConfig{
			Scope: scope, AssignedAgentID: "agent", PollInterval: time.Hour,
			LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, loopResolver(&engine, host)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerLoopAgent(t, ctx, engine, scope, "agent")
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	time.Sleep(20 * time.Millisecond) // let the start-up claim pass find nothing
	run, err := runtime.NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "wake me", Source: RunSourceObjective,
	})
	if err != nil || run.Run == nil {
		t.Fatal(err)
	}
	engine.WakeAgentWorkersForScope(Scope{Kind: "local", ID: "other"})
	select {
	case <-claimed:
		t.Fatal("a wake for another scope must not claim this scope's Run")
	case <-time.After(50 * time.Millisecond):
	}
	woke := time.Now()
	engine.WakeAgentWorkersForScope(scope)
	select {
	case at := <-claimed:
		if latency := at.Sub(woke); latency > 500*time.Millisecond {
			t.Fatalf("wake to model step took %s", latency)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scoped wake did not claim the Run")
	}
	waitForRunStatus(t, ctx, engine, scope, run.Run.ID, AgentRunStatusCompleted)
}
