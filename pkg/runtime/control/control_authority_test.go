package control

import (
	"reflect"
	"slices"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// offeredControlTools lists the control tools an authority offers, in the
// advertised order.
func offeredControlTools(a agentrun.ControlAuthority) []string {
	names := []string{}
	for _, name := range controlToolOrder {
		if modelstep.ControlToolOffered(name, a) {
			names = append(names, string(name))
		}
	}
	return names
}

// controlAuthorityTeamCatalog is a Team whose roster holds the current Agent
// (lead) and one delegation target (reviewer).
func controlAuthorityTeamCatalog(scope models.Scope) *controlTeamCatalog {
	ref := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	lead := &kernelagent.AgentDeployment{ID: "lead", Scope: ref, DefinitionID: "lead-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	reviewer := &kernelagent.AgentDeployment{ID: "reviewer", Scope: ref, DefinitionID: "reviewer-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	return &controlTeamCatalog{
		deployment:  lead,
		deployments: []*kernelagent.AgentDeployment{lead, reviewer},
		definitions: map[string]*kernelagent.AgentDefinition{
			"lead-definition@1":     {ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate"},
			"reviewer-definition@1": {ID: "reviewer-definition", Version: "1", DisplayName: "Reviewer", Purpose: "Review"},
		},
		teamDeployment: &kernelteam.Deployment{
			ID: "team", Scope: ref, DefinitionID: "team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
			Roster: []kernelteam.RosterAssignment{
				{ID: "lead", AgentDeploymentID: "lead"},
				{ID: "reviewer", AgentDeploymentID: "reviewer"},
			},
		},
	}
}

func controlAuthorityTeamRun(scope models.Scope, context map[string]interface{}) *models.AgentRun {
	return &models.AgentRun{
		ID: "run", Scope: scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"},
		AssignedAgentID: "lead", Goal: "Coordinate a release", Source: models.RunSourceManual, Context: context,
	}
}

func resolveControlAuthorityForTest(t *testing.T, in ControlAuthorityInput) agentrun.ControlAuthority {
	t.Helper()
	authority, err := ResolveControlAuthority(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// claimedForegroundChatRun schedules and claims the foreground conversation
// Run for one user message in an Agent's chat.
func claimedForegroundChatRun(t *testing.T, store *sqlite.Store) *models.AgentRun {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	conversations := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := conversations.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "control-authority-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := conversationruntest.PostMessage(t, conversations, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion,
		"Review the release in the background", "control-authority-message")
	scheduled, _, err := conversationruntest.NewScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	return claimed
}

func chatAuthorityInput(store *sqlite.Store, run *models.AgentRun) ControlAuthorityInput {
	return ControlAuthorityInput{
		Run: run, DeploymentID: "agent", ConversationTasks: store, Runs: store, Conversations: store,
	}
}

func assertOnlyFinalTool(t *testing.T, authority agentrun.ControlAuthority, mode agentrun.FinalAnswerMode, tool models.ControlToolName) {
	t.Helper()
	if authority.FinalMode != mode {
		t.Fatalf("final mode = %q, want %q", authority.FinalMode, mode)
	}
	if got := offeredControlTools(authority); !reflect.DeepEqual(got, []string{string(tool)}) {
		t.Fatalf("offered = %v", got)
	}
	// Every other field is empty, not merely hidden by the final mode.
	if !reflect.DeepEqual(authority, agentrun.ControlAuthority{AgentID: "lead", FinalMode: mode}) {
		t.Fatalf("authority = %#v", authority)
	}
}

func TestDecisionRunOffersOnlyDecisionTool(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{agentrun.AgentRequestInboxContextKey: map[string]interface{}{"requestId": "request"}})
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
		SkillPromptRefs: []string{"skill:prompt"}, Operations: []agentrun.HostedRunbookOperation{{Name: "op"}},
	})
	assertOnlyFinalTool(t, authority, agentrun.FinalModeDecision, models.ControlToolRequestDecision)
}

func TestReviewRunOffersOnlyReviewTool(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{agentrun.AgentRequestCompletionReviewContextKey: map[string]interface{}{"requestId": "request"}})
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope),
		SkillPromptRefs: []string{"skill:prompt"}, Operations: []agentrun.HostedRunbookOperation{{Name: "op"}},
	})
	assertOnlyFinalTool(t, authority, agentrun.FinalModeReview, models.ControlToolCompletionReview)
}

