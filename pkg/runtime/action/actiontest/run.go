package actiontest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// AssertRunUntouched checks a loop proposal left the Run as claimed:
// running, same lease, same revision, no wake condition.
func AssertRunUntouched(t *testing.T, store models.PortfolioStore, run *models.AgentRun) {
	t.Helper()
	current, err := store.GetAgentRun(context.Background(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != models.AgentRunStatusRunning || current.Revision != run.Revision || current.LeaseOwner != run.LeaseOwner || current.WakeCondition != nil {
		t.Fatalf("loop proposal changed the Run: %#v", current)
	}
}

// ClaimedPersonChatRun claims a 1:1 chat run of release-agent acting for
// actingUser.
func ClaimedPersonChatRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, actingUser string) *models.AgentRun {
	t.Helper()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "send mail",
		Source: models.RunSourceObjective, Kind: models.RunKindConversation, ActingUser: actingUser,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation-1", models.ConversationRunTriggerIDContextKey: "message-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

// ActingUser is the person ClaimedPersonChatRun's runs act for in tests.
const ActingUser = "7"

// ClaimedRun claims a run of release-agent owned by the release team.
func ClaimedRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, worker string) *models.AgentRun {
	t.Helper()
	return ClaimedOwnedRun(t, store, scope, now, worker, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "release-team"})
}

// ClaimedOwnedRun claims a run of release-agent under owner.
func ClaimedOwnedRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, worker string, owner models.ObjectiveOwner) *models.AgentRun {
	t.Helper()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != run.ID {
		t.Fatalf("run was not claimed: %#v", claimed)
	}
	return claimed
}
