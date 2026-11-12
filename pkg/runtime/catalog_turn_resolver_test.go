package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

type resolverCatalog struct {
	deployment            *kernelagent.AgentDeployment
	deployments           []*kernelagent.AgentDeployment
	definition            *kernelagent.AgentDefinition
	definitions           map[string]*kernelagent.AgentDefinition
	teamDeployment        *kernelteam.Deployment
	teamDefinition        *kernelteam.Definition
	activation            *skill.ActivationSnapshot
	activations           map[string]*skill.ActivationSnapshot
	activationDeployments []string
	gotHost               skill.HostCapabilityState
	actors                []skill.RunActor
}

func TestRunbookSkillAuthorityExcludesConversationDeliveryTools(t *testing.T) {
	activation := &skill.ActivationSnapshot{SnapshotID: "snapshot", Skills: []skill.ActivatedSkill{
		{SkillID: "skill-browser", SkillVersion: "2", Actions: []capability.ModelAction{{SkillID: "skill-browser", Action: "commit"}}},
		{SkillID: "skill-slack", SkillVersion: "2", Actions: []capability.ModelAction{{SkillID: "skill-slack", Action: "slack-read-messages"}}},
	}}
	filtered, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: []interface{}{"skill-browser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Skills) != 1 || filtered.Skills[0].SkillID != "skill-browser" || len(activation.Skills) != 2 {
		t.Fatalf("filtered activation = %#v; original = %#v", filtered.Skills, activation.Skills)
	}
	empty, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: []interface{}{},
	})
	if err != nil || len(empty.Skills) != 0 {
		t.Fatalf("empty authority = %#v, %v", empty, err)
	}
	if _, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: "skill-browser",
	}); err == nil {
		t.Fatal("scalar Skill authority accepted")
	}
}

func (c *resolverCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return c.teamDeployment, nil
}

func (c *resolverCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return c.teamDefinition, nil
}

func (c *resolverCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.deployment, nil
}

func (c *resolverCatalog) ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	if c.deployments != nil {
		return c.deployments, nil
	}
	if c.deployment == nil {
		return nil, nil
	}
	return []*kernelagent.AgentDeployment{c.deployment}, nil
}

func (c *resolverCatalog) GetAgentDefinition(_ context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	if c.definitions != nil {
		return c.definitions[id+"@"+version], nil
	}
	return c.definition, nil
}

func (c *resolverCatalog) ActivateSkills(ctx context.Context, _ skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	c.gotHost = host
	actor, _ := skill.RunActorFromContext(ctx)
	c.actors = append(c.actors, actor)
	c.activationDeployments = append(c.activationDeployments, deploymentID)
	if c.activations != nil {
		return c.activations[deploymentID], nil
	}
	return c.activation, nil
}

func (c *resolverCatalog) GetActionCall(context.Context, Scope, string) (*ActionCall, error) {
	return nil, ErrActionNotFound
}

// loopBindingTestConfig is the minimal LoopBindingConfig of a test: the
// store's sessions, steps and calls and a tool dispatcher over them.
func loopBindingTestConfig(t testing.TB, store KernelStore, host StepHost) LoopBindingConfig {
	t.Helper()
	tools, err := NewStepToolDispatcher(StepToolDispatcherConfig{Sessions: store, Turns: store, Actions: store})
	if err != nil {
		t.Fatal(err)
	}
	return LoopBindingConfig{Host: host, Store: store, Tools: tools}
}

// bindingTestChatWorkspaces puts every Run in the chat its context names.
type bindingTestChatWorkspaces struct{}

func (bindingTestChatWorkspaces) PrepareChatWorkspace(_ context.Context, run *AgentRun) (string, error) {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	return conversationID, nil
}

// claimedBindingRun stores run as claimed by worker w1, so the resolver may
// create its session.
func claimedBindingRun(t *testing.T, store *sqlite.Store, run *AgentRun) *AgentRun {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	run.Status, run.LeaseOwner, run.LeaseExpiresAt, run.Revision = AgentRunStatusRunning, "w1", &expires, 1
	if run.Context == nil {
		run.Context = map[string]interface{}{}
	}
	if err := store.CreateAgentRun(context.Background(), cloneAgentRun(run)); err != nil {
		t.Fatalf("create run %s: %v", run.ID, err)
	}
	return run
}

