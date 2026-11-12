package models

import (
	"fmt"
	"strings"
)

func ValidateSessionEvents(scope Scope, runID string, events []*ActivityEvent, kind error) error {
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

func ValidateCreateRunAgentSessionTxRequest(req CreateRunAgentSessionTxRequest) error {
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
	return ValidateAppendAgentSessionEntries(InitialEntriesAppendRequest(req, RunAgentSessionID(req.Scope, req.OwnerRunID)))
}

// InitialEntriesAppendRequest is the append that seeds a new run session. The fence
// is the caller's (the creator's lease or its transaction), so the request
// carries a placeholder lease only to pass the shared validation.
func InitialEntriesAppendRequest(req CreateRunAgentSessionTxRequest, sessionID string) AppendAgentSessionEntriesRequest {
	return AppendAgentSessionEntriesRequest{
		Scope:     req.Scope,
		SessionID: sessionID,
		RunID:     req.OwnerRunID,
		Lease:     &AgentRunLeaseGuard{WorkerID: "agent-session-create", Now: req.Now},
		Entries:   req.InitialEntries,
		Now:       req.Now,
	}
}
