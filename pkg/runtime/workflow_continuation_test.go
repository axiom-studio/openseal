package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Use real catalog activation, rather than handing the worker a preselected
// runner or action snapshot. Changing the active definition and binding while
// the Run sleeps must affect the resumed Turn.
type workflowContinuationCatalog struct {
	registry *kernelagent.Registry
	skills   *skill.Catalog
	outreach *OutreachService
}

func (c workflowContinuationCatalog) GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	return c.registry.GetDeployment(ctx, scope, id)
}
func (c workflowContinuationCatalog) ListAgentDeployments(ctx context.Context, filter kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	return c.registry.ListDeployments(ctx, filter)
}
func (c workflowContinuationCatalog) GetAgentDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	return c.registry.GetDefinition(ctx, id, version)
}
func (c workflowContinuationCatalog) ActivateSkills(ctx context.Context, scope skill.ScopeReference, id string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return c.skills.Activate(ctx, scope, id, host)
}
func (c workflowContinuationCatalog) GetOutreachThread(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	return c.outreach.Get(ctx, scope, id)
}
func (c workflowContinuationCatalog) ReconcileOutreachAction(ctx context.Context, scope Scope, id string, request ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	return c.outreach.ReconcileAction(ctx, scope, id, request)
}

// The fake implements only the provider verification boundary. Tenant,
// deployment, binding, account pins and durable event publication are all
// supplied and enforced by the ordinary gateway ingress service.
type workflowContinuationProvider struct {
	event NormalizedExternalConversationEvent
	calls int
}

func (p *workflowContinuationProvider) NormalizeExternalConversationGateway(_ context.Context, request ExternalConversationGatewayHostRequest) (*ExternalConversationGatewayHostResult, error) {
	p.calls++
	if request.Adapter == nil || request.Adapter.Binding == nil || request.Adapter.Binding.ID != "event-account" || request.Gateway.Provider != "test" || request.Request == nil || len(request.Request.Headers["X-Test-Signature"]) != 1 || request.Request.Headers["X-Test-Signature"][0] != "verified" {
		return nil, errors.New("provider signature or installed binding does not match")
	}
	return &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{InstallationID: "installation-one", ApplicationID: "application-one", Address: p.event.ExternalConversationID, Event: p.event}}}, nil
}

// No LLM or network is involved: this host consumes the same request that a
// real hosted Turn receives and derives its result solely from the kernel's
// matched event or explicit timeout checkpoint.
type workflowContinuationHost struct {
	requests []HostedTurnRequest
}

func (h *workflowContinuationHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.requests = append(h.requests, request)
	resolution, ok := request.ContinuationCheckpoint[runEventWaitCheckpointKey].(map[string]interface{})
	if !ok {
		return nil, errors.New("resumed Turn is missing the durable event resolution")
	}
	var summary string
	switch resolution["status"] {
	case string(RunEventWaitMatched):
		event, _ := resolution["event"].(map[string]interface{})
		payload, _ := event["payload"].(map[string]interface{})
		text, _ := payload["text"].(string)
		if text == "" {
			return nil, errors.New("matched Turn is missing the actual provider reply")
		}
		summary = "Received reply: " + text
	case string(RunEventWaitTimedOut):
		if _, invented := resolution["event"]; invented {
			return nil, errors.New("timeout contains a fabricated reply")
		}
		summary = "The requested reply did not arrive before the deadline."
	default:
		return nil, fmt.Errorf("unexpected event resolution: %v", resolution["status"])
	}
	return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "deterministic-continuation", NextRunStatus: AgentRunStatusCompleted, OutputSummary: summary, RunOutput: map[string]interface{}{"summary": summary}, ContinuationCheckpoint: cloneMap(request.ContinuationCheckpoint)}, nil
}

type workflowContinuationReportingOutage struct {
	ConversationStore
	attempts int
}

func (s *workflowContinuationReportingOutage) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if record.Message.Sender.Type == ConversationParticipantAgent {
		s.attempts++
		return nil, errors.New("temporary chat delivery outage")
	}
	return s.ConversationStore.CommitChannelMessage(ctx, record)
}

