package runtime

import (
	"reflect"
	"slices"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// offeredControlTools lists the control tools an authority offers, in the
// advertised order.
func offeredControlTools(a ControlAuthority) []string {
	names := []string{}
	for _, name := range controlToolOrder {
		if controlToolOffered(name, a) {
			names = append(names, string(name))
		}
	}
	return names
}

// controlAuthorityTeamCatalog is a Team whose roster holds the current Agent
// (lead) and one delegation target (reviewer).
func controlAuthorityTeamCatalog(scope Scope) *resolverCatalog {
	ref := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	lead := &kernelagent.AgentDeployment{ID: "lead", Scope: ref, DefinitionID: "lead-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	reviewer := &kernelagent.AgentDeployment{ID: "reviewer", Scope: ref, DefinitionID: "reviewer-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	return &resolverCatalog{
		deployment:  lead,
		deployments: []*kernelagent.AgentDeployment{lead, reviewer},
		definitions: map[string]*kernelagent.AgentDefinition{
			"lead-definition@1":     {ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate"},
			"reviewer-definition@1": {ID: "reviewer-definition", Version: "1", DisplayName: "Reviewer", Purpose: "Review"},
		},
		teamDeployment: &kernelteam.Deployment{
			ID: "team", Scope: ref, DefinitionID: "team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
			Roster: []kernelteam.RosterAssignment{
				{ID: "lead", RoleID: "lead", AgentDeploymentID: "lead"},
				{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: "reviewer"},
			},
		},
	}
}

func controlAuthorityTeamRun(scope Scope, context map[string]interface{}) *AgentRun {
	return &AgentRun{
		ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"},
		AssignedAgentID: "lead", Goal: "Coordinate a release", Source: RunSourceManual, Context: context,
	}
}

func resolveControlAuthorityForTest(t *testing.T, in ControlAuthorityInput) ControlAuthority {
	t.Helper()
	authority, err := ResolveControlAuthority(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// claimedForegroundChatRun schedules and claims the foreground conversation
// Run for one user message in an Agent's chat.
func claimedForegroundChatRun(t *testing.T, store *MemoryStore) *AgentRun {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "control-authority-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion,
		"Review the release in the background", "control-authority-message")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	return claimed
}

func chatAuthorityInput(store *MemoryStore, run *AgentRun) ControlAuthorityInput {
	return ControlAuthorityInput{
		Run: run, DeploymentID: "agent", ConversationTasks: store, Runs: store, Conversations: store,
	}
}

func assertOnlyFinalTool(t *testing.T, authority ControlAuthority, mode FinalAnswerMode, tool ControlToolName) {
	t.Helper()
	if authority.FinalMode != mode {
		t.Fatalf("final mode = %q, want %q", authority.FinalMode, mode)
	}
	if got := offeredControlTools(authority); !reflect.DeepEqual(got, []string{string(tool)}) {
		t.Fatalf("offered = %v", got)
	}
	// Every other field is empty, not merely hidden by the final mode.
	if !reflect.DeepEqual(authority, ControlAuthority{AgentID: "lead", FinalMode: mode}) {
		t.Fatalf("authority = %#v", authority)
	}
}

func TestDecisionRunOffersOnlyDecisionTool(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{AgentRequestInboxContextKey: map[string]interface{}{"requestId": "request"}})
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
		SkillPromptRefs: []string{"skill:prompt"}, Operations: []HostedRunbookOperation{{Name: "op"}},
	})
	assertOnlyFinalTool(t, authority, FinalModeDecision, ControlToolRequestDecision)
}

func TestReviewRunOffersOnlyReviewTool(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{AgentRequestCompletionReviewContextKey: map[string]interface{}{"requestId": "request"}})
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
		SkillPromptRefs: []string{"skill:prompt"}, Operations: []HostedRunbookOperation{{Name: "op"}},
	})
	assertOnlyFinalTool(t, authority, FinalModeReview, ControlToolCompletionReview)
}

func TestTaskWorkerForksSelfOnlyAndCannotDelegateOrStartTasks(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	catalog := controlAuthorityTeamCatalog(scope)

	// The same Team Run outside a task may fork to and delegate to the reviewer.
	ordinary := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: controlAuthorityTeamRun(scope, map[string]interface{}{}), DeploymentID: "lead", Catalog: catalog,
	})
	want := []HostedAgentTarget{{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review"}}
	if !ordinary.ForkAllowed || !reflect.DeepEqual(ordinary.ForkTargets, want) || !reflect.DeepEqual(ordinary.DelegateTargets, want) || ordinary.TaskWork {
		t.Fatalf("ordinary authority = %#v", ordinary)
	}

	worker := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: controlAuthorityTeamRun(scope, map[string]interface{}{ConversationTaskContextKey: "task-1"}), DeploymentID: "lead", Catalog: catalog,
		Operations: []HostedRunbookOperation{{Name: "op"}},
	})
	if !worker.ForkAllowed || len(worker.ForkTargets) != 0 || len(worker.DelegateTargets) != 0 || !worker.TaskWork {
		t.Fatalf("task worker authority = %#v", worker)
	}
	if worker.TaskStart != nil && worker.TaskStart.CanStart {
		t.Fatalf("task worker may start tasks: %#v", worker.TaskStart)
	}
	got := offeredControlTools(worker)
	if !reflect.DeepEqual(got, []string{"turn_final_answer", "turn_report_progress", "wait", "control_fork", "run_operation"}) {
		t.Fatalf("task worker offered = %v", got)
	}
}

