package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestConversationSessionIDIsStablePerLane(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "sessions"}
	lane := models.ConversationRunLane("chat", "", "agent-a")
	if models.ConversationSessionID(scope, lane) != models.ConversationSessionID(scope, lane) {
		t.Fatal("same lane gave two ids")
	}
	ids := map[string]string{
		"base":   models.ConversationSessionID(scope, lane),
		"thread": models.ConversationSessionID(scope, models.ConversationRunLane("chat", "root", "agent-a")),
		"agent":  models.ConversationSessionID(scope, models.ConversationRunLane("chat", "", "agent-b")),
		"scope":  models.ConversationSessionID(models.Scope{Kind: "tenant", ID: "other"}, lane),
	}
	seen := map[string]string{}
	for name, id := range ids {
		if other, ok := seen[id]; ok {
			t.Fatalf("%s and %s share session id %s", name, other, id)
		}
		seen[id] = name
	}
	// Before its row exists, a chat Run on the thread lane resolves to the
	// lane's in-memory session, and only under the lane's own id.
	threadRun := &models.AgentRun{ID: "thread-run", Scope: scope, AssignedAgentID: "agent-a", SessionID: ids["thread"],
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "chat", "threadRootMessageId": "root"}}
	if session, err := models.ConversationRunSession(context.Background(), kerneltest.NewStore(t), threadRun); err != nil || session == nil ||
		session.ID != ids["thread"] || session.LaneKey != "chat:thread:root:agent:agent-a" || session.Validate() != nil {
		t.Fatalf("lane session = %+v, %v", session, err)
	}

	// A 1:1 Agent chat Run, on the channel and on a thread lane.
	agentChat := &models.Conversation{ID: "chat", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-a"}}
	message := &models.ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat", Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "7"}}
	request := conversation.ConversationAgentRunRequest(agentChat, message)
	if request.SessionID == "" || request.SessionID != models.ConversationSessionID(scope, request.ConcurrencyKey) || request.SessionID != ids["base"] {
		t.Fatalf("agent chat request session = %q, lane %q", request.SessionID, request.ConcurrencyKey)
	}
	threaded := &models.ChannelMessage{ID: "m2", Scope: scope, ConversationID: "chat", ThreadRootID: "root", Sender: message.Sender}
	if request := conversation.ConversationAgentRunRequest(agentChat, threaded); request.SessionID != ids["thread"] {
		t.Fatalf("thread request session = %q, want %q", request.SessionID, ids["thread"])
	}

	// A Team member's Run uses the member's own lane session.
	team := &models.Conversation{ID: "chat", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}}
	routing := &models.AgentRun{ID: "routing", Scope: scope, Owner: team.Owner, Context: map[string]interface{}{"threadRootMessageId": "root"}}
	member := conversation.TeamMemberRunRequest(routing, team, message, models.ConversationResponder{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-a"}}, "key")
	if member.SessionID != ids["thread"] || member.SessionID != models.ConversationSessionID(scope, member.ConcurrencyKey) {
		t.Fatalf("member request session = %q, want %q", member.SessionID, ids["thread"])
	}
}

func TestTeamRoutingRunHasNoSession(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "sessions"}
	team := &models.Conversation{ID: "chat", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}}
	message := &models.ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat", Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "7"}}
	request := conversation.ConversationAgentRunRequest(team, message)
	if request.AssignedAgentID != "" || request.SessionID != "" {
		t.Fatalf("routing request = agent %q session %q", request.AssignedAgentID, request.SessionID)
	}
	run := &models.AgentRun{ID: "routing", Scope: scope, Kind: models.RunKindConversation, Owner: team.Owner,
		ConcurrencyKey: request.ConcurrencyKey, Context: request.Context}
	store := kerneltest.NewStore(t)
	session, err := models.ConversationRunSession(context.Background(), store, run)
	if err != nil || session != nil {
		t.Fatalf("routing run session = %+v, %v", session, err)
	}
	// An agentless lane has no session id, so a routing Run that claims the
	// id such a lane would have gets no lane session.
	claimed := &models.AgentRun{ID: "routing", Scope: scope, Kind: models.RunKindConversation, Owner: team.Owner,
		SessionID: models.ConversationSessionID(scope, models.ConversationRunLane("chat", "", "")),
		Context:   map[string]interface{}{models.ConversationRunConversationIDContextKey: "chat"}}
	if session, err := models.ConversationRunSession(context.Background(), store, claimed); !errors.Is(err, models.ErrInvalidAgentRun) || session != nil {
		t.Fatalf("routing lane session = %+v, %v", session, err)
	}
}

