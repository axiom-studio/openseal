package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// chatRosterTeams serves one Team deployment through both Team catalog shapes.
type chatRosterTeams struct {
	team *kernelteam.Deployment
	err  error
}

func (c chatRosterTeams) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.team == nil || c.team.ID != id || c.team.Scope != scope {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return c.team, nil
}

func (c chatRosterTeams) GetDeployment(ctx context.Context, scope skill.ScopeReference, id string) (*kernelteam.Deployment, error) {
	return c.GetTeamDeployment(ctx, scope, id)
}

func chatRosterTeam(scope Scope, members ...string) *kernelteam.Deployment {
	team := &kernelteam.Deployment{ID: "crew", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive}
	for _, member := range members {
		team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: member, AgentDeploymentID: member})
	}
	return team
}

// A chat's roster is its owning Agent, or its owning Team's active members.
func TestAgentOnChatRoster(t *testing.T) {
	ctx, scope := context.Background(), Scope{Kind: "tenant", ID: "roster"}
	agentChat := ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}
	teamChat := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	team := chatRosterTeam(scope, "member")
	teams := chatRosterTeams{team: team}
	for _, test := range []struct {
		name  string
		teams TeamRosterReader
		owner ObjectiveOwner
		agent string
		want  bool
	}{
		{"agent chat owner", nil, agentChat, "own", true},
		{"agent chat other Agent", teams, agentChat, "member", false},
		{"agent chat empty Agent", nil, agentChat, "", false},
		{"team member", teams, teamChat, "member", true},
		{"padded team member", teams, teamChat, " member", false},
		{"team outsider", teams, teamChat, "outsider", false},
		{"team without catalog", nil, teamChat, "member", false},
		{"missing team", teams, ObjectiveOwner{Type: OwnerTypeTeam, ID: "other"}, "member", false},
		{"team in another scope", chatRosterTeams{team: chatRosterTeam(Scope{Kind: "tenant", ID: "other"}, "member")}, teamChat, "member", false},
		{"unknown owner", teams, ObjectiveOwner{Type: "person", ID: "member"}, "member", false},
	} {
		got, err := AgentOnChatRoster(ctx, test.teams, scope, test.owner, test.agent)
		if err != nil || got != test.want {
			t.Fatalf("%s = %v, %v", test.name, got, err)
		}
	}
	team.Status = kernelteam.DeploymentPaused
	if got, err := AgentOnChatRoster(ctx, teams, scope, teamChat, "member"); err != nil || got {
		t.Fatalf("paused Team member = %v, %v", got, err)
	}
	failure := errors.New("store down")
	if _, err := AgentOnChatRoster(ctx, chatRosterTeams{err: failure}, scope, teamChat, "member"); !errors.Is(err, failure) {
		t.Fatalf("store error = %v", err)
	}
}