// prepareBindingStep is the model request of the binding's first step.
func prepareBindingStep(t *testing.T, binding *TurnRunnerBinding, run *AgentRun) ModelStepRequest {
	t.Helper()
	if _, ok := binding.Producer.(*ModelStepProducer); !ok {
		t.Fatalf("binding producer = %T, want the model step producer", binding.Producer)
	}
	// The compactor builds its summary requests with the producer's own
	// preparer, so it prepares the step the producer would.
	compactor, ok := binding.Compactor.(*LoopCompactor)
	if !ok {
		t.Fatalf("binding compactor = %T, want the loop compactor", binding.Compactor)
	}
	req, err := compactor.Requests.PrepareModelStep(t.Context(), StepInput{
		Run: run, Step: &AgentTurn{ID: "step-1", RunID: run.ID, Sequence: 1}, Session: binding.Session, Binding: binding, Mode: StepModeNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func bindingToolNames(req ModelStepRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestResolveLoopBindingPerRunKind(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "kinds"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	action := capability.ModelAction{
		Name: "forum.reply", BindingID: "forum-account", BindingRevision: 3,
		SkillID: "forum", Version: "1", Action: "reply", SideEffect: capability.SideEffectExternal,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "agent", Scope: skillScope, DefinitionID: "agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "agent", Version: "1", Purpose: "Help", SystemPrompt: "Be truthful."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skillScope, DeploymentID: "agent", Skills: []skill.ActivatedSkill{{
			BindingID: "forum-account", BindingRevision: 3, SkillID: "forum", SkillVersion: "1", Name: "Forum", Actions: []capability.ModelAction{action},
		}}},
	}
	script := map[string]interface{}{
		"path": "scripts/weekly.py", "digest": "sha256:" + strings.Repeat("a", 64), "filesVersion": float64(2),
		"command": "python3 scripts/weekly.py", "timeoutSeconds": float64(120),
	}
	cases := []struct {
		name          string
		run           AgentRun
		deterministic bool
		producer      string
		finalMode     FinalAnswerMode
		fork          bool
		taskWork      bool
		actions       int
		instruction   string
		goalKind      string // "" = no seed
		hostTools     bool
	}{
		{name: "agent work", run: AgentRun{Goal: "Summarize the forum"},
			producer: "model", fork: true, actions: 1, goalKind: "routine"},
		{name: "routine", run: AgentRun{Goal: "Report the weekly numbers", Source: RunSourceSchedule, Context: map[string]interface{}{
			conversationRunContextConversationID: "chat-1", RoutineScriptContextKey: script,
		}}, producer: "model", fork: true, actions: 1, goalKind: "routine", hostTools: true},
		{name: "task work", run: AgentRun{Goal: "Research in the background", Source: RunSourceFork, Context: map[string]interface{}{
			ConversationTaskContextKey: "task-1",
		}}, producer: "model", taskWork: true, actions: 1, goalKind: "task"},
		{name: "fork", run: AgentRun{Goal: "Check one source", Source: RunSourceFork, Checkpoint: map[string]interface{}{
			"forkChild": map[string]interface{}{"groupId": "group", "branchId": "branch"},
		}}, producer: "model", actions: 1, goalKind: "fork_branch"},
		{name: "delegation execution", run: AgentRun{Goal: "Do the accepted work", Source: RunSourceRequest, Context: map[string]interface{}{
			"collaboration": map[string]interface{}{"requestId": "request"},
		}}, producer: "model", fork: true, actions: 1, instruction: acceptedAgentRequestExecutionSystemInstruction, goalKind: "delegation"},
		{name: "decision", run: AgentRun{Goal: "Evaluate incoming work", Source: RunSourceRequestDecision, Context: map[string]interface{}{
			AgentRequestInboxContextKey: map[string]interface{}{"requestId": "request", "requestRevision": int64(1)},
		}}, producer: "model", finalMode: FinalModeDecision, instruction: agentRequestDecisionSystemInstruction, goalKind: "decision"},
		{name: "review", run: AgentRun{Goal: "Review delegated work", Context: map[string]interface{}{
			AgentRequestCompletionReviewContextKey: map[string]interface{}{"requestId": "request"},
		}}, producer: "model", finalMode: FinalModeReview, instruction: agentRequestCompletionReviewSystemInstruction, goalKind: "review"},
		{name: "runbook child", run: AgentRun{Goal: "Draft the summary", Source: RunSourceRequest, Entrypoint: "draft", Context: map[string]interface{}{
			DelegationModeContextKey: string(runbook.DelegateReason),
		}}, producer: "model", fork: true, actions: 1, goalKind: "routine"},
		{name: "capability invocation", run: AgentRun{Goal: "Reply on the forum", Context: map[string]interface{}{
			"capabilityInvocation": map[string]interface{}{"skillId": "forum", "skillVersion": "1", "action": "reply"},
		}}, deterministic: true, producer: "capability", actions: 1},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestKernelStore(t)
			run := tc.run
			run.ID, run.Scope, run.Kind = fmt.Sprintf("run-%d", index), scope, RunKindAgentWork
			run.Owner, run.AssignedAgentID = ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, "agent"
			claimed := claimedBindingRun(t, store, &run)
			config := loopBindingTestConfig(t, store, &progressAckStepHost{})
			config.ChatWorkspaces = bindingTestChatWorkspaces{}
			binding, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(claimed), config)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.producer {
			case "model":
				if _, ok := binding.Producer.(*ModelStepProducer); !ok {
					t.Fatalf("producer = %T", binding.Producer)
				}
			case "capability":
				if _, ok := binding.Producer.(*CapabilityInvocationStepProducer); !ok {
					t.Fatalf("producer = %T", binding.Producer)
				}
			}
			if binding.Tools == nil || binding.Final != nil || binding.Session == nil || binding.Host == nil {
				t.Fatalf("loop seams: tools=%v final=%v session=%v host=%v", binding.Tools, binding.Final, binding.Session, binding.Host)
			}
			if len(binding.ModelActions) != tc.actions {
				t.Fatalf("model actions = %#v", binding.ModelActions)
			}
			authority := binding.Authority
			if authority.AgentID != "agent" || authority.finalMode() != tc.finalMode && !(tc.finalMode == "" && authority.finalMode() == FinalModeDefault) ||
				authority.ForkAllowed != tc.fork || authority.TaskWork != tc.taskWork {
				t.Fatalf("authority = %#v", authority)
			}
			if tc.deterministic {
				if authority.CanReportProgress || authority.CanWait || binding.Compactor != nil || len(binding.Preparers) != 0 || binding.ProgressAcknowledgementRenderer != nil {
					t.Fatalf("deterministic binding offers model seams: %#v", binding)
				}
			} else if binding.Compactor == nil || len(binding.Preparers) != 1 || binding.ProgressAcknowledgementRenderer == nil {
				t.Fatalf("model binding lacks compaction, run start or progress acknowledgements: %#v", binding)
			}
			if tc.instruction != "" && !slices.Contains(binding.Prompt.AgentInstructions, tc.instruction) {
				t.Fatalf("prompt = %#v", binding.Prompt)
			}
			for _, other := range []string{agentRequestDecisionSystemInstruction, agentRequestCompletionReviewSystemInstruction, acceptedAgentRequestExecutionSystemInstruction} {
				if other != tc.instruction && slices.Contains(binding.Prompt.AgentInstructions, other) {
					t.Fatalf("instruction of another kind leaked: %#v", binding.Prompt)
				}
			}

			// The Run's first claim created its parentless run session.
			stored, err := store.GetAgentRun(t.Context(), scope, run.ID)
			if err != nil || binding.Session.ID != RunAgentSessionID(scope, run.ID) || stored.SessionID != binding.Session.ID ||
				binding.Session.Kind != AgentSessionKindRun || binding.Session.ParentSessionID != "" {
				t.Fatalf("session = %#v stored=%#v err=%v", binding.Session, stored, err)
			}
			entries, err := store.ListAgentSessionEntries(t.Context(), SessionEntryFilter{Scope: scope, SessionID: binding.Session.ID})
			if err != nil {
				t.Fatal(err)
			}
			if tc.goalKind == "" {
				if len(entries) != 0 {
					t.Fatalf("deterministic session seeded: %#v", entries)
				}
			} else if len(entries) != 1 || entries[0].User == nil || entries[0].User.Origin != UserEntryGoal || entries[0].User.GoalKind != tc.goalKind {
				t.Fatalf("seed = %#v", entries)
			}
			// The next claim keeps the session.
			again, err := ResolveLoopBinding(t.Context(), catalog, stored, config)
			if err != nil || again.Session == nil || again.Session.ID != binding.Session.ID {
				t.Fatalf("second claim session = %#v err=%v", again, err)
			}

			if tc.producer != "model" {
				return
			}
			req := prepareBindingStep(t, binding, claimed)
			tools := bindingToolNames(req)
			hostTool := slices.Contains(tools, hostToolName(workspace.OperationRunCommand))
			if hostTool != tc.hostTools {
				t.Fatalf("host tools offered=%v want %v: %v", hostTool, tc.hostTools, tools)
			}
			routineSection := false
			for _, section := range req.System.Sections {
				routineSection = routineSection || section.ID == loopSectionRoutineScript
			}
			if routineSection != (tc.name == "routine") {
				t.Fatalf("routine script section = %v: %#v", routineSection, req.System.Sections)
			}
			if !slices.Contains(req.System.AgentInstructions, "Be truthful.") {
				t.Fatalf("agent instructions = %#v", req.System.AgentInstructions)
			}
		})
	}
}

