package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConversationSessionIDIsStablePerLane(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "sessions"}
	lane := conversationRunLane("chat", "", "agent-a")
	if conversationSessionID(scope, lane) != conversationSessionID(scope, lane) {
		t.Fatal("same lane gave two ids")
	}
	ids := map[string]string{
		"base":   conversationSessionID(scope, lane),
		"thread": conversationSessionID(scope, conversationRunLane("chat", "root", "agent-a")),
		"agent":  conversationSessionID(scope, conversationRunLane("chat", "", "agent-b")),
		"scope":  conversationSessionID(Scope{Kind: "tenant", ID: "other"}, lane),
	}
	seen := map[string]string{}
	for name, id := range ids {
		if other, ok := seen[id]; ok {
			t.Fatalf("%s and %s share session id %s", name, other, id)
		}
		seen[id] = name
	}
	if got := conversationLaneSessionID(scope, "chat", "root", "agent-a"); got != ids["thread"] {
		t.Fatalf("lane session id = %s, want %s", got, ids["thread"])
	}
	if session := conversationLaneSession(scope, "chat", "root", "agent-a"); session.ID != ids["thread"] ||
		session.LaneKey != "chat:thread:root:agent:agent-a" || session.Validate() != nil {
		t.Fatalf("lane session = %+v (%v)", session, session.Validate())
	}

	// A 1:1 Agent chat Run, on the channel and on a thread lane.
	agentChat := &Conversation{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}}
	message := &ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "7"}}
	request := conversationAgentRunRequest(agentChat, message)
	if request.SessionID == "" || request.SessionID != conversationSessionID(scope, request.ConcurrencyKey) || request.SessionID != ids["base"] {
		t.Fatalf("agent chat request session = %q, lane %q", request.SessionID, request.ConcurrencyKey)
	}
	threaded := &ChannelMessage{ID: "m2", Scope: scope, ConversationID: "chat", ThreadRootID: "root", Sender: message.Sender}
	if request := conversationAgentRunRequest(agentChat, threaded); request.SessionID != ids["thread"] {
		t.Fatalf("thread request session = %q, want %q", request.SessionID, ids["thread"])
	}

	// A Team member's Run uses the member's own lane session.
	team := &Conversation{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}}
	routing := &AgentRun{ID: "routing", Scope: scope, Owner: team.Owner, Context: map[string]interface{}{"threadRootMessageId": "root"}}
	member := teamMemberRunRequest(routing, team, message, ConversationResponder{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-a"}}, "key")
	if member.SessionID != ids["thread"] || member.SessionID != conversationSessionID(scope, member.ConcurrencyKey) {
		t.Fatalf("member request session = %q, want %q", member.SessionID, ids["thread"])
	}
}

func TestTeamRoutingRunHasNoSession(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "sessions"}
	team := &Conversation{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}}
	message := &ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "7"}}
	request := conversationAgentRunRequest(team, message)
	if request.AssignedAgentID != "" || request.SessionID != "" {
		t.Fatalf("routing request = agent %q session %q", request.AssignedAgentID, request.SessionID)
	}
	if id := conversationLaneSessionID(scope, "chat", "", ""); id != "" {
		t.Fatalf("routing lane session id = %q", id)
	}
	run := &AgentRun{ID: "routing", Scope: scope, Kind: RunKindConversation, Owner: team.Owner,
		ConcurrencyKey: request.ConcurrencyKey, Context: request.Context}
	session, err := conversationRunSession(context.Background(), NewMemoryStore(), run)
	if err != nil || session != nil {
		t.Fatalf("routing run session = %+v, %v", session, err)
	}
}

