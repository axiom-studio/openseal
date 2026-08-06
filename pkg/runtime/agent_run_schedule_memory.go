package runtime

import (
	"context"
	"time"
)

func (s *MemoryStore) ClaimNextAgentRun(_ context.Context, claim AgentRunClaim) (*AgentRun, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activeByAgent := make(map[string]int)
	activeByOwner := make(map[string]int)
	activeByObjective := make(map[string]int)
	activeByConcurrencyKey := make(map[string]int)
	if claim.MaxActiveForAgent > 0 || claim.MaxActiveForOwner > 0 || claim.MaxActiveForObjective > 0 || claim.MaxActiveForConcurrencyKey > 0 {
		for _, run := range s.agentRuns {
			if run.Scope != claim.Scope || run.Status != AgentRunStatusRunning ||
				run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now) {
				continue
			}
			if claim.MaxActiveForAgent > 0 && run.AssignedAgentID != "" {
				activeByAgent[run.AssignedAgentID]++
			}
			if claim.MaxActiveForOwner > 0 {
				activeByOwner[agentRunOwnerSchedulingKey(run.Owner)]++
			}
			if claim.MaxActiveForObjective > 0 && run.ObjectiveID != "" {
				activeByObjective[run.ObjectiveID]++
			}
			if claim.MaxActiveForConcurrencyKey > 0 && run.ConcurrencyKey != "" {
				activeByConcurrencyKey[run.ConcurrencyKey]++
			}
		}
	}
	var selected *AgentRun
	for _, run := range s.agentRuns {
		if !agentRunEligible(run, claim) {
			continue
		}
		if claim.MaxActiveForAgent > 0 && run.AssignedAgentID != "" && activeByAgent[run.AssignedAgentID] >= claim.MaxActiveForAgent {
			continue
		}
		if claim.MaxActiveForOwner > 0 && activeByOwner[agentRunOwnerSchedulingKey(run.Owner)] >= claim.MaxActiveForOwner {
			continue
		}
		if claim.MaxActiveForObjective > 0 && run.ObjectiveID != "" && activeByObjective[run.ObjectiveID] >= claim.MaxActiveForObjective {
			continue
		}
		if claim.MaxActiveForConcurrencyKey > 0 && run.ConcurrencyKey != "" && activeByConcurrencyKey[run.ConcurrencyKey] >= claim.MaxActiveForConcurrencyKey {
			continue
		}
		if selected == nil || agentRunSchedulesBefore(run, selected, claim.Now, claim.AgingInterval) {
			selected = run
		}
	}
	if selected == nil {
		return nil, nil
	}
	if err := applyAgentRunClaim(selected, claim); err != nil {
		return nil, err
	}
	return cloneAgentRun(selected), nil
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