func TestDecisionRunOffersOnlyDecisionToolViaBinding(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "reviewer", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "reviewer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "model-binding"}},
		},
		definition: &kernelagent.AgentDefinition{
			ID: "reviewer", Version: "1", Purpose: "Review launches", SystemPrompt: "Be precise.",
			Runbook: &runbook.Definition{APIVersion: runbook.APIVersion, ID: "normal-work", Version: "1",
				Interfaces: map[string]runbook.Interface{"go": {Description: "Go", InputSchema: map[string]interface{}{"type": "object"}}}},
		},
	}
	for _, tc := range []struct {
		key, tool, instruction string
		value                  map[string]interface{}
	}{
		{AgentRequestInboxContextKey, string(ControlToolRequestDecision), agentRequestDecisionSystemInstruction, map[string]interface{}{"requestId": "request", "requestRevision": int64(1)}},
		{AgentRequestCompletionReviewContextKey, string(ControlToolCompletionReview), agentRequestCompletionReviewSystemInstruction, map[string]interface{}{"requestId": "request"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			catalog.activationDeployments = nil
			store := newTestKernelStore(t)
			run := claimedBindingRun(t, store, &AgentRun{
				ID: "decision", Scope: scope, Kind: RunKindAgentWork, Source: RunSourceRequestDecision,
				Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reviewer"}, AssignedAgentID: "reviewer",
				Goal: "Evaluate incoming work", Context: map[string]interface{}{tc.key: tc.value, conversationRunContextConversationID: "chat-1"},
			})
			config := loopBindingTestConfig(t, store, &progressAckStepHost{})
			config.ChatWorkspaces = bindingTestChatWorkspaces{}
			binding, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(run), config)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.activationDeployments) != 0 || len(binding.ModelActions) != 0 || len(binding.RunbookOperations) != 0 ||
				!reflect.DeepEqual(binding.InputContextRefs, []string{"run:" + tc.key}) {
				t.Fatalf("structured Run binding = %#v activations=%v", binding, catalog.activationDeployments)
			}
			req := prepareBindingStep(t, binding, run)
			if tools := bindingToolNames(req); !reflect.DeepEqual(tools, []string{tc.tool}) {
				t.Fatalf("offered tools = %v, want only %s", tools, tc.tool)
			}
			if len(req.System.SkillPrompts) != 0 || !slices.Contains(req.System.AgentInstructions, tc.instruction) ||
				!slices.Contains(req.System.AgentInstructions, "Be precise.") {
				t.Fatalf("system = %#v", req.System)
			}
			if req.ModelCredential == nil || req.ModelCredential.ID != "model-binding" || binding.ModelCredential == nil || binding.ModelCredential.ID != "model-binding" {
				t.Fatalf("model credential request=%#v binding=%#v", req.ModelCredential, binding.ModelCredential)
			}
		})
	}
}