func TestForkChildCannotFork(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{})
	run.Source = RunSourceFork
	run.ParentRunID = "parent"
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope)})
	if authority.ForkAllowed || len(authority.ForkTargets) != 0 {
		t.Fatalf("fork child may fork: %#v", authority)
	}
	for _, name := range offeredControlTools(authority) {
		if name == string(ControlToolFork) {
			t.Fatal("control_fork offered to a fork child")
		}
	}
	// A fork child keeps its other authority (delegation in a Team).
	if len(authority.DelegateTargets) != 1 {
		t.Fatalf("fork child delegate targets = %#v", authority.DelegateTargets)
	}
}

func TestVoiceCallDisablesTaskStart(t *testing.T) {
	store := NewMemoryStore()
	run := claimedForegroundChatRun(t, store)

	typed := resolveControlAuthorityForTest(t, chatAuthorityInput(store, run))
	if typed.TaskStart == nil || !typed.TaskStart.CanStart || typed.VoiceCallStarted {
		t.Fatalf("typed chat task authority = %#v", typed.TaskStart)
	}
	if !slices.Contains(offeredControlTools(typed), string(ControlToolStartTask)) {
		t.Fatalf("typed chat offered = %v", offeredControlTools(typed))
	}

	voice := cloneAgentRun(run)
	voice.Context["voiceCallStarted"] = true
	spoken := resolveControlAuthorityForTest(t, chatAuthorityInput(store, voice))
	if !spoken.VoiceCallStarted || spoken.TaskStart == nil || spoken.TaskStart.CanStart {
		t.Fatalf("voice call task authority = %#v voice=%v", spoken.TaskStart, spoken.VoiceCallStarted)
	}
	if slices.Contains(offeredControlTools(spoken), string(ControlToolStartTask)) {
		t.Fatalf("voice call offered = %v", offeredControlTools(spoken))
	}
}

func TestChatReplyCannotWait(t *testing.T) {
	store := NewMemoryStore()
	run := claimedForegroundChatRun(t, store)
	chat := resolveControlAuthorityForTest(t, chatAuthorityInput(store, run))
	if chat.CanWait {
		t.Fatal("a chat reply may wait")
	}
	// The answering Agent of the chat may react, ask the user and broadcast.
	if !chat.CanReact || !chat.CanAskUser || !chat.BroadcastAllowed || chat.ApprovalMode != DefaultConversationApprovalMode || chat.ExternalChannelReply {
		t.Fatalf("chat authority = %#v", chat)
	}
	got := offeredControlTools(chat)
	if !reflect.DeepEqual(got, []string{"turn_final_answer", "turn_report_progress", "ask_user", "react", "conversation_start_task", "control_fork"}) {
		t.Fatalf("chat offered = %v", got)
	}

	// Another Agent speaking for the Run is not the answering participant.
	other := chatAuthorityInput(store, run)
	other.DeploymentID = "someone-else"
	foreign := resolveControlAuthorityForTest(t, other)
	if foreign.CanReact || foreign.CanAskUser {
		t.Fatalf("non-answering Agent authority = %#v", foreign)
	}

	background := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run:          &AgentRun{ID: "routine", Scope: run.Scope, Kind: RunKindAgentWork, Owner: run.Owner, AssignedAgentID: "agent", Context: map[string]interface{}{}},
		DeploymentID: "agent", Runs: store, Conversations: store, ConversationTasks: store,
	})
	if !background.CanWait || background.CanReact || background.CanAskUser || background.BroadcastAllowed {
		t.Fatalf("background authority = %#v", background)
	}
}

func TestControlAuthorityFinalModesAndDeterministicRuns(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "control-authority"}
	for name, tc := range map[string]struct {
		context map[string]interface{}
		mode    FinalAnswerMode
		visual  bool
	}{
		"default":  {map[string]interface{}{}, FinalModeDefault, true},
		"spoken":   {map[string]interface{}{"responseMode": "spoken"}, FinalModeSpoken, false},
		"evidence": {map[string]interface{}{EvidenceSnapshotContextKey: map[string]interface{}{"id": "snapshot"}}, FinalModeEvidence, true},
	} {
		t.Run(name, func(t *testing.T) {
			authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{
				Run: controlAuthorityTeamRun(scope, tc.context), DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
			})
			if authority.FinalMode != tc.mode || authority.VisualAllowed != tc.visual || !authority.CanReportProgress {
				t.Fatalf("authority = %#v", authority)
			}
		})
	}
	deterministic := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: controlAuthorityTeamRun(scope, map[string]interface{}{}), DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
		Operations: []HostedRunbookOperation{{Name: "op"}}, Deterministic: true,
	})
	if !reflect.DeepEqual(deterministic, ControlAuthority{AgentID: "lead", FinalMode: FinalModeDefault}) {
		t.Fatalf("deterministic authority = %#v", deterministic)
	}
	if _, err := ResolveControlAuthority(t.Context(), ControlAuthorityInput{Run: controlAuthorityTeamRun(scope, nil)}); err == nil {
		t.Fatal("authority resolved without a speaking Agent")
	}
}
