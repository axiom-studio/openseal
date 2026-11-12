package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
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

// A Team member's background task reaches account setup exactly as its 1:1
// task does, and loses it once the member leaves the roster.
func TestTeamMemberTaskReachesSetupWhileOnTheRoster(t *testing.T) {
	ctx, memory, scope := t.Context(), newTestKernelStore(t), Scope{Kind: "tenant", ID: "task-sql"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	team := chatRosterTeam(scope, "task-agent")
	store := teamRosterPortfolio{Store: memory, team: team}
	conversations := conversation.NewConversationService(store)
	seedTestSpace(t, memory, scope)
	chat, _, err := conversations.CreateConversation(ctx, conversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Team task", IdempotencyKey: "team-task-chat"})
	if err != nil {
		t.Fatal(err)
	}
	message := conversationruntest.PostMessage(t, conversations, chat, ConversationParticipantUser, MessageIntentQuestion, "Read the community in the background", "team-task-source")
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: "task-agent", Goal: "Answer", Source: RunSourceChat,
		ConcurrencyKey: ConversationRunLane(chat.ID, message.ThreadRootID, "task-agent"),
		Context:        map[string]interface{}{conversationRunContextConversationID: chat.ID, conversationRunContextTriggerID: message.ID, "threadRootMessageId": message.ThreadRootID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	settled := conversationruntest.SettleTaskSource(t, store, scope, source.ID, "worker", conversationruntest.TaskSourceProposal{
		Goal: "Read the community and report the outcome", Acknowledgment: "I have started reading the community.", Budget: &BudgetPolicy{MaxTurns: 5}})
	task, err := conversationrun.NewConversationTaskService(store).Start(ctx, settled.Request)
	if err != nil {
		t.Fatalf("Team member task start = %v", err)
	}
	work, err := store.ClaimNextAgentRun(ctx, storetest.ConversationTaskAdmissionClaim(time.Now().UTC().Add(time.Second), 0))
	if err != nil || work == nil || work.ID != task.WorkRun.ID {
		t.Fatalf("claim Team task: %#v %v", work, err)
	}
	origin, err := skillbinding.ConversationTaskSetupOrigin(ctx, store, work)
	if err != nil || origin == nil || origin.TargetAgentID != "task-agent" {
		t.Fatalf("Team member task setup origin = %#v, %v", origin, err)
	}
	team.Roster = nil
	if _, err := skillbinding.ConversationTaskSetupOrigin(ctx, store, work); !errors.Is(err, ErrInvalidSkillSetup) {
		t.Fatalf("removed member's task setup origin err = %v", err)
	}
}