func TestCatalogTurnResolverSelectsDeterministicKernelRunnersAndFailsClosedWithoutHost(t *testing.T) {
	scope := Scope{Kind: "local", ID: "research"}
	action := capability.ModelAction{
		Name: "forum.reply", BindingID: "forum-account", BindingRevision: 3,
		SkillID: "forum", Version: "1", Action: "reply", SideEffect: capability.SideEffectExternal,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "researcher", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "researcher", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "researcher", Version: "1", Purpose: "Research communities", SystemPrompt: "Be truthful."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-1", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", Skills: []skill.ActivatedSkill{{
			BindingID: "forum-account", BindingRevision: 3, SkillID: "forum", SkillVersion: "1", Name: "Forum", Actions: []capability.ModelAction{action},
		}}},
	}
	base := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Context: map[string]interface{}{}}
	config := loopBindingTestConfig(t, newTestKernelStore(t), nil)

	invocation := cloneAgentRun(base)
	invocation.Context = map[string]interface{}{}
	invocation.Context["capabilityInvocation"] = map[string]interface{}{"skillId": "forum", "skillVersion": "1", "action": "reply"}
	binding, err := ResolveLoopBinding(t.Context(), catalog, invocation, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binding.Producer.(*CapabilityInvocationStepProducer); !ok || len(binding.ModelActions) != 1 {
		t.Fatalf("capability binding = %#v", binding)
	}
	// Outside a claim nothing is written: no session.
	if binding.Session != nil || binding.Tools == nil {
		t.Fatalf("unclaimed binding session=%#v tools=%v", binding.Session, binding.Tools)
	}
	// The Agent's activation acts for the run, so host bindings for it apply.
	if got := catalog.actors[len(catalog.actors)-1]; got != (skill.RunActor{RunID: "run", AgentID: "researcher", Umbrella: skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "researcher"}}) {
		t.Fatalf("activation actor = %#v", got)
	}

	_, err = ResolveLoopBinding(t.Context(), catalog, base, config)
	if !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("unhosted prompt work error = %v", err)
	}
	if _, err := ResolveLoopBinding(t.Context(), catalog, base, LoopBindingConfig{Host: &progressAckStepHost{}}); err == nil {
		t.Fatal("binding without a kernel store and tool dispatcher was accepted")
	}
}

// progressAcknowledgementInstruction is the system instruction of every
// progress acknowledgement step.
const progressAcknowledgementInstruction = "Produce only a brief progress acknowledgement in the Agent's configured personality. Use only the verified facts in the user message. Write at most two short sentences and 100 characters. Do not claim completion, expose hidden reasoning, propose actions, call tools, or add unrelated explanation."

func TestLoopBindingOffersHostToolsOnlyInAChat(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "11"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "coding-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "coding-agent",
			ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		},
		definition: &kernelagent.AgentDefinition{ID: "coding-agent", Version: "1", Purpose: "Work in repositories", SystemPrompt: "Work carefully."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "coding-agent"},
	}
	store := newTestKernelStore(t)
	run := claimedBindingRun(t, store, &AgentRun{
		ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coding-agent"},
		AssignedAgentID: "coding-agent", Goal: "Inspect the repository",
	})
	stepHost := &progressAckStepHost{reply: func(req ModelStepRequest) ModelStepResult { return progressAckResult(req, "done") }}
	config := loopBindingTestConfig(t, store, stepHost)
	config.ChatWorkspaces = bindingTestChatWorkspaces{}
	binding, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(run), config)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := binding.ProgressAcknowledgementRenderer.RenderRunProgressAcknowledgement(t.Context(), RunProgressAcknowledgementRequest{
		Snapshot:     RunProgressSnapshot{RunID: run.ID, Scope: run.Scope, AgentID: run.AssignedAgentID, Goal: run.Goal, Status: AgentRunStatusRunning, Revision: 1},
		MaxSentences: 2, MaxCharacters: 100,
	})
	if err != nil || progress != "done" || len(stepHost.requests) != 1 {
		t.Fatalf("progress=%q requests=%d error=%v", progress, len(stepHost.requests), err)
	}
	if ack := stepHost.requests[0]; ack.Tools != nil || ack.SessionID != "" || ack.Purpose != ModelStepPurposeAcknowledgement || len(ack.System.SkillPrompts) != 0 ||
		!slices.Contains(ack.System.AgentInstructions, "Work carefully.") ||
		!slices.Contains(ack.System.AgentInstructions, progressAcknowledgementInstruction) {
		t.Fatalf("progress acknowledgement request = %#v", ack)
	}
	req := prepareBindingStep(t, binding, run)
	if !slices.Contains(req.System.AgentInstructions, agentResourcefulnessGuidance) {
		t.Fatal("model work did not receive resourcefulness guidance")
	}
	for _, name := range bindingToolNames(req) {
		if strings.HasPrefix(name, "workspace_") {
			t.Fatalf("work outside a chat has files: %v", bindingToolNames(req))
		}
	}

	chatStore := newTestKernelStore(t)
	chat := claimedBindingRun(t, chatStore, &AgentRun{
		ID: "chat-run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coding-agent"},
		AssignedAgentID: "coding-agent", Goal: "Inspect the repository", Context: map[string]interface{}{conversationRunContextConversationID: "chat-1"},
	})
	chatConfig := loopBindingTestConfig(t, chatStore, stepHost)
	chatConfig.ChatWorkspaces = bindingTestChatWorkspaces{}
	chatBinding, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(chat), chatConfig)
	if err != nil {
		t.Fatal(err)
	}
	if tools := bindingToolNames(prepareBindingStep(t, chatBinding, chat)); !slices.Contains(tools, hostToolName(workspace.OperationReadFile)) {
		t.Fatalf("chat Run lacks its chat files: %v", tools)
	}
	// Without a resolver no Run is offered host tools.
	chatConfig.ChatWorkspaces = nil
	plain, err := ResolveLoopBinding(t.Context(), catalog, cloneAgentRun(chat), chatConfig)
	if err != nil {
		t.Fatal(err)
	}
	if tools := bindingToolNames(prepareBindingStep(t, plain, chat)); slices.Contains(tools, hostToolName(workspace.OperationReadFile)) {
		t.Fatalf("host tools without a chat workspace resolver: %v", tools)
	}
}

