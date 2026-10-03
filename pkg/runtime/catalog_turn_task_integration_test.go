package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type catalogTaskIntegrationHostFunc func(context.Context, HostedTurnRequest) (*HostedTurnResponse, error)

func (f catalogTaskIntegrationHostFunc) ExecuteHostedTurn(ctx context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return f(ctx, request)
}

func TestCatalogConversationTaskResolvesThroughForegroundHostAndWorker(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "catalog-task-integration"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Title: "Research assistant", IdempotencyKey: "catalog-task-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion,
		"Research the documentation in the background and report the result here.", "catalog-task-trigger")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, trigger.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "catalog-task-worker", Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "agent", Scope: skill.ScopeReference(scope), DefinitionID: "research-agent",
			ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "research-agent", Version: "1", Purpose: "Research documentation", SystemPrompt: "Be truthful."},
		activation: &skill.ActivationSnapshot{SnapshotID: "catalog-task-snapshot", Scope: skill.ScopeReference(scope), DeploymentID: "agent"},
	}
	proposal := &TurnTaskProposal{TaskKey: "documentation-research", Goal: strings.Repeat("文", 700), Acknowledgment: "I have started researching the documentation."}
	modelCalls := 0
	host := catalogTaskIntegrationHostFunc(func(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
		modelCalls++
		if request.ConversationTasks == nil || !request.ConversationTasks.CanStart || request.ConversationTasks.ConversationID != conversation.ID {
			t.Fatalf("actual foreground catalog resolution did not offer task admission: %#v", request.ConversationTasks)
		}
		if len(request.ConversationTasks.Tasks) != 0 {
			t.Fatalf("foreground claimed work before admission: %#v", request.ConversationTasks.Tasks)
		}
		schema, err := HostedTurnFormJSONSchema(request.Actions, HostedTurnFormAuthority{CanStartTask: request.ConversationTasks.CanStart})
		if err != nil {
			t.Fatal(err)
		}
		if _, offered := schema["properties"].(map[string]interface{})["proposedTask"]; !offered {
			t.Fatal("foreground model contract omitted task proposals")
		}
		source, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
		if err != nil || source == nil || source.Status != AgentRunStatusRunning || len(source.Output) != 0 {
			t.Fatalf("foreground acknowledged work before the model proposed admission: %#v %v", source, err)
		}
		return &HostedTurnResponse{
			APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID,
			ModelProvider: "test", Model: "catalog-task-model", Usage: TurnUsage{InputTokens: 10, OutputTokens: 5},
			NextRunStatus: AgentRunStatusRunning, ProposedTask: cloneTurnTaskProposal(proposal),
		}, nil
	})
	resolverCalls := 0
	agentResolver := TurnRunnerResolverFunc(func(ctx context.Context, lowerRun *AgentRun) (*TurnRunnerBinding, error) {
		resolverCalls++
		if lowerRun.ID != claimed.ID || lowerRun.Kind != RunKindAgentWork {
			t.Fatalf("foreground catalog projection was not the lowered source Run: %#v", lowerRun)
		}
		return ResolveCatalogTurnRunner(ctx, catalog, lowerRun, CatalogTurnResolverConfig{Host: host, ConversationTasks: store})
	})
	conversationRunner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, conversations),
		ConversationRunTurnRunnerConfig{AgentTurns: agentResolver})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, conversationRunner, nil, AgentRunWorkerConfig{Scope: scope, MaxTurnsPerClaim: 1})
	if err != nil {
		t.Fatal(err)
	}
	pool.executeClaim(t.Context(), "catalog-task-worker", claimed)
	completed, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
	if err != nil || completed == nil || completed.Status != AgentRunStatusCompleted || completed.Output["summary"] != proposal.Acknowledgment {
		t.Fatalf("actual worker did not atomically admit the task and complete its source: %#v %v", completed, err)
	}
	if modelCalls != 1 || resolverCalls != 1 {
		t.Fatalf("foreground work repeated resolution or model execution: resolver=%d model=%d", resolverCalls, modelCalls)
	}
	tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{
		Scope: scope, Owner: conversation.Owner, ConversationID: conversation.ID, AuthenticatedActor: trigger.Sender, Limit: 10,
	})
	if err != nil || len(tasks) != 1 || tasks[0].Task == nil || tasks[0].WorkRun == nil {
		t.Fatalf("worker did not persist exactly one independent task: %#v %v", tasks, err)
	}
	accepted := tasks[0]
	if accepted.Task.SourceRunID != claimed.ID || accepted.Task.SourceMessageID != trigger.ID || accepted.Task.AuthenticatedActor != trigger.Sender ||
		accepted.Task.TargetAgentID != claimed.AssignedAgentID || accepted.Task.Goal != proposal.Goal ||
		accepted.WorkRun.Status != AgentRunStatusQueued || accepted.WorkRun.ParentRunID != "" || accepted.WorkRun.RootRunID != accepted.WorkRun.ID ||
		accepted.WorkRun.ID == claimed.ID || accepted.WorkRun.Kind != RunKindAgentWork || accepted.WorkRun.Context[ConversationTaskContextKey] != accepted.Task.ID {
		t.Fatalf("admitted task lost independent execution or canonical provenance: %#v", accepted)
	}
	if completed.Output["conversationTaskWorkRunId"] != accepted.WorkRun.ID {
		t.Fatalf("foreground acknowledgment was not bound to accepted work: %#v", completed.Output)
	}
	backgroundBinding, err := ResolveCatalogTurnRunner(t.Context(), catalog, accepted.WorkRun, CatalogTurnResolverConfig{Host: host, ConversationTasks: store})
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogTaskIntegrationReadOnlyContext(t, backgroundBinding, accepted.WorkRun, accepted.Task)
	forgedHint := cloneAgentRun(accepted.WorkRun)
	forgedHint.Context[ConversationTaskContextKey] = "forged-task"
	forgedHint.Context["conversationTasks"] = map[string]interface{}{"conversationId": "forged-conversation", "canStart": true}
	forgedHintBinding, err := ResolveCatalogTurnRunner(t.Context(), catalog, forgedHint, CatalogTurnResolverConfig{Host: host, ConversationTasks: store})
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogTaskIntegrationReadOnlyContext(t, forgedHintBinding, forgedHint, accepted.Task)
	forgedBackground := cloneAgentRun(accepted.WorkRun)
	forgedBackground.Context = map[string]interface{}{
		conversationRunContextConversationID: "forged-conversation", conversationRunContextTriggerID: "forged-message",
		"threadRootMessageId": "forged-thread", ConversationTaskContextKey: "forged-task",
		"conversationTasks": map[string]interface{}{"conversationId": "forged-conversation", "canStart": true},
	}
	forgedBinding, err := ResolveCatalogTurnRunner(t.Context(), catalog, forgedBackground, CatalogTurnResolverConfig{Host: host, ConversationTasks: store})
	if err != nil {
		t.Fatal(err)
	}
	forgedRunner, ok := forgedBinding.Runner.(*HostedTurnRunner)
	if !ok {
		t.Fatalf("forged context resolved an unexpected runner: %T", forgedBinding.Runner)
	}
	forgedRequest, err := forgedRunner.buildRequest(TurnExecutionContext{Run: forgedBackground, Turn: &AgentTurn{ID: "inspect-forged-background-task"}})
	if err != nil {
		t.Fatal(err)
	}
	if forgedRequest.ConversationTasks != nil {
		t.Fatalf("copied conversation provenance injected typed task authority: %#v", forgedRequest.ConversationTasks)
	}
	if modelCalls != 1 {
		t.Fatalf("background context resolution unexpectedly invoked the model: %d calls", modelCalls)
	}
}

