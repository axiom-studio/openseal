package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Conversation session identity (run-loop section 05 §1, §3.1). Ids come from
// the section 01 functions ConversationAgentSessionID and RunAgentSessionID;
// the stores create the rows (OpenConversationAgentSession at a chat Run's
// first step, createRunAgentSessionTx for same-Agent children and promotion,
// CreateRunAgentSession for parentless run sessions at first claim).

// ConversationSessionID is the stable id of the chat session of one lane
// (ConversationRunLane). It never changes, including across task promotion:
// the promoted Run moves to a child session and the lane keeps this one.
func ConversationSessionID(scope Scope, lane string) string {
	return ConversationAgentSessionID(scope, lane)
}

// conversationLaneSessionID is the session id a chat Run in this lane writes
// to, or "" when the Run has no acting Agent (a Team's routing Run).
func conversationLaneSessionID(scope Scope, conversationID, threadRoot, agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return ""
	}
	return ConversationSessionID(scope, ConversationRunLane(conversationID, threadRoot, agentID))
}

// conversationLaneSession is the in-memory shape of a lane session before its
// row exists. The store creates the row lazily at the Run's first step.
func conversationLaneSession(scope Scope, conversationID, threadRoot, agentID string) *AgentSession {
	lane := ConversationRunLane(conversationID, threadRoot, agentID)
	id := ConversationSessionID(scope, lane)
	return &AgentSession{
		ID: id, Scope: scope, Kind: AgentSessionKindConversation, LaneKey: lane,
		ConversationID: conversationID, ThreadRootID: threadRoot, AgentID: agentID,
		RootSessionID: id, NextSequence: 1,
	}
}

// ConversationRunSession resolves the session a conversation Run writes to.
// It returns (nil, nil) for Runs that have no session (Team routing Runs and
// pinned-runbook Runs). A lane session whose row does not exist yet comes back
// as its in-memory shape (NextSequence 1, Revision 0). Any other id is a child
// or run-owned session that must already be stored; a missing one is
// ErrInvalidAgentRun.
func ConversationRunSession(ctx context.Context, sessions AgentSessionStore, run *AgentRun) (*AgentSession, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: run is required", ErrInvalidAgentRun)
	}
	if run.SessionID == "" {
		return nil, nil
	}
	if sessions == nil {
		return nil, errors.New("agent session store is not configured")
	}
	conversationID, _ := run.Context[ConversationRunConversationIDContextKey].(string)
	root, _ := run.Context["threadRootMessageId"].(string)
	lane := conversationLaneSessionID(run.Scope, conversationID, root, run.AssignedAgentID)
	session, err := sessions.GetAgentSession(ctx, run.Scope, run.SessionID)
	switch {
	case err == nil:
	case errors.Is(err, ErrAgentSessionNotFound) && conversationID != "" && run.SessionID == lane:
		return conversationLaneSession(run.Scope, conversationID, root, run.AssignedAgentID), nil
	case errors.Is(err, ErrAgentSessionNotFound):
		return nil, fmt.Errorf("%w: run session %s does not exist", ErrInvalidAgentRun, run.SessionID)
	default:
		return nil, err
	}
	if session.ID != run.SessionID {
		return nil, fmt.Errorf("%w: run session %s resolved to %s", ErrInvalidAgentRun, run.SessionID, session.ID)
	}
	if session.ID == lane && session.Kind != AgentSessionKindConversation {
		return nil, fmt.Errorf("%w: lane session %s is not a conversation session", ErrInvalidAgentRun, session.ID)
	}
	if session.Kind == AgentSessionKindRun && session.OwnerRunID != run.ID {
		return nil, fmt.Errorf("%w: run session %s belongs to run %s", ErrInvalidAgentRun, session.ID, session.OwnerRunID)
	}
	return session, nil
}

// validateConversationRunSession is the session rule of ValidateConversationRun
// (section 05 §1.2): a conversation Run that is not a task continuation writes
// only to its own lane session.
func validateConversationRunSession(run *AgentRun) error {
	if run.SessionID == "" {
		return nil
	}
	if task, _ := run.Context[ConversationTaskContextKey].(string); task != "" {
		return nil
	}
	if run.SessionID != ConversationSessionID(run.Scope, run.ConcurrencyKey) {
		return fmt.Errorf("%w: conversation Run is bound to another session", ErrInvalidAgentRun)
	}
	return nil
}