func TestCatalogTurnResolverUsesDeploymentSpecificSkillHost(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "sre", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "sre", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "sre", Version: "1", Purpose: "Operate safely", SystemPrompt: "Use bounded tools."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-host", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "sre"},
	}
	wanted := skill.HostCapabilityState{OperatingSystem: "linux", Architecture: "arm64", Revision: "enterprise-host/v1"}
	var resolvedScope skill.ScopeReference
	var resolvedDeployment string
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}, AssignedAgentID: "sre", Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{}}}
	config := loopBindingTestConfig(t, newTestKernelStore(t), nil)
	config.SkillHosts = SkillHostCapabilityResolverFunc(func(_ context.Context, gotScope skill.ScopeReference, deploymentID string) (*skill.HostCapabilityState, error) {
		resolvedScope, resolvedDeployment = gotScope, deploymentID
		return &wanted, nil
	})
	_, err := ResolveLoopBinding(t.Context(), catalog, run, config)
	if err == nil || !strings.Contains(err.Error(), "requires authorized actions") {
		t.Fatalf("expected deterministic runner validation after host resolution, got %v", err)
	}
	if resolvedScope != (skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}) || resolvedDeployment != "sre" || !reflect.DeepEqual(catalog.gotHost, wanted) {
		t.Fatalf("scope=%#v deployment=%q host=%#v", resolvedScope, resolvedDeployment, catalog.gotHost)
	}
}

func TestCatalogTurnResolverCarriesOpaqueDeploymentModelCredentialOnlyToHost(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "analyst", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "private-model-reference-17"}},
		},
		definition: &kernelagent.AgentDefinition{ID: "analyst", Version: "1", Purpose: "Analyze", SystemPrompt: "Work carefully."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "analyst"},
	}
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, AssignedAgentID: "analyst", Goal: "Analyze", Context: map[string]interface{}{}}
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	req := prepareBindingStep(t, binding, run)
	if req.ModelCredential == nil || req.ModelCredential.Kind != "model-provider" || req.ModelCredential.ID != "private-model-reference-17" ||
		binding.ModelCredential == nil || binding.ModelCredential.ID != "private-model-reference-17" {
		t.Fatalf("model credential = %#v", req.ModelCredential)
	}
	modelInput, err := json.Marshal(struct {
		System  ModelStepSystem
		Tools   []ModelStepTool
		Entries []SessionEntryView
	}{req.System, req.Tools, req.Entries})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(modelInput), "private-model-reference-17") || strings.Contains(string(modelInput), "MODEL_PROVIDER") {
		t.Fatalf("model input leaked credential reference: %s", modelInput)
	}
}

func TestCatalogTurnResolverHidesOriginatingRunbookFromDelegatedAgent(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "daily-browser-task", Version: "1.2.3", Name: "Daily browser task",
		Interfaces: map[string]runbook.Interface{"dailyOccurrence": {
			Description:  "Run the daily browser task",
			InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false},
			OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": true},
		}},
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "browser", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "browser", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "model-binding"}},
		},
		definition: &kernelagent.AgentDefinition{ID: "browser", Version: "1", Purpose: "Browse", SystemPrompt: "Use bounded tools.", Runbook: definition},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "browser"},
	}
	run := &AgentRun{
		ID: "child", Scope: scope, Kind: RunKindAgentWork, Source: RunSourceRequest,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser"}, AssignedAgentID: "browser", Goal: "Perform delegated work",
		Context: map[string]interface{}{"triggerInput": map[string]interface{}{
			"runbookDefinitionId": "daily-browser-task", "runbookDefinitionVersion": "1.2.3",
		}},
	}
	config := loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{})
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.RunbookOperations) != 0 || len(binding.Authority.Operations) != 0 ||
		slices.Contains(bindingToolNames(prepareBindingStep(t, binding, run)), string(ControlToolRunOperation)) {
		t.Fatalf("originating Runbook remained callable: %#v", binding.RunbookOperations)
	}

	run.Context["triggerInput"].(map[string]interface{})["runbookDefinitionVersion"] = "different"
	binding, err = ResolveLoopBinding(t.Context(), catalog, run, config)
	if err != nil || len(binding.RunbookOperations) != 1 || len(binding.Authority.Operations) != 1 {
		t.Fatalf("unrelated Runbook identity was filtered: binding=%#v error=%v", binding, err)
	}
}

