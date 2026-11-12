package models

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateConversationRunRejectsForeignSession(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "sessions"}
	lane := ConversationRunLane("chat", "", "agent-a")
	valid := func() *AgentRun {
		return &AgentRun{ID: "run", Scope: scope, Kind: RunKindConversation,
			Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a", ConcurrencyKey: lane,
			SessionID: ConversationSessionID(scope, lane),
			Context: map[string]interface{}{
				ConversationRunConversationIDContextKey: "chat",
				ConversationRunTriggerIDContextKey:      "m1",
			}}
	}
	if err := ValidateConversationRun(valid()); err != nil {
		t.Fatalf("own lane session: %v", err)
	}
	unbound := valid()
	unbound.SessionID = ""
	if err := ValidateConversationRun(unbound); err != nil {
		t.Fatalf("unbound run: %v", err)
	}
	for name, sessionID := range map[string]string{
		"other agent":  ConversationSessionID(scope, ConversationRunLane("chat", "", "agent-b")),
		"other thread": ConversationSessionID(scope, ConversationRunLane("chat", "root", "agent-a")),
		"other scope":  ConversationSessionID(Scope{Kind: "tenant", ID: "other"}, lane),
		"run session":  RunAgentSessionID(scope, "run"),
	} {
		run := valid()
		run.SessionID = sessionID
		if err := ValidateConversationRun(run); !errors.Is(err, ErrInvalidAgentRun) {
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

func TestChildSessionSeedBoundsBriefAndKind(t *testing.T) {
	if _, err := ChildSessionSeed(testSessionScope, "child", "operation", "brief"); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("unknown kind err = %v", err)
	}
	if _, err := ChildSessionSeed(testSessionScope, "", "task", "brief"); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("no run err = %v", err)
	}
	if _, err := ChildSessionSeed(testSessionScope, "child", "task", "  "); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("empty brief err = %v", err)
	}
	long := strings.Repeat("a", MaximumChildBriefBytes-1) + "é" + "tail"
	seed, err := ChildSessionSeed(testSessionScope, "child", "fork_branch", long)
	if err != nil {
		t.Fatal(err)
	}
	text := seed.User.Content[0].Text
	body := strings.TrimSuffix(strings.TrimPrefix(text, "<brief>\n"), "\n</brief>")
	if len(body) != MaximumChildBriefBytes-1 || !utf8.ValidString(text) {
		t.Fatalf("brief kept %d bytes (valid %v)", len(body), utf8.ValidString(text))
	}
	if err := seed.User.validate(); err != nil {
		t.Fatalf("seed entry invalid: %v", err)
	}
}
