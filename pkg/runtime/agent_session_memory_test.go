package runtime

import (
	"context"
	"testing"
)

// TestMemoryAgentSessionStore runs the session store conformance suite
// (agent_session_conformance_test.go) against the memory backend.
func TestMemoryAgentSessionStore(t *testing.T) {
	runAgentSessionStoreConformance(t, func(*testing.T) agentSessionConformanceStore { return NewMemoryStore() })
}

func (s *MemoryStore) conformanceCreateRunAgentSessionTx(_ context.Context, req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createRunAgentSessionLocked(req)
}