func TestCatalogTurnResolverPrefersDefinitionOperationWithoutCallableActivation(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "community-engagement", Version: "1", Name: "Community engagement",
		Interfaces: map[string]runbook.Interface{"engage-now": {
			Description:  "Run the reviewed engagement operation",
			InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false},
			OutputSchema: map[string]interface{}{"type": "object"},
		}},
	}
	start := capability.ModelAction{
		Name: "openseal.runbooks.start", SkillID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Action: RunbookActionStart, BindingID: "bundled:runbooks", BindingRevision: 1,
	}
	replace := capability.ModelAction{
		Name: "openseal.runbooks.replace_schedule", SkillID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Action: RunbookActionReplaceSchedule, BindingID: "bundled:runbooks", BindingRevision: 1,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "community-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "community-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		},
		definition: &kernelagent.AgentDefinition{
			ID: "community-agent", Version: "1", Purpose: "Engage", SystemPrompt: "Act carefully.", Runbook: definition,
		},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "community-agent",
			Skills: []skill.ActivatedSkill{{
				BindingID: "bundled:runbooks", BindingRevision: 1, SkillID: RunbookManagementSkillID,
				SkillVersion: RunbookManagementSkillVersion, Name: "Runbooks", Actions: []capability.ModelAction{start, replace},
			}},
		},
	}
	run := &AgentRun{
		ID: "conversation-turn", Scope: scope, Kind: RunKindAgentWork,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "community-agent"}, AssignedAgentID: "community-agent",
		Goal: "Run it now", Context: map[string]interface{}{},
	}
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.RunbookOperations) != 1 || binding.RunbookOperations[0].Entrypoint != "engage-now" ||
		len(binding.ModelActions) != 1 || binding.ModelActions[0].Action != RunbookActionReplaceSchedule ||
		len(binding.Authority.Operations) != 1 {
		t.Fatalf("conversation capabilities = %#v", binding)
	}
}

func TestCatalogTurnResolverProjectsActiveSameScopeDelegationCatalog(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	current := &kernelagent.AgentDeployment{
		ID: "lead", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: "lead-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
	}
	reviewer := &kernelagent.AgentDeployment{
		ID: "reviewer-7", Scope: current.Scope,
		DefinitionID: "reviewer-definition", ActiveVersion: "2", RolloutStatus: kernelagent.RolloutActive,
	}
	paused := &kernelagent.AgentDeployment{
		ID: "paused-agent", Scope: current.Scope,
		DefinitionID: "paused-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutPaused,
	}
	catalog := &resolverCatalog{
		deployment:  current,
		deployments: []*kernelagent.AgentDeployment{paused, reviewer, current},
		definition:  &kernelagent.AgentDefinition{ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate", SystemPrompt: "Coordinate safely."},
		definitions: map[string]*kernelagent.AgentDefinition{
			"lead-definition@1": {
				ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate", SystemPrompt: "Coordinate safely.",
			},
			"reviewer-definition@2": {
				ID: "reviewer-definition", Version: "2", DisplayName: "Release Reviewer", Purpose: "Review releases", SystemPrompt: "Review safely.",
			},
		},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID,
		},
		teamDeployment: &kernelteam.Deployment{
			ID: "release-team", Scope: current.Scope, DefinitionID: "release-team", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Roster: []kernelteam.RosterAssignment{
				{ID: "lead", AgentDeploymentID: current.ID},
				{ID: "reviewer", AgentDeploymentID: reviewer.ID},
			},
		},
		teamDefinition: &kernelteam.Definition{
			ID: "release-team", Version: "1",
		},
		activations: map[string]*skill.ActivationSnapshot{
			current.ID:     {SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID},
			"release-team": {SnapshotID: "team-snapshot", Scope: current.Scope, DeploymentID: "release-team"},
		},
	}
	run := &AgentRun{
		ID: "run", Scope: scope, Kind: RunKindAgentWork,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: current.ID,
		Goal: "Delegate a release review", Context: map[string]interface{}{},
	}
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	want := HostedAgentTarget{ID: "reviewer-7", DisplayName: "Release Reviewer", Purpose: "Review releases"}
	if targets := binding.Authority.DelegateTargets; len(targets) != 1 || targets[0] != want {
		t.Fatalf("delegation targets = %#v", targets)
	}
	if targets := binding.Authority.ForkTargets; len(targets) != 1 || targets[0] != want {
		t.Fatalf("fork targets = %#v", targets)
	}
}

func TestCatalogTurnResolverDoesNotProjectTenantDirectoryIntoStandaloneAgent(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	current := &kernelagent.AgentDeployment{ID: "standalone", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "standalone", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	deployments := []*kernelagent.AgentDeployment{current}
	definitions := map[string]*kernelagent.AgentDefinition{
		"standalone@1": {ID: "standalone", Version: "1", DisplayName: "Standalone", Purpose: "Work independently", SystemPrompt: "Work safely."},
	}
	for index := 0; index < 200; index++ {
		id := fmt.Sprintf("tenant-agent-%03d", index)
		definitionID := id + "-definition"
		deployments = append(deployments, &kernelagent.AgentDeployment{ID: id, Scope: current.Scope, DefinitionID: definitionID, ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive})
		definitions[definitionID+"@1"] = &kernelagent.AgentDefinition{ID: definitionID, Version: "1", DisplayName: id, Purpose: strings.Repeat("unrelated tenant purpose ", 8)}
	}
	catalog := &resolverCatalog{
		deployment: current, deployments: deployments, definitions: definitions,
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID},
	}
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: current.ID}, AssignedAgentID: current.ID, Goal: "Answer concisely", Context: map[string]interface{}{}}
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Authority.DelegateTargets) != 0 || len(binding.Authority.ForkTargets) != 0 {
		t.Fatalf("standalone Agent received tenant directory: %#v", binding.Authority)
	}
	if tools := bindingToolNames(prepareBindingStep(t, binding, run)); slices.Contains(tools, string(ControlToolDelegate)) {
		t.Fatalf("standalone Agent offered delegation: %v", tools)
	}
}