func TestConversationRunSessionResolvesLaneAndChildSessions(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	lane := models.ConversationRunLane("c1", "", "agent-a")
	run := putTestSessionRun(t, store, "r1", models.ConversationSessionID(testSessionScope, lane), models.AgentRunStatusRunning, 0)
	run.AssignedAgentID = "agent-a"
	run.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: "c1"}

	// Before the first step: the in-memory lane shape.
	pending, err := models.ConversationRunSession(ctx, store, run)
	if err != nil || pending == nil || pending.ID != run.SessionID || pending.Revision != 0 || pending.NextSequence != 1 {
		t.Fatalf("pending lane session = %+v, %v", pending, err)
	}
	opened := openTestConversationSession(t, store, "r1")
	stored, err := models.ConversationRunSession(ctx, store, run)
	if err != nil || stored.ID != opened.ID || stored.Revision != opened.Revision || stored.Kind != models.AgentSessionKindConversation {
		t.Fatalf("stored lane session = %+v, %v", stored, err)
	}

	// A child session id that does not exist is invalid.
	child := &models.AgentRun{ID: "child", Scope: testSessionScope, AssignedAgentID: "agent-a", SessionID: models.RunAgentSessionID(testSessionScope, "child"),
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "c1"}}
	if _, err := models.ConversationRunSession(ctx, store, child); !errors.Is(err, models.ErrInvalidAgentRun) {
		t.Fatalf("missing child session err = %v", err)
	}
	// Another Run's run session is not this Run's.
	foreign := *child
	foreign.ID = "other"
	putTestSessionRun(t, store, "child", "", models.AgentRunStatusRunning, 0)
	if _, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{Scope: testSessionScope, OwnerRunID: "child", AgentID: "agent-a", Lease: testSessionLease()}); err != nil {
		t.Fatal(err)
	}
	if session, err := models.ConversationRunSession(ctx, store, child); err != nil || session.OwnerRunID != "child" {
		t.Fatalf("child session = %+v, %v", session, err)
	}
	if _, err := models.ConversationRunSession(ctx, store, &foreign); !errors.Is(err, models.ErrInvalidAgentRun) {
		t.Fatalf("foreign run session err = %v", err)
	}
}

func TestChildSessionRequiresForkEntryAndParent(t *testing.T) {
	base := models.ChildSessionRequest{Scope: testSessionScope, ParentSessionID: "parent", ForkSequence: 2, OwnerRunID: "child", AgentID: "agent-a"}
	for name, mutate := range map[string]func(*models.ChildSessionRequest){
		"no parent":     func(r *models.ChildSessionRequest) { r.ParentSessionID = " " },
		"no fork entry": func(r *models.ChildSessionRequest) { r.ForkSequence = 0 },
		"negative fork": func(r *models.ChildSessionRequest) { r.ForkSequence = -1 },
		"no owner run":  func(r *models.ChildSessionRequest) { r.OwnerRunID = "" },
		"no agent":      func(r *models.ChildSessionRequest) { r.AgentID = "" },
		"own parent": func(r *models.ChildSessionRequest) {
			r.ParentSessionID = models.RunAgentSessionID(testSessionScope, "child")
		},
		"no scope":     func(r *models.ChildSessionRequest) { r.Scope = models.Scope{} },
		"foreign seed": func(r *models.ChildSessionRequest) { r.Seed = &models.SessionEntry{RunID: "someone-else"} },
	} {
		req := base
		mutate(&req)
		if _, err := models.NewChildSession(req); !errors.Is(err, models.ErrInvalidAgentRun) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}

	// A valid request forks the parent at the given entry through the store.
	store := kerneltest.NewStore(t)
	parent := openTestConversationSession(t, store, "r1")
	if _, err := testAppend(store, parent.ID, "r1", storetest.UserSessionEntry("p1", "a"), storetest.UserSessionEntry("p2", "b"), storetest.UserSessionEntry("p3", "c")); err != nil {
		t.Fatal(err)
	}
	seed, err := models.ChildSessionSeed(testSessionScope, "child", "task", models.ConversationTaskBrief("  draft the launch note "))
	if err != nil {
		t.Fatal(err)
	}
	req := base
	req.ParentSessionID = parent.ID
	req.Seed = seed
	txReq, err := models.NewChildSession(req)
	if err != nil {
		t.Fatal(err)
	}
	session, err := forkTestSession(t, store, "r1", txReq)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != models.RunAgentSessionID(testSessionScope, "child") ||
		session.ParentSessionID != parent.ID || session.ForkSequence != 2 || session.ForkEntryID != "p2" ||
		session.ConversationID != "c1" || session.NextSequence != 2 {
		t.Fatalf("child session = %+v", session)
	}
	if run := testStoredSessionRun(t, store, "child"); run.SessionID != session.ID {
		t.Fatalf("child run session = %q", run.SessionID)
	}
	entry, err := store.GetSessionEntry(context.Background(), testSessionScope, seed.ID)
	if err != nil || entry.SessionID != session.ID || entry.User == nil || entry.User.Origin != models.UserEntryGoal || entry.User.GoalKind != "task" ||
		entry.User.Content[0].Text != "<brief>\nBackground task: draft the launch note\n</brief>" {
		t.Fatalf("seed = %+v, %v", entry, err)
	}
	if seed.ID != models.SessionEntryID(session.ID, models.GoalSourceKey("child")) {
		t.Fatalf("seed id %s is not the goal key", seed.ID)
	}
}
