package routine

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// Runbook actions run as the acting Agent with its own or its Team's binding,
// only while it is on its owner's current roster.
func TestRunbookActionOwnerChecksTheCurrentRoster(t *testing.T) {
	ctx, scope := context.Background(), models.Scope{Kind: "tenant", ID: "roster"}
	catalogScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bound := func(deployment string) *skill.BoundAction {
		return &skill.BoundAction{Binding: &skill.Binding{Scope: catalogScope, DeploymentID: deployment}}
	}
	team := routineRosterTeam(scope, "harp")
	teams := routineRosterTeams{team: team}
	member := &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "harp"}
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
	own := &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := runbookActionOwner(ctx, nil, own, bound("own")); err != nil {
		t.Fatalf("1:1 Agent = %v", err)
	}
	delegated := &models.AgentRun{Scope: scope, Owner: own.Owner, AssignedAgentID: "harp"}
	if err := runbookActionOwner(ctx, nil, delegated, bound("harp")); err == nil {
		t.Fatal("Agent-owned Run assigned to another Agent accepted")
	}
}

// A Team member creates a routine in its Team chat as in its own chat: the
// Team owns it and the member runs it, only while the member is on the roster.
func TestTeamMemberCreatesARoutineInTheTeamChat(t *testing.T) {
	ctx, store, scope := t.Context(), kerneltest.NewStore(t), models.Scope{Kind: "tenant", ID: "task-test"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}
	service := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	chat, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Recurring research", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{Scope: scope, ConversationID: chat.ID, ExpectedRevision: chat.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}, Intent: models.MessageIntentQuestion, Content: "Check the public forecast every thirty minutes", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: "researcher", Goal: "Handle request", Source: models.RunSourceChat, Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: chat.ID, models.ConversationRunTriggerIDContextKey: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalog()
	definition := RunbookManagementSkill()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "routines", Revision: 1, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{models.RunbookActionCreateTask}, MaximumRisk: skill.RiskLevelWrite}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, "researcher", definition.ID, definition.Version, models.RunbookActionCreateTask)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Forecast update", "goal": "Read the current public forecast and summarize it with its source.", "trigger": map[string]interface{}{"schedule": map[string]interface{}{"cron": "0 */30 * * * *", "timezone": "Asia/Kolkata", "maximumOccurrences": float64(2)}},
		"brief": map[string]interface{}{"summary": "Reads the public forecast every thirty minutes and posts a summary here.", "grants": []interface{}{}}}
	team := routineRosterTeam(scope, "writer")
	team.ID = owner.ID
	teams := routineRosterTeams{team: team}
	validator, err := NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	validator.SetRoutineTeams(teams)
	if _, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err == nil {
		t.Fatal("routine from an Agent off the Team roster accepted")
	}
	team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: "researcher", AgentDeploymentID: "researcher"})
	if _, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args}); err != nil {
		t.Fatalf("member routine proposal = %v", err)
	}
	dispatcher, err := NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetRoutineTeams(teams)
	call := &models.ActionCall{ID: "create-routine", Scope: scope, RunID: run.ID, DeploymentID: "researcher", IdempotencyKey: "create-routine"}
	if _, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Run: run, Bound: bound, Call: call, Arguments: args}); err != nil {
		t.Fatalf("member routine dispatch = %v", err)
	}
	activations, err := store.ListRunbookActivations(ctx, models.RunbookActivationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(activations) != 1 || activations[0].AssignedAgentID != "researcher" || activations[0].Owner != owner {
		t.Fatalf("Team routine = %#v, %v", activations, err)
	}
	agents := kernelagent.NewRegistryWithStore(kernelagent.NewMemoryStore())
	agentDefinition := routineAgentDefinition("1.0.0")
	if _, err := agents.RegisterDefinition(ctx, agentDefinition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "researcher", Scope: binding.Scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "test"); err != nil {
		t.Fatal(err)
	}
	// Each firing rechecks the roster; a removed member's routine is unverified, so the scheduler pauses it.
	reader := routineRosterPortfolio{Store: store, team: team}
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