func TestDeferredWorkflowContinuesThroughNormalAgentWorkerAndDurableReporting(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, outcome := range []string{"reply", "early_reply", "timeout"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				ctx := t.Context()
				var store KernelStore = NewMemoryStore()
				path := filepath.Join(t.TempDir(), "continuation.db")
				if kind == "sqlite" {
					sqlite, err := NewSQLiteStore(path)
					if err != nil {
						t.Fatal(err)
					}
					store = sqlite
					t.Cleanup(func() { _ = store.(*SQLiteStore).Close() })
				}
				source, skills, bound, args := workflowActionFixture(t, store)
				now := time.Now().UTC().Add(time.Second)
				provider := &workflowContinuationProvider{event: NormalizedExternalConversationEvent{ID: "verified-reply", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "direct-conversation", ExternalMessageID: "provider-message-one", ExternalParticipantID: "person-one", Text: "I am finishing the release notes.", Direct: true, OrderingKey: "direct-conversation:provider-message-one", OccurredAt: now}}
				ingest := func() {
					t.Helper()
					gateway, err := store.(ExternalConversationGatewayStore).GetExternalConversationGateway(ctx, source.Scope, "event-gateway")
					if err != nil {
						t.Fatal(err)
					}
					transport := NewExternalConversationTransportService(store.(ExternalConversationStore), skills)
					received, err := transport.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{Route: gateway.IngressRoute, Method: http.MethodPost, Headers: map[string][]string{"X-Test-Signature": {"verified"}}, Body: []byte(`{"event":"reply"}`)}, provider)
					if err != nil || received == nil || received.Response.StatusCode != http.StatusOK || len(received.Received) != 0 {
						t.Fatalf("trusted ingress=%#v error=%v", received, err)
					}
				}
				// Some providers reveal the conversation ID only after sending.
				// A fast reply can arrive before workflow registration completes.
				if outcome == "early_reply" {
					ingest()
				}
				result, err := dispatchWorkflowForTest(t, store, skills, source, bound, args, "durable-follow-up")
				if err != nil {
					t.Fatal(err)
				}
				work := result["run"].(*AgentRun)
				conversationID := work.Context[conversationRunContextConversationID].(string)
				triggerID := work.Context[conversationRunContextTriggerID].(string)
				wait, err := store.(RunEventWaitStore).GetRunEventWait(ctx, work.Scope, work.ID, "initial")
				if err != nil || wait == nil || wait.Status != RunEventWaitPending {
					t.Fatalf("created wait=%#v error=%v", wait, err)
				}
				activity := NewRunActivityService(store, store)
				finishedSource, _, err := activity.TransitionRun(ctx, source.Scope, source.ID, RunTransitionRequest{ExpectedRevision: source.Revision, Status: AgentRunStatusRunning, Actor: ActivityActor{Type: "test", ID: "chat-worker"}, Summary: "Handle the originating chat request"})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := activity.TransitionRun(ctx, source.Scope, source.ID, RunTransitionRequest{ExpectedRevision: finishedSource.Revision, Status: AgentRunStatusCompleted, Actor: ActivityActor{Type: "test", ID: "chat-worker"}, Summary: "The chat request finishes while its follow-up waits"}); err != nil {
					t.Fatal(err)
				}

				// Reactivate the current definition and narrow a binding after the
				// wait was saved. SQLite uses its real persisted Agent registry.
				registryStore, persisted := store.(kernelagent.Store)
				if !persisted {
					registryStore = kernelagent.NewMemoryStore()
				}
				registry := kernelagent.NewRegistryWithStore(registryStore)
				if !persisted {
					definition := sqliteAgentDefinition("1.0.0")
					if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
						t.Fatal(err)
					}
					if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: work.AssignedAgentID, Scope: bound.Binding.Scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "initial definition"); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := registry.RegisterDefinition(ctx, sqliteAgentDefinition("2.0.0")); err != nil {
					t.Fatal(err)
				}
				deployment, err := registry.GetDeployment(ctx, bound.Binding.Scope, work.AssignedAgentID)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := registry.ActivateDefinition(ctx, bound.Binding.Scope, deployment.ID, "2.0.0", deployment.Revision, "user", "user", "updated while waiting"); err != nil {
					t.Fatal(err)
				}
				currentBinding := *bound.Binding
				currentBinding.AllowedActions = nil
				for _, action := range bound.Binding.AllowedActions {
					if action != RunbookActionCreateTask {
						currentBinding.AllowedActions = append(currentBinding.AllowedActions, action)
					}
				}
				if _, err := skills.UpsertBinding(ctx, skill.UpsertBindingRequest{Binding: &currentBinding, ExpectedRevision: bound.Binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "user"}, Reason: "narrow tools while waiting"}); err != nil {
					t.Fatal(err)
				}

				if outcome == "reply" {
					ingest()
				} else if outcome == "timeout" {
					now = wait.Spec.Deadline.Add(time.Second)
				}
				eventWorker, err := NewRunEventWaitWorker(store.(RunEventWaitStore), RunEventWaitWorkerOptions{WorkerID: "continuation-events", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
				eventWorker.now = func() time.Time { return now }
				if processed, err := eventWorker.ProcessScope(ctx, work.Scope); err != nil || processed != 1 {
					t.Fatalf("event resolution count=%d error=%v", processed, err)
				}
				resolved, err := store.GetAgentRun(ctx, work.Scope, work.ID)
				if err != nil || resolved.Status != AgentRunStatusQueued || resolved.ID != work.ID {
					t.Fatalf("queued continuation=%#v error=%v", resolved, err)
				}

				host := &workflowContinuationHost{}
				catalog := workflowContinuationCatalog{registry: registry, skills: skills, outreach: NewOutreachService(store.(OutreachStore), store.(ProjectStore), store.(SourceMonitorStore), store)}
				resolver := TurnRunnerResolverFunc(func(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
					return ResolveCatalogTurnRunner(ctx, catalog, run, CatalogTurnResolverConfig{Host: host, SkillHost: skill.HostCapabilityState{Adapters: map[string]skill.AdapterCapability{skill.AdapterConversation: {State: skill.AdapterStateAvailable, Features: []string{"plugin"}}}}})
				})
				pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: work.Scope, Kind: RunKindAgentWork, AssignedAgentID: work.AssignedAgentID, WorkerIDPrefix: "continuation-agent", Concurrency: 1, MaxTurnsPerClaim: 1, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				clock := func() time.Time { return now }
				pool.scheduler.now, pool.activity.now, pool.coordinator.activity.now, pool.coordinator.turns.now, pool.runCommands.now = clock, clock, clock, clock, clock
				outage := &workflowContinuationReportingOutage{ConversationStore: store.(ConversationStore)}
				pool.reportingStore = outage
				claim := AgentRunClaimRequest{Scope: work.Scope, Kind: RunKindAgentWork, AssignedAgentID: work.AssignedAgentID, WorkerID: "continuation-agent", LeaseDuration: time.Minute}
				claimed, err := pool.scheduler.ClaimNext(ctx, claim)
				if err != nil || claimed == nil || claimed.ID != work.ID {
					t.Fatalf("normal worker admission=%#v error=%v", claimed, err)
				}
				// Exercise exactly the production execution path with deterministic
				// admission/clock; no polling sleep or substitute TurnCoordinator.
				pool.executeClaim(ctx, claim.WorkerID, claimed)
				completed, err := store.GetAgentRun(ctx, work.Scope, work.ID)
				if err != nil || completed.Status != AgentRunStatusCompleted || len(host.requests) != 1 || outage.attempts != 1 {
					t.Fatalf("completed=%#v hosted turns=%d reporting attempts=%d error=%v", completed, len(host.requests), outage.attempts, err)
				}
				request := host.requests[0]
				origin, _ := request.InputContext["conversationRequest"].(map[string]interface{})
				trigger, err := store.(ConversationStore).GetChannelMessage(ctx, work.Scope, conversationID, triggerID)
				if err != nil || trigger == nil || request.RunID != work.ID || request.AgentID != work.AssignedAgentID || request.DefinitionVersion != "2.0.0" || request.Goal != args["goal"] || origin["conversationId"] != conversationID || origin["messageId"] != triggerID || origin["content"] != trigger.Content || !reflect.DeepEqual(request.ContinuationCheckpoint[runEventWaitCheckpointKey], resolved.Checkpoint[runEventWaitCheckpointKey]) {
					t.Fatalf("canonical continuation run=%s agent=%s definition=%s origin=%#v checkpoint=%#v trigger=%#v error=%v", request.RunID, request.AgentID, request.DefinitionVersion, origin, request.ContinuationCheckpoint, trigger, err)
				}
				currentTools := false
				for _, action := range request.Actions {
					if action.Action == RunbookActionCreateTask {
						t.Fatal("continuation received a revoked action")
					}
					if action.Action == RunbookActionInspectWorkflows && action.BindingRevision == bound.Binding.Revision+1 {
						currentTools = true
					}
				}
				if !currentTools {
					t.Fatalf("continuation did not resolve current authorized tools: %#v", request.Actions)
				}
				intent, err := store.(RunTerminalReportingStore).GetRunTerminalReport(ctx, work.Scope, work.ID, AgentRunStatusCompleted)
				if err != nil || intent.Run == nil || intent.DeliveredAt != nil || intent.Run.Output["summary"] != completed.Output["summary"] {
					t.Fatalf("atomic report intent=%#v error=%v", intent, err)
				}
				messages, err := store.(ConversationStore).ListChannelMessages(ctx, ChannelMessageFilter{Scope: work.Scope, ConversationID: conversationID})
				if err != nil || len(messages) != 1 {
					t.Fatalf("unavailable chat unexpectedly received result: %#v error=%v", messages, err)
				}
				if kind == "sqlite" {
					if err := store.(*SQLiteStore).Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := NewSQLiteStore(path)
					if err != nil {
						t.Fatal(err)
					}
					store = reopened
				}

				reporter, err := NewRunTerminalReportingWorker(store.(interface {
					RunTerminalReportingStore
					ConversationStore
				}), RunTerminalReportingWorkerOptions{WorkerID: "global-continuation-report", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
				reporter.now = clock
				project := reporter.project
				reporter.project = func(ctx context.Context, run *AgentRun) error {
					if err := project(ctx, run); err != nil {
						return err
					}
					return errors.New("delivery committed but acknowledgment was lost")
				}
				if delivered, err := reporter.ProcessBatch(ctx); err == nil || delivered != 0 {
					t.Fatalf("lost acknowledgment delivery count=%d error=%v", delivered, err)
				}
				intent, err = store.(RunTerminalReportingStore).GetRunTerminalReport(ctx, work.Scope, work.ID, AgentRunStatusCompleted)
				if err != nil || intent.DeliveredAt != nil || intent.Attempts != 1 {
					t.Fatalf("report retry intent=%#v error=%v", intent, err)
				}
				now = intent.AvailableAt.Add(time.Second)
				reporter.project = project
				if delivered, err := reporter.ProcessBatch(ctx); err != nil || delivered != 1 {
					t.Fatalf("durable global reporting count=%d error=%v", delivered, err)
				}

				// Provider redelivery and both worker retries must not schedule
				// another Turn, dispatch another action, or duplicate the result.
				if outcome != "timeout" {
					ingest()
					if provider.calls != 2 {
						t.Fatalf("provider redelivery calls=%d", provider.calls)
					}
				}
				eventWorker, err = NewRunEventWaitWorker(store.(RunEventWaitStore), RunEventWaitWorkerOptions{WorkerID: "continuation-retry", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
				eventWorker.now = clock
				if count, err := eventWorker.ProcessScope(ctx, work.Scope); err != nil || count != 0 {
					t.Fatalf("event retry count=%d error=%v", count, err)
				}
				if count, err := reporter.ProcessBatch(ctx); err != nil || count != 0 {
					t.Fatalf("report retry count=%d error=%v", count, err)
				}
				scheduler := NewAgentRunScheduler(store)
				scheduler.now = clock
				if run, err := scheduler.ClaimNext(ctx, claim); err != nil || run != nil {
					t.Fatalf("continuation was scheduled twice: run=%#v error=%v", run, err)
				}
				runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: work.Scope, Kind: RunKindAgentWork})
				if err != nil || len(runs) != 1 || runs[0].ID != work.ID {
					t.Fatalf("follow-up Runs=%#v error=%v", runs, err)
				}
				turns, err := store.ListAgentTurns(ctx, AgentTurnFilter{Scope: work.Scope, RunID: work.ID})
				if err != nil || len(turns) != 1 || len(host.requests) != 1 || turns[0].DefinitionVersion != "2.0.0" {
					t.Fatalf("follow-up Turns=%#v host calls=%d error=%v", turns, len(host.requests), err)
				}
				actions, err := store.ListActionCalls(ctx, ActionFilter{Scope: work.Scope, RunID: work.ID})
				if err != nil || len(actions) != 0 {
					t.Fatalf("reply summarization unexpectedly dispatched actions=%#v error=%v", actions, err)
				}
				messages, err = store.(ConversationStore).ListChannelMessages(ctx, ChannelMessageFilter{Scope: work.Scope, ConversationID: conversationID})
				if err != nil || len(messages) != 2 {
					t.Fatalf("original chat result count=%d error=%v", len(messages), err)
				}
				message := messages[1]
				wantSummary := "Received reply: " + provider.event.Text
				if outcome == "timeout" {
					wantSummary = "The requested reply did not arrive before the deadline."
				}
				if message.Content != wantSummary || message.Content != completed.Output["summary"] || message.Sender.ID != work.AssignedAgentID || message.ReplyToMessageID != triggerID || len(message.References) != 1 || message.References[0].ID != work.ID {
					t.Fatalf("original chat report=%#v saved output=%#v", message, completed.Output)
				}
				intent, err = store.(RunTerminalReportingStore).GetRunTerminalReport(ctx, work.Scope, work.ID, AgentRunStatusCompleted)
				if err != nil || intent.DeliveredAt == nil || intent.Attempts != 2 {
					t.Fatalf("acknowledged report=%#v error=%v", intent, err)
				}
			})
		}
	}
}