// A Team chat hands work only to the Team or an Agent on its active roster;
// an Agent's 1:1 chat hands work to no one.
func TestChatDelegationStaysOnTheRoster(t *testing.T) {
	ctx, scope := context.Background(), Scope{Kind: "tenant", ID: "roster"}
	service := &CollaborationService{teams: chatRosterTeams{team: chatRosterTeam(scope, "marlow", "harp")}}
	source := &AgentRun{ID: "marlow-run", Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "marlow"}
	for _, recipient := range []CollaborationParty{{Type: OwnerTypeAgent, ID: "harp"}, {Type: OwnerTypeTeam, ID: "crew"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); err != nil {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	for _, recipient := range []CollaborationParty{{Type: OwnerTypeAgent, ID: "outsider"}, {Type: OwnerTypeTeam, ID: "other-team"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); !errors.Is(err, ErrAgentRequestUnauthorized) {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	if err := (&CollaborationService{}).validateChatDelegation(ctx, source, CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("hand-off without a Team roster = %v", err)
	}
	own := &AgentRun{ID: "own-run", Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, own, CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}); !errors.Is(err, ErrInvalidAgentRequestState) {
		t.Fatalf("1:1 hand-off = %v", err)
	}
	work := &AgentRun{ID: "work", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, work, CollaborationParty{Type: OwnerTypeAgent, ID: "outsider"}); err != nil {
		t.Fatalf("non-chat delegation = %v", err)
	}
}

// A Task's result is its own Agent's answer; another member's answer to the
// same message never counts as it.
func TestConversationTaskResultIsTheExpectedSpeakersOwnAnswer(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "roster"}
	run := &AgentRun{ID: "harp-run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "harp"}
	task := &ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: run.Owner, TargetAgentID: "harp", Mode: ConversationTaskModeContinuation}
	answer := func(sender string) *ChannelMessage {
		return &ChannelMessage{Scope: scope, ConversationID: "chat", ReplyToMessageID: "ask", ThreadRootID: "ask", Intent: MessageIntentAnswer, Content: "Done",
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: sender}, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}}
	}
	if !conversationTaskResultMessageMatches(task, run, answer("harp"), false) {
		t.Fatal("member's own answer rejected")
	}
	if conversationTaskResultMessageMatches(task, run, answer("marlow"), false) {
		t.Fatal("another member's answer accepted as this Task's result")
	}
	task.TargetAgentID = ""
	if conversationTaskResultMessageMatches(task, run, answer("marlow"), false) || !conversationTaskResultMessageMatches(task, run, answer("harp"), false) {
		t.Fatal("routed continuation result is not its assigned member's")
	}
	own := &AgentRun{ID: "own-run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	ownTask := &ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: own.Owner, TargetAgentID: "own", Mode: ConversationTaskModeContinuation}
	ownAnswer := answer("own")
	ownAnswer.References = []ConversationReference{{Kind: ConversationReferenceRun, ID: own.ID}}
	if !conversationTaskResultMessageMatches(ownTask, own, ownAnswer, false) {
		t.Fatal("1:1 result rejected")
	}
}

// Runbook actions run as the acting Agent with its own or its Team's binding,
// only while it is on its owner's current roster.
func TestRunbookActionOwnerChecksTheCurrentRoster(t *testing.T) {
	ctx, scope := context.Background(), Scope{Kind: "tenant", ID: "roster"}
	catalogScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bound := func(deployment string) *skill.BoundAction {
		return &skill.BoundAction{Binding: &skill.Binding{Scope: catalogScope, DeploymentID: deployment}}
	}
	team := chatRosterTeam(scope, "harp")
	teams := chatRosterTeams{team: team}
	member := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "harp"}
	for _, deployment := range []string{"harp", "crew"} {
		if err := runbookActionOwner(ctx, teams, member, bound(deployment)); err != nil {
			t.Fatalf("member with %s binding = %v", deployment, err)
		}
	}
	if err := runbookActionOwner(ctx, teams, member, bound("marlow")); err == nil {
		t.Fatal("member used another member's binding")
	}
	if err := runbookActionOwner(ctx, nil, member, bound("harp")); err == nil {
		t.Fatal("member accepted without a Team roster")
	}
	team.Roster = nil
	if err := runbookActionOwner(ctx, teams, member, bound("harp")); err == nil {
		t.Fatal("removed member accepted")
	}
	own := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := runbookActionOwner(ctx, nil, own, bound("own")); err != nil {
		t.Fatalf("1:1 Agent = %v", err)
	}
	delegated := &AgentRun{Scope: scope, Owner: own.Owner, AssignedAgentID: "harp"}
	if err := runbookActionOwner(ctx, nil, delegated, bound("harp")); err == nil {
		t.Fatal("Agent-owned Run assigned to another Agent accepted")
	}
}

// Objective activity on a Team-owned item names the member that acted.
func TestOwnedItemActivityNamesTheActingMember(t *testing.T) {
	member := ActivityActor{Type: "agent", ID: "harp"}
	team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	if event := objectiveActivityEvent(&Objective{ID: "o", Owner: team}, member, "", "objective.updated", "Updated", time.Time{}, nil); event.AgentID != "harp" || event.TeamID != "crew" {
		t.Fatalf("Team objective event = %q / %q", event.AgentID, event.TeamID)
	}
	if event := objectiveActivityEvent(&Objective{ID: "o", Owner: team}, ActivityActor{Type: "user", ID: "operator"}, "", "objective.updated", "Updated", time.Time{}, nil); event.AgentID != "" {
		t.Fatalf("person's Team objective event names Agent %q", event.AgentID)
	}
}