func TestCatalogTurnResolverSeparatesAcceptedRequestExecutionFromIntake(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	action := capability.ModelAction{
		Name: "browser.snapshot", BindingID: "browser", BindingRevision: 1,
		SkillID: "skill-browser", Version: "1", Action: "snapshot", SideEffect: capability.SideEffectRead,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "browser-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "browser-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		},
		definition: &kernelagent.AgentDefinition{ID: "browser-agent", Version: "1", Purpose: "Browse safely", SystemPrompt: "Use evidence."},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "browser-agent",
			Skills: []skill.ActivatedSkill{{BindingID: "browser", BindingRevision: 1, SkillID: "skill-browser", SkillVersion: "1", Actions: []capability.ModelAction{action}}},
		},
	}
	run := &AgentRun{
		ID: "execution", Scope: scope, Kind: RunKindAgentWork, Source: RunSourceRequest,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser-agent"}, AssignedAgentID: "browser-agent",
		Goal: "Perform the accepted request", Context: map[string]interface{}{"collaboration": map[string]interface{}{"requestId": "request"}},
	}
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	req := prepareBindingStep(t, binding, run)
	tools := bindingToolNames(req)
	if !slices.Contains(req.System.AgentInstructions, acceptedAgentRequestExecutionSystemInstruction) ||
		slices.Contains(req.System.AgentInstructions, agentRequestDecisionSystemInstruction) ||
		slices.Contains(tools, string(ControlToolRequestDecision)) || !slices.Contains(tools, string(ControlToolFinalAnswer)) ||
		len(binding.ModelActions) != 1 {
		t.Fatalf("accepted execution system=%#v tools=%v", req.System.AgentInstructions, tools)
	}
}

func TestCatalogTurnResolverProjectsTeamOwnedActionsWithoutLeakingThemToAgentRuns(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	teamAction := capability.ModelAction{
		Name: "openseal.teams.update_role", BindingID: "teams", BindingRevision: 2,
		SkillID: "openseal.teams", Version: "1.0.0", Action: "update_role", Risk: capability.RiskLevelWrite,
	}
	agentAction := teamAction
	agentAction.BindingID = "agent-teams"
	agentAction.BindingRevision = 1
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "reviewer-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "reviewer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "reviewer", Version: "1", Purpose: "Review", SystemPrompt: "Review carefully.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, AllowedSkillIDs: []string{"openseal.teams"}}},
		teamDeployment: &kernelteam.Deployment{
			ID: "review-team", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "review-team", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Revision: 1, Roster: []kernelteam.RosterAssignment{{ID: "reviewer", AgentDeploymentID: "reviewer-agent"}},
		},
		teamDefinition: &kernelteam.Definition{
			ID: "review-team", Version: "1", Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
		},
		activations: map[string]*skill.ActivationSnapshot{
			"reviewer-agent": {SnapshotID: "agent-snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "reviewer-agent", Skills: []skill.ActivatedSkill{{
				BindingID: "agent-teams", BindingRevision: 1, SkillID: "openseal.teams", SkillVersion: "1.0.0", Name: "Teams", Actions: []capability.ModelAction{agentAction},
			}}},
			"review-team": {SnapshotID: "team-snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "review-team", Skills: []skill.ActivatedSkill{{
				BindingID: "teams", BindingRevision: 2, SkillID: "openseal.teams", SkillVersion: "1.0.0", Name: "Teams", Actions: []capability.ModelAction{teamAction},
			}}},
		},
	}
	config := loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{})
	teamRun := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "review-team"}, AssignedAgentID: "reviewer-agent", Goal: "Enable the reviewer", Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{}}
	binding, err := ResolveLoopBinding(t.Context(), catalog, teamRun, config)
	if err != nil {
		t.Fatal(err)
	}
	if binding.DeploymentID != "reviewer-agent" || binding.ActionDeploymentID != "reviewer-agent" || len(binding.ModelActions) != 1 || binding.ModelActions[0].BindingID != "teams" || binding.ModelActions[0].DeploymentID != "review-team" || !reflect.DeepEqual(catalog.activationDeployments, []string{"reviewer-agent", "review-team"}) {
		t.Fatalf("Team binding = %#v activations=%v", binding, catalog.activationDeployments)
	}
	if req := prepareBindingStep(t, binding, teamRun); req.AgentID != "reviewer-agent" || binding.Authority.AgentID != "reviewer-agent" {
		t.Fatalf("Team model identity = %q authority=%q", req.AgentID, binding.Authority.AgentID)
	}

	catalog.activationDeployments = nil
	agentRun := cloneAgentRun(teamRun)
	agentRun.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "reviewer-agent"}
	agentBinding, err := ResolveLoopBinding(t.Context(), catalog, agentRun, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.activationDeployments, []string{"reviewer-agent"}) || agentBinding.ActionDeploymentID != "reviewer-agent" || agentBinding.ModelActions[0].BindingID != "agent-teams" {
		t.Fatalf("Agent run binding=%#v activations=%v", agentBinding, catalog.activationDeployments)
	}
}