func TestDeferredWorkflowOriginRejectsForgedContext(t *testing.T) {
	store := NewMemoryStore()
	source, catalog, bound, args := workflowActionFixture(t, store)
	result, err := dispatchWorkflowForTest(t, store, catalog, source, bound, args, "verified-origin")
	if err != nil {
		t.Fatal(err)
	}
	work := result["run"].(*AgentRun)
	for _, change := range []string{"missing-source", "self-source", "foreign-owner", "foreign-agent", "foreign-scope", "foreign-conversation", "foreign-trigger", "wrong-kind", "wrong-flag"} {
		t.Run(change, func(t *testing.T) {
			candidate := cloneAgentRun(work)
			switch change {
			case "missing-source":
				candidate.Context["workflowSourceRunId"] = "missing-source"
			case "self-source":
				candidate.Context["workflowSourceRunId"] = work.ID
			case "foreign-owner":
				candidate.Owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "foreign-team"}
			case "foreign-agent":
				candidate.Owner.ID, candidate.AssignedAgentID = "foreign-agent", "foreign-agent"
			case "foreign-scope":
				candidate.Scope.ID = "foreign-tenant"
			case "foreign-conversation":
				candidate.Context[conversationRunContextConversationID] = "foreign-conversation"
			case "foreign-trigger":
				candidate.Context[conversationRunContextTriggerID] = "foreign-human-message"
			case "wrong-kind":
				candidate.Kind = RunKindConversation
			case "wrong-flag":
				candidate.Context[scheduledTaskContextKey] = false
			}
			origin, channel, human, err := conversationWorkOrigin(t.Context(), store, store, candidate)
			if err == nil || origin != nil || channel != nil || human != nil {
				t.Fatalf("forged origin exposed context: origin=%#v channel=%#v human=%#v error=%v", origin, channel, human, err)
			}
		})
	}
	t.Run("canonical-agent-message-is-not-a-human-trigger", func(t *testing.T) {
		conversationID := work.Context[conversationRunContextConversationID].(string)
		conversation, err := store.GetConversation(t.Context(), work.Scope, conversationID)
		if err != nil {
			t.Fatal(err)
		}
		posted, err := NewConversationService(store).PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: work.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: work.AssignedAgentID}, Intent: MessageIntentUpdate, Content: "Agent-authored text cannot become the pinned human request.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "agent-origin"})
		if err != nil {
			t.Fatal(err)
		}
		candidate := cloneAgentRun(work)
		candidate.Context[conversationRunContextTriggerID] = posted.Message.ID
		origin, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: work.Scope, Kind: RunKindConversation, Owner: work.Owner, AssignedAgentID: work.AssignedAgentID, Goal: "Handle the agent update", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: posted.Message.ID}})
		if err != nil {
			t.Fatal(err)
		}
		candidate.Context["workflowSourceRunId"] = origin.ID
		if _, _, _, err := conversationWorkOrigin(t.Context(), store, store, candidate); !errors.Is(err, ErrInvalidAgentRun) {
			t.Fatalf("non-human origin error=%v", err)
		}
	})
}