// A Team member creates a routine in its Team chat as in its own chat: the
// Team owns it and the member runs it, only while the member is on the roster.
func TestTeamMemberCreatesARoutineInTheTeamChat(t *testing.T) {
	ctx, store, scope := t.Context(), NewMemoryStore(), Scope{Kind: "tenant", ID: "task-test"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	service := NewConversationService(store)
	chat, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Recurring research", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: chat.ID, ExpectedRevision: chat.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion, Content: "Check the public forecast every thirty minutes", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: "researcher", Goal: "Handle request", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: chat.ID, conversationRunContextTriggerID: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalog()
	definition := RunbookManagementSkill()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "routines", Revision: 1, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{RunbookActionCreateTask}, MaximumRisk: skill.RiskLevelWrite}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, "researcher", definition.ID, definition.Version, RunbookActionCreateTask)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Forecast update", "goal": "Read the current public forecast and summarize it with its source.", "cron": "0 */30 * * * *", "timezone": "Asia/Kolkata", "maximumOccurrences": float64(2),
		"brief": map[string]interface{}{"summary": "Reads the public forecast every thirty minutes and posts a summary here.", "grants": []interface{}{}}}
	team := chatRosterTeam(scope, "writer")
	team.ID = owner.ID
	teams := chatRosterTeams{team: team}
	validator, err := NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	validator.SetRoutineTeams(teams)
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err == nil {
		t.Fatal("routine from an Agent off the Team roster accepted")
	}
	team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: "researcher", AgentDeploymentID: "researcher"})
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err != nil {
		t.Fatalf("member routine proposal = %v", err)
	}
	dispatcher, err := NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetRoutineTeams(teams)
	call := &ActionCall{ID: "create-routine", Scope: scope, RunID: run.ID, DeploymentID: "researcher", IdempotencyKey: "create-routine"}
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Run: run, Bound: bound, Call: call, Arguments: args}); err != nil {
		t.Fatalf("member routine dispatch = %v", err)
	}
	activations, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(activations) != 1 || activations[0].AssignedAgentID != "researcher" || activations[0].Owner != owner {
		t.Fatalf("Team routine = %#v, %v", activations, err)
	}
	agents := kernelagent.NewRegistryWithStore(kernelagent.NewMemoryStore())
	agentDefinition := sqliteAgentDefinition("1.0.0")
	if _, err := agents.RegisterDefinition(ctx, agentDefinition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "researcher", Scope: binding.Scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "test"); err != nil {
		t.Fatal(err)
	}
	// Each firing rechecks the roster; a removed member's routine is unverified, so the scheduler pauses it.
	reader := teamRosterPortfolio{MemoryStore: store, team: team}
	if err := validateScheduledTaskTarget(ctx, reader, agents, activations[0]); err != nil {
		t.Fatalf("member routine target = %v", err)
	}
	if err := validateScheduledTaskTarget(ctx, store, agents, activations[0]); !errors.Is(err, ErrRunbookActivationUnverified) {
		t.Fatalf("Team routine without a roster = %v", err)
	}
	team.Roster = team.Roster[:1]
	if err := validateScheduledTaskTarget(ctx, reader, agents, activations[0]); !errors.Is(err, ErrRunbookActivationUnverified) {
		t.Fatalf("removed member's routine target = %v", err)
	}
}

// A Team member's background task reaches account setup exactly as its 1:1
// task does, and loses it once the member leaves the roster.
func TestTeamMemberTaskReachesSetupWhileOnTheRoster(t *testing.T) {
	ctx, memory, scope := t.Context(), NewMemoryStore(), Scope{Kind: "tenant", ID: "task-sql"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	team := chatRosterTeam(scope, "task-agent")
	store := teamRosterPortfolio{MemoryStore: memory, team: team}
	conversations := NewConversationService(store)
	chat, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Team task", IdempotencyKey: "team-task-chat"})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, conversations, chat, ConversationParticipantUser, MessageIntentQuestion, "Read the community in the background", "team-task-source")
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: "task-agent", Goal: "Answer", Source: RunSourceChat,
		ConcurrencyKey: conversationRunLane(chat.ID, message.ThreadRootID, "task-agent"),
		Context:        map[string]interface{}{conversationRunContextConversationID: chat.ID, conversationRunContextTriggerID: message.ID, "threadRootMessageId": message.ThreadRootID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	settled := settleTaskSource(t, store, scope, source.ID, "worker", taskSourceProposal{
		Goal: "Read the community and report the outcome", Acknowledgment: "I have started reading the community.", Budget: &BudgetPolicy{MaxTurns: 5}})
	task, err := NewConversationTaskService(store).Start(ctx, settled.Request)
	if err != nil {
		t.Fatalf("Team member task start = %v", err)
	}
	work, err := store.ClaimNextAgentRun(ctx, conversationTaskAdmissionClaim(time.Now().UTC().Add(time.Second), 0))
	if err != nil || work == nil || work.ID != task.WorkRun.ID {
		t.Fatalf("claim Team task: %#v %v", work, err)
	}
	origin, err := conversationTaskSetupOrigin(ctx, store, work)
	if err != nil || origin == nil || origin.TargetAgentID != "task-agent" {
		t.Fatalf("Team member task setup origin = %#v, %v", origin, err)
	}
	team.Roster = nil
	if _, err := conversationTaskSetupOrigin(ctx, store, work); !errors.Is(err, ErrInvalidSkillSetup) {
		t.Fatalf("removed member's task setup origin err = %v", err)
	}
}
