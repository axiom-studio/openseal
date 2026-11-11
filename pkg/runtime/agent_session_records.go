package runtime

import (
	"fmt"
	"strings"
	"time"
)

func sessionNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func validateSessionEvents(scope Scope, runID string, events []*ActivityEvent, kind error) error {
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("%w: %v", kind, err)
		}
		if event.Scope != scope || event.RunID != runID {
			return fmt.Errorf("%w: activity event belongs to another run", kind)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Open / create

func validateCreateRunAgentSessionTxRequest(req CreateRunAgentSessionTxRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	if strings.TrimSpace(req.OwnerRunID) == "" || strings.TrimSpace(req.AgentID) == "" {
		return fmt.Errorf("%w: ownerRunId and agentId are required", ErrInvalidAgentSession)
	}
	if req.ForkSequence < 0 || (req.ParentSessionID == "" && req.ForkSequence != 0) {
		return fmt.Errorf("%w: invalid fork sequence", ErrInvalidAgentSession)
	}
	if len(req.InitialEntries) == 0 {
		return nil
	}
	return validateAppendAgentSessionEntries(initialEntriesAppend(req, RunAgentSessionID(req.Scope, req.OwnerRunID)))
}

// initialEntriesAppend is the append that seeds a new run session. The fence
// is the caller's (the creator's lease or its transaction), so the request
// carries a placeholder lease only to pass the shared validation.
func initialEntriesAppend(req CreateRunAgentSessionTxRequest, sessionID string) AppendAgentSessionEntriesRequest {
	return AppendAgentSessionEntriesRequest{
		Scope:     req.Scope,
		SessionID: sessionID,
		RunID:     req.OwnerRunID,
		Lease:     &AgentRunLeaseGuard{WorkerID: "agent-session-create", Now: req.Now},
		Entries:   req.InitialEntries,
		Now:       req.Now,
	}
}

// ---------------------------------------------------------------------------
// Reads

// ---------------------------------------------------------------------------
// Append

// ---------------------------------------------------------------------------
// Tool entry transitions