func TestValidateConversationRunRejectsForeignSession(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "sessions"}
	lane := conversationRunLane("chat", "", "agent-a")
	valid := func() *AgentRun {
		return &AgentRun{ID: "run", Scope: scope, Kind: RunKindConversation,
			Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a", ConcurrencyKey: lane,
			SessionID: conversationSessionID(scope, lane),
			Context: map[string]interface{}{
				conversationRunContextConversationID: "chat",
				conversationRunContextTriggerID:      "m1",
			}}
	}
	if err := validateConversationRun(valid()); err != nil {
		t.Fatalf("own lane session: %v", err)
	}
	unbound := valid()
	unbound.SessionID = ""
	if err := validateConversationRun(unbound); err != nil {
		t.Fatalf("unbound run: %v", err)
	}
	for name, sessionID := range map[string]string{
		"other agent":  conversationSessionID(scope, conversationRunLane("chat", "", "agent-b")),
		"other thread": conversationSessionID(scope, conversationRunLane("chat", "root", "agent-a")),
		"other scope":  conversationSessionID(Scope{Kind: "tenant", ID: "other"}, lane),
		"run session":  RunAgentSessionID(scope, "run"),
	} {
		run := valid()
		run.SessionID = sessionID
		if err := validateConversationRun(run); !errors.Is(err, ErrInvalidAgentRun) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// A task continuation writes to its child session (§3.3); the lane rule
	// does not apply to it.
	continuation := valid()
	continuation.SessionID = RunAgentSessionID(scope, "run")
	continuation.Context[ConversationTaskContextKey] = "task-1"
	if err := validateConversationRunSession(continuation); err != nil {
		t.Fatalf("task continuation: %v", err)
	}
}

func TestConversationRunSessionResolvesLaneAndChildSessions(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	lane := conversationRunLane("c1", "", "agent-a")
	run := putTestSessionRun(t, store, "r1", conversationSessionID(testSessionScope, lane), AgentRunStatusRunning, 0)
	run.AssignedAgentID = "agent-a"
	run.Context = map[string]interface{}{conversationRunContextConversationID: "c1"}

	// Before the first step: the in-memory lane shape.
	pending, err := conversationRunSession(ctx, store, run)
	if err != nil || pending == nil || pending.ID != run.SessionID || pending.Revision != 0 || pending.NextSequence != 1 {
		t.Fatalf("pending lane session = %+v, %v", pending, err)
	}
	opened := openTestConversationSession(t, store, "r1")
	stored, err := conversationRunSession(ctx, store, run)
	if err != nil || stored.ID != opened.ID || stored.Revision != opened.Revision || stored.Kind != AgentSessionKindConversation {
		t.Fatalf("stored lane session = %+v, %v", stored, err)
	}

	// A child session id that does not exist is invalid.
	child := &AgentRun{ID: "child", Scope: testSessionScope, AssignedAgentID: "agent-a", SessionID: RunAgentSessionID(testSessionScope, "child"),
		Context: map[string]interface{}{conversationRunContextConversationID: "c1"}}
	if _, err := conversationRunSession(ctx, store, child); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("missing child session err = %v", err)
	}
	// Another Run's run session is not this Run's.
	foreign := *child
	foreign.ID = "other"
	putTestSessionRun(t, store, "child", "", AgentRunStatusRunning, 0)
	if _, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{Scope: testSessionScope, OwnerRunID: "child", AgentID: "agent-a", Lease: testSessionLease()}); err != nil {
		t.Fatal(err)
	}
	if session, err := conversationRunSession(ctx, store, child); err != nil || session.OwnerRunID != "child" {
		t.Fatalf("child session = %+v, %v", session, err)
	}
	if _, err := conversationRunSession(ctx, store, &foreign); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("foreign run session err = %v", err)
	}
}