func TestProjectActivatedSkillsPrefersCanonicalKernelManagementBinding(t *testing.T) {
	action := func(bindingID string, revision int64) capability.ModelAction {
		return capability.ModelAction{
			Name:      AgentManagementSkillID + "." + AgentActionAmendBehavior,
			BindingID: bindingID, BindingRevision: revision, DeploymentID: "agent-1",
			SkillID: AgentManagementSkillID, Version: AgentManagementSkillVersion, Action: AgentActionAmendBehavior,
		}
	}
	activation := &skill.ActivationSnapshot{
		SnapshotID: "snapshot-management", DeploymentID: "agent-1",
		Skills: []skill.ActivatedSkill{
			{
				BindingID: "workforce:agent-1:" + AgentManagementSkillID, BindingRevision: 1,
				SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
				Actions: []capability.ModelAction{action("workforce:agent-1:"+AgentManagementSkillID, 1)},
			},
			{
				BindingID: "bundled:agents", BindingRevision: 2,
				SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
				Actions: []capability.ModelAction{action("bundled:agents", 2)},
			},
		},
	}

	_, actions, prepared, refs, err := projectActivatedSkills(activation)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].BindingID != "bundled:agents" || actions[0].BindingRevision != 2 {
		t.Fatalf("management actions = %#v", actions)
	}
	if len(prepared) != 0 || len(refs) != 1 {
		t.Fatalf("prepared = %#v, refs = %#v", prepared, refs)
	}
}

func TestProjectActivatedSkillsPreservesExplicitVersionOverChatDefaults(t *testing.T) {
	action := func(binding, version, name string) capability.ModelAction {
		return capability.ModelAction{Name: "openseal.builtin." + name, SkillID: "openseal.builtin", Version: version, Action: name, BindingID: binding, BindingRevision: 1}
	}
	bundled := skill.ActivatedSkill{BindingID: "bundled:builtin", SkillID: "openseal.builtin", SkillVersion: "1.5.0", Actions: []capability.ModelAction{action("bundled:builtin", "1.5.0", "read_attachment"), action("bundled:builtin", "1.5.0", "generate_image")}}
	explicit := skill.ActivatedSkill{BindingID: "workforce:agent:openseal.builtin", SkillID: "openseal.builtin", SkillVersion: "1.4.4", Actions: []capability.ModelAction{action("workforce:agent:openseal.builtin", "1.4.4", "read_attachment")}}
	for _, skills := range [][]skill.ActivatedSkill{{bundled, explicit}, {explicit, bundled}} {
		_, actions, _, _, err := projectActivatedSkills(&skill.ActivationSnapshot{DeploymentID: "agent", Skills: skills})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 2 {
			t.Fatalf("expected explicit overlap and default-only action, got %#v", actions)
		}
		for _, a := range actions {
			if a.Action == "read_attachment" && (a.BindingID != explicit.BindingID || a.Version != "1.4.4") {
				t.Fatalf("explicit version was replaced: %#v", a)
			}
		}
	}
	other := explicit
	other.BindingID = "other-explicit"
	other.Actions = []capability.ModelAction{action(other.BindingID, "1.4.4", "read_attachment")}
	// Distinct explicit bindings may carry different credentials or authority;
	// exposing one name for both is refused rather than routed to either.
	if _, _, _, _, err := projectActivatedSkills(&skill.ActivationSnapshot{DeploymentID: "agent", Skills: []skill.ActivatedSkill{explicit, other}}); !errors.Is(err, skill.ErrBindingAmbiguous) {
		t.Fatalf("duplicate explicit binding names were not refused: %v", err)
	}
}

func TestPinnedRunbookPlanPreventsDefinitionAndTriggerDrift(t *testing.T) {
	definition := &runbook.Definition{
		ID: "chat", Version: "2",
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "respond", ObjectiveID: "agent:agent:respond",
		}},
	}
	run := &AgentRun{
		Entrypoint: "respond",
		Plan: map[string]interface{}{"runbook": map[string]interface{}{
			"id": "chat", "version": "2", "trigger": "on-message",
		}},
	}
	if err := validatePinnedRunbookPlan(definition, run); err != nil {
		t.Fatal(err)
	}
	run.Plan["runbook"].(map[string]interface{})["version"] = "3"
	if err := validatePinnedRunbookPlan(definition, run); err == nil {
		t.Fatal("Runbook version drift was accepted")
	}
	run.Plan["runbook"].(map[string]interface{})["version"] = "2"
	run.Entrypoint = "other"
	if err := validatePinnedRunbookPlan(definition, run); err == nil {
		t.Fatal("Runbook trigger drift was accepted")
	}
}

func TestLoopRunSessionSeed(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "seed"}
	plain := &AgentRun{Scope: scope, ID: "run-plain", Goal: "Send the weekly summary."}
	entries, err := loopRunSessionSeedEntries(t.Context(), nil, nil, nil, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SourceKey != GoalSourceKey(plain.ID) || entries[0].User.GoalKind != "routine" {
		t.Fatalf("a non-routine Run gets one brief seed, got %+v", entries)
	}
	if err := entries[0].Validate(); err != nil {
		t.Fatal(err)
	}

	empty := &AgentRun{Scope: scope, ID: "run-empty"}
	if entries, err := loopRunSessionSeedEntries(t.Context(), nil, nil, nil, empty); err != nil || len(entries) != 0 {
		t.Fatalf("a Run without a goal gets no seed, got %d entries, %v", len(entries), err)
	}

	occurrence, store := routineEventSeedFixture(2, 0)
	entries, err = loopRunSessionSeedEntries(t.Context(), store, nil, nil, occurrence)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].SourceKey != GoalSourceKey(occurrence.ID) || entries[1].SourceKey != RoutineEventEntrySourceKey(occurrence.ID) {
		t.Fatalf("an event occurrence gets the brief then its events, got %+v", entries)
	}
	if entries[0].ID == entries[1].ID {
		t.Fatal("brief and event entries share an id")
	}
}
