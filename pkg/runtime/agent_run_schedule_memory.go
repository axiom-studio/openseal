package runtime

import (
	"context"
	"time"
)

func (s *MemoryStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	decision, err := s.ClaimNextAgentRunWithDecision(ctx, claim)
	if err != nil || decision == nil {
		return nil, err
	}
	return decision.Run, nil
}

func (s *MemoryStore) ClaimNextAgentRunWithDecision(_ context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := make([]*AgentRun, 0, len(s.agentRuns))
	for _, run := range s.agentRuns {
		runs = append(runs, run)
	}
	objectives := make(map[string]*Objective, len(s.objectives))
	for _, objective := range s.objectives {
		if objective.Scope == claim.Scope {
			objectives[objective.ID] = objective
		}
	}
	selected, decision := evaluateAgentRunAdmission(runs, objectives, claim)
	if selected == nil {
		return decision, nil
	}
	if err := applyAgentRunClaim(selected, claim); err != nil {
		return nil, err
	}
	decision.Run = cloneAgentRun(selected)
	decision.Outcome = AgentRunAdmissionClaimed
	if selected.Status == AgentRunStatusPaused && selected.BudgetState == BudgetStateExhausted {
		decision.Outcome = AgentRunAdmissionBudgetStopped
		decision.Blocks = []AgentRunAdmissionBlock{{
			RunID: selected.ID, ObjectiveID: selected.ObjectiveID, AssignedAgentID: selected.AssignedAgentID,
			Owner: selected.Owner, ConcurrencyKey: selected.ConcurrencyKey, Reason: AgentRunAdmissionReasonAttemptBudgetExhausted,
		}}
	}
	return decision, nil
}

func (s *MemoryStore) RenewAgentRunLease(_ context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.agentRuns[portfolioKey(scope, runID)]
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.Status != AgentRunStatusRunning || run.LeaseOwner != workerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return nil, ErrLeaseLost
	}
	expires := now.Add(leaseDuration)
	run.LeaseExpiresAt = &expires
	run.UpdatedAt = now
	run.Revision++
	return cloneAgentRun(run), nil
}