// ChildSessionRequest describes a same-Agent session that branches from a
// parent (self-fork, conversation task, promotion; A3). Cross-Agent children
// and routines get a parentless run session instead (AgentSessionKindRun).
type ChildSessionRequest struct {
	Scope           Scope
	ParentSessionID string // required
	// ForkSequence is the last parent entry the child renders (inclusive):
	// the sequence just before the forking assistant entry, or the parent leaf
	// (NextSequence-1) at promotion. Required (>= 1).
	ForkSequence   int64
	OwnerRunID     string // required: the child Run (already stored)
	AgentID        string // required: the child's acting Agent
	ConversationID string // optional; the store copies the parent's when empty
	ThreadRootID   string
	Seed           *SessionEntry // optional first entry (childSessionSeed)
}

// NewChildSession returns the createRunAgentSessionTx request of req. The
// child id is deterministic: RunAgentSessionID(scope, OwnerRunID).
func NewChildSession(req ChildSessionRequest) (CreateRunAgentSessionTxRequest, error) {
	if err := req.Scope.Validate(); err != nil {
		return CreateRunAgentSessionTxRequest{}, fmt.Errorf("%w: %v", ErrInvalidAgentRun, err)
	}
	if strings.TrimSpace(req.ParentSessionID) == "" || strings.TrimSpace(req.OwnerRunID) == "" || strings.TrimSpace(req.AgentID) == "" {
		return CreateRunAgentSessionTxRequest{}, fmt.Errorf("%w: child session needs a parent, an owner Run and an Agent", ErrInvalidAgentRun)
	}
	if req.ForkSequence < 1 {
		return CreateRunAgentSessionTxRequest{}, fmt.Errorf("%w: child session needs a fork entry", ErrInvalidAgentRun)
	}
	if req.ParentSessionID == RunAgentSessionID(req.Scope, req.OwnerRunID) {
		return CreateRunAgentSessionTxRequest{}, fmt.Errorf("%w: child session cannot be its own parent", ErrInvalidAgentRun)
	}
	out := CreateRunAgentSessionTxRequest{
		Scope: req.Scope, OwnerRunID: req.OwnerRunID, AgentID: req.AgentID,
		ConversationID: req.ConversationID, ThreadRootID: req.ThreadRootID,
		ParentSessionID: req.ParentSessionID, ForkSequence: req.ForkSequence,
	}
	if req.Seed != nil {
		if req.Seed.RunID != "" && req.Seed.RunID != req.OwnerRunID {
			return CreateRunAgentSessionTxRequest{}, fmt.Errorf("%w: child seed belongs to another Run", ErrInvalidAgentRun)
		}
		out.InitialEntries = []*SessionEntry{req.Seed}
	}
	return out, nil
}

// MaximumChildBriefBytes bounds a child seed's brief on a UTF-8 boundary (the
// same 16 KiB rule as a conversation task's goal).
const MaximumChildBriefBytes = 16 << 10

// ConversationTaskBrief is the brief text of a conversation task's seed.
func ConversationTaskBrief(goal string) string {
	return "Background task: " + strings.TrimSpace(goal)
}

// ChildSessionSeed is the first user entry of the run-owned session of runID:
// the brief as a user entry with Origin goal. kind is a goal kind
// (task|fork_branch|decision|review|delegation|routine). The id and SourceKey
// are the deterministic goal key, so a replayed create writes nothing new.
func ChildSessionSeed(scope Scope, runID, kind, brief string) (*SessionEntry, error) {
	if strings.TrimSpace(runID) == "" || !userEntryGoalKinds[kind] {
		return nil, fmt.Errorf("%w: child seed needs a Run and a goal kind, got %q", ErrInvalidAgentRun, kind)
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return nil, fmt.Errorf("%w: child seed needs a brief", ErrInvalidAgentRun)
	}
	text := "<brief>\n" + TruncateUTF8(brief, MaximumChildBriefBytes) + "\n</brief>"
	key := GoalSourceKey(runID)
	return &SessionEntry{
		ID: SessionEntryID(RunAgentSessionID(scope, runID), key), RunID: runID,
		Type: SessionEntryUser, SourceKey: key,
		User: &UserEntry{
			Origin: UserEntryGoal, GoalKind: kind,
			Author:  SessionAuthor{Type: "service", ID: "kernel"},
			Content: []ContentPart{{Type: ContentPartText, Text: text}},
		},
	}, nil
}
