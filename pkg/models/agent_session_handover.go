package models

import (
	"fmt"
	"strings"
	"time"
)

// AgentSessionHandOver gives a run session to a new owner Run: a finished
// subagent continued by a new child Run keeps its whole context. The session
// changes owner only from a terminal Run, inside the transaction that creates
// the new Run (RunDependencyGroupCreateRecord.SessionHandOvers).
type AgentSessionHandOver struct {
	SessionID string
	FromRunID string // the current owner; must be terminal
	ToRunID   string // the new owner, bound to SessionID
}

func (h AgentSessionHandOver) validate() error {
	if strings.TrimSpace(h.SessionID) == "" || strings.TrimSpace(h.FromRunID) == "" || strings.TrimSpace(h.ToRunID) == "" || h.FromRunID == h.ToRunID {
		return fmt.Errorf("%w: a session hand-over needs the session, its terminal owner and a new owner", ErrInvalidAgentSession)
	}
	return nil
}

// HandOverAgentSession is session owned by h.ToRunID from now. The stored
// session must be a run session owned by from, a terminal Run acting as the
// same Agent as the new owner.
func HandOverAgentSession(session *AgentSession, from, to *AgentRun, h AgentSessionHandOver, now time.Time) (*AgentSession, error) {
	if session == nil || from == nil || to == nil {
		return nil, ErrAgentSessionNotFound
	}
	if session.Kind != AgentSessionKindRun || session.ID != h.SessionID || session.OwnerRunID != h.FromRunID || from.ID != h.FromRunID || to.ID != h.ToRunID {
		return nil, fmt.Errorf("%w: session %s is not owned by run %s", ErrAgentSessionConflict, h.SessionID, h.FromRunID)
	}
	if !IsTerminalAgentRunStatus(from.Status) {
		return nil, fmt.Errorf("%w: run %s still owns session %s", ErrAgentSessionConflict, from.ID, session.ID)
	}
	if from.Scope != session.Scope || to.Scope != session.Scope || from.AssignedAgentID != to.AssignedAgentID || session.AgentID != to.AssignedAgentID {
		return nil, fmt.Errorf("%w: session %s cannot move to another Agent", ErrAgentSessionConflict, session.ID)
	}
	next := *session
	next.OwnerRunID = to.ID
	next.Revision = session.Revision + 1
	next.UpdatedAt = now.UTC()
	return &next, nil
}