func assertCatalogTaskIntegrationReadOnlyContext(t *testing.T, binding *TurnRunnerBinding, run *AgentRun, task *ConversationTask) {
	t.Helper()
	if binding == nil {
		t.Fatal("background catalog binding is missing")
	}
	runner, ok := binding.Runner.(*HostedTurnRunner)
	if !ok {
		t.Fatalf("background catalog binding did not resolve the hosted runner: %T", binding.Runner)
	}
	request, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "inspect-background-task"}})
	if err != nil {
		t.Fatal(err)
	}
	state := request.ConversationTasks
	if state == nil || state.CanStart || state.ConversationID != task.ConversationID || len(state.Tasks) != 1 {
		t.Fatalf("background task did not receive canonical read-only context: %#v", state)
	}
	current := state.Tasks[0]
	if current.TaskID != task.ID || current.WorkRunID != task.WorkRunID || current.Status != AgentRunStatusQueued || current.Revision < 1 ||
		current.Goal != ConversationTaskGoalSummary(task.Goal) || utf8.RuneCountInString(current.Goal) > 512 {
		t.Fatalf("background task snapshot is not bounded canonical state: %#v", current)
	}
	schema, err := HostedTurnFormJSONSchema(request.Actions, HostedTurnFormAuthority{CanStartTask: state.CanStart})
	if err != nil {
		t.Fatal(err)
	}
	if _, offered := schema["properties"].(map[string]interface{})["proposedTask"]; offered {
		t.Fatal("background task was offered another independent task")
	}
}