func TestChildSessionRequiresForkEntryAndParent(t *testing.T) {
	base := ChildSessionRequest{Scope: testSessionScope, ParentSessionID: "parent", ForkSequence: 2, OwnerRunID: "child", AgentID: "agent-a"}
	for name, mutate := range map[string]func(*ChildSessionRequest){
		"no parent":     func(r *ChildSessionRequest) { r.ParentSessionID = " " },
		"no fork entry": func(r *ChildSessionRequest) { r.ForkSequence = 0 },
		"negative fork": func(r *ChildSessionRequest) { r.ForkSequence = -1 },
		"no owner run":  func(r *ChildSessionRequest) { r.OwnerRunID = "" },
		"no agent":      func(r *ChildSessionRequest) { r.AgentID = "" },
		"own parent":    func(r *ChildSessionRequest) { r.ParentSessionID = RunAgentSessionID(testSessionScope, "child") },
		"no scope":      func(r *ChildSessionRequest) { r.Scope = Scope{} },
		"foreign seed":  func(r *ChildSessionRequest) { r.Seed = &SessionEntry{RunID: "someone-else"} },
	} {
		req := base
		mutate(&req)
		if _, err := newChildSession(req); !errors.Is(err, ErrInvalidAgentRun) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}

	// A valid request forks the parent at the given entry through the store.
	store := NewMemoryStore()
	parent := openTestConversationSession(t, store, "r1")
	if _, err := testAppend(store, parent.ID, "r1", testUserEntry("p1", "a"), testUserEntry("p2", "b"), testUserEntry("p3", "c")); err != nil {
		t.Fatal(err)
	}
	putTestSessionRun(t, store, "child", "", AgentRunStatusQueued, 0)
	seed, err := childSessionSeed(testSessionScope, "child", "task", conversationTaskBrief("  draft the launch note "))
	if err != nil {
		t.Fatal(err)
	}
	req := base
	req.ParentSessionID = parent.ID
	req.Seed = seed
	txReq, err := newChildSession(req)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	created, err := store.createRunAgentSessionLocked(txReq)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	session := created.Session
	if session.ID != RunAgentSessionID(testSessionScope, "child") || session.ID != newRunSession(testSessionScope, "child", "agent-a").ID ||
		session.ParentSessionID != parent.ID || session.ForkSequence != 2 || session.ForkEntryID != "p2" ||
		session.ConversationID != "c1" || session.NextSequence != 2 {
		t.Fatalf("child session = %+v", session)
	}
	if run := testMemoryRun(t, store, "child"); run.SessionID != session.ID {
		t.Fatalf("child run session = %q", run.SessionID)
	}
	entry, err := store.GetSessionEntry(context.Background(), testSessionScope, seed.ID)
	if err != nil || entry.SessionID != session.ID || entry.User == nil || entry.User.Origin != UserEntryGoal || entry.User.GoalKind != "task" ||
		entry.User.Content[0].Text != "<brief>\nBackground task: draft the launch note\n</brief>" {
		t.Fatalf("seed = %+v, %v", entry, err)
	}
	if seed.ID != SessionEntryID(session.ID, GoalSourceKey("child")) {
		t.Fatalf("seed id %s is not the goal key", seed.ID)
	}
}

func TestChildSessionSeedBoundsBriefAndKind(t *testing.T) {
	if _, err := childSessionSeed(testSessionScope, "child", "operation", "brief"); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("unknown kind err = %v", err)
	}
	if _, err := childSessionSeed(testSessionScope, "", "task", "brief"); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("no run err = %v", err)
	}
	if _, err := childSessionSeed(testSessionScope, "child", "task", "  "); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("empty brief err = %v", err)
	}
	long := strings.Repeat("a", maximumChildBriefBytes-1) + "é" + "tail"
	seed, err := childSessionSeed(testSessionScope, "child", "fork_branch", long)
	if err != nil {
		t.Fatal(err)
	}
	text := seed.User.Content[0].Text
	body := strings.TrimSuffix(strings.TrimPrefix(text, "<brief>\n"), "\n</brief>")
	if len(body) != maximumChildBriefBytes-1 || !utf8.ValidString(text) {
		t.Fatalf("brief kept %d bytes (valid %v)", len(body), utf8.ValidString(text))
	}
	if err := seed.User.validate(); err != nil {
		t.Fatalf("seed entry invalid: %v", err)
	}
}