func TestTaskWorkerForksSelfOnlyAndCannotDelegateOrStartTasks(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	catalog := controlAuthorityTeamCatalog(scope)

	// The same Team Run outside a task may fork to and delegate to the reviewer.
	ordinary := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: controlAuthorityTeamRun(scope, map[string]interface{}{}), DeploymentID: "lead", Catalog: catalog,
	})
	want := []agentrun.HostedAgentTarget{{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review"}}
	if !ordinary.ForkAllowed || !reflect.DeepEqual(ordinary.ForkTargets, want) || !reflect.DeepEqual(ordinary.DelegateTargets, want) || ordinary.TaskWork {
		t.Fatalf("ordinary authority = %#v", ordinary)
	}

	worker := resolveControlAuthorityForTest(t, ControlAuthorityInput{
		Run: controlAuthorityTeamRun(scope, map[string]interface{}{models.ConversationTaskContextKey: "task-1"}), DeploymentID: "lead", Catalog: catalog,
		Operations: []agentrun.HostedRunbookOperation{{Name: "op"}},
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
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	run := controlAuthorityTeamRun(scope, map[string]interface{}{})
	run.Source = models.RunSourceFork
	run.ParentRunID = "parent"
	authority := resolveControlAuthorityForTest(t, ControlAuthorityInput{Run: run, DeploymentID: "lead", Catalog: controlAuthorityTeamCatalog(scope)})
	if authority.ForkAllowed || len(authority.ForkTargets) != 0 {
		t.Fatalf("fork child may fork: %#v", authority)
	}
	for _, name := range offeredControlTools(authority) {
		if name == string(models.ControlToolFork) {
			t.Fatal("control_fork offered to a fork child")
		}
	}
	// A fork child keeps its other authority (delegation in a Team).
	if len(authority.DelegateTargets) != 1 {
		t.Fatalf("fork child delegate targets = %#v", authority.DelegateTargets)
	}
}

func TestVoiceCallDisablesTaskStart(t *testing.T) {
	store := conversationtest.NewStore(t)
	run := claimedForegroundChatRun(t, store)

	typed := resolveControlAuthorityForTest(t, chatAuthorityInput(store, run))
	if typed.TaskStart == nil || !typed.TaskStart.CanStart || typed.VoiceCallStarted {
		t.Fatalf("typed chat task authority = %#v", typed.TaskStart)
	}
	if !slices.Contains(offeredControlTools(typed), string(models.ControlToolStartTask)) {
		t.Fatalf("typed chat offered = %v", offeredControlTools(typed))
	}

	voice := models.CloneAgentRun(run)
	voice.Context["voiceCallStarted"] = true
	spoken := resolveControlAuthorityForTest(t, chatAuthorityInput(store, voice))
	if !spoken.VoiceCallStarted || spoken.TaskStart == nil || spoken.TaskStart.CanStart {
		t.Fatalf("voice call task authority = %#v voice=%v", spoken.TaskStart, spoken.VoiceCallStarted)
	}
	if slices.Contains(offeredControlTools(spoken), string(models.ControlToolStartTask)) {
		t.Fatalf("voice call offered = %v", offeredControlTools(spoken))
	}
}

func TestChatReplyCannotWait(t *testing.T) {
	store := conversationtest.NewStore(t)
	run := claimedForegroundChatRun(t, store)
	chat := resolveControlAuthorityForTest(t, chatAuthorityInput(store, run))
	if chat.CanWait {
		t.Fatal("a chat reply may wait")
	}
	// The answering Agent of the chat may react, ask the user and broadcast.
	if !chat.CanReact || !chat.CanAskUser || !chat.BroadcastAllowed || chat.ApprovalMode != models.DefaultConversationApprovalMode || chat.ExternalChannelReply {
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
		Run:          &models.AgentRun{ID: "routine", Scope: run.Scope, Kind: models.RunKindAgentWork, Owner: run.Owner, AssignedAgentID: "agent", Context: map[string]interface{}{}},
		DeploymentID: "agent", Runs: store, Conversations: store, ConversationTasks: store,
	})
	if !background.CanWait || background.CanReact || background.CanAskUser || background.BroadcastAllowed {
		t.Fatalf("background authority = %#v", background)
	}
}

func TestControlAuthorityFinalModesAndDeterministicRuns(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "control-authority"}
	for name, tc := range map[string]struct {
		context map[string]interface{}
		mode    agentrun.FinalAnswerMode
		visual  bool
	}{
		"default": {map[string]interface{}{}, agentrun.FinalModeDefault, true},
		"spoken":  {map[string]interface{}{"responseMode": "spoken"}, conversationrun.FinalModeSpoken, false},
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
		Operations: []agentrun.HostedRunbookOperation{{Name: "op"}}, Deterministic: true,
	})
	if !reflect.DeepEqual(deterministic, agentrun.ControlAuthority{AgentID: "lead", FinalMode: agentrun.FinalModeDefault}) {
		t.Fatalf("deterministic authority = %#v", deterministic)
	}
	if _, err := ResolveControlAuthority(t.Context(), ControlAuthorityInput{Run: controlAuthorityTeamRun(scope, nil)}); err == nil {
		t.Fatal("authority resolved without a speaking Agent")
	}
}
