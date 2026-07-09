package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

type AgentRunClaim struct {
	Scope             Scope
	WorkerID          string
	AssignedAgentID   string
	Now               time.Time
	LeaseDuration     time.Duration
	AgingInterval     time.Duration
	MaxActiveForAgent int
}

func (c AgentRunClaim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.WorkerID) == "" || c.Now.IsZero() || c.LeaseDuration <= 0 || c.AgingInterval <= 0 {
		return errors.New("worker id, current time, lease duration, and aging interval are required")
	}
	if c.MaxActiveForAgent < 0 {
		return errors.New("max active runs cannot be negative")
	}
	return nil
}

type AgentRunClaimRequest struct {
	Scope             Scope
	WorkerID          string
	AssignedAgentID   string
	LeaseDuration     time.Duration
	AgingInterval     time.Duration
	MaxActiveForAgent int
}

type AgentRunScheduleStore interface {
	ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error)
	RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error)
}

type AgentRunScheduler struct {
	store AgentRunScheduleStore
	now   func() time.Time
}

func NewAgentRunScheduler(store AgentRunScheduleStore) *AgentRunScheduler {
	return &AgentRunScheduler{store: store, now: time.Now}
}

func (s *AgentRunScheduler) ClaimNext(ctx context.Context, req AgentRunClaimRequest) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("worker id is required")
	}
	if req.LeaseDuration <= 0 {
		req.LeaseDuration = 30 * time.Second
	}
	if req.AgingInterval <= 0 {
		req.AgingInterval = time.Minute
	}
	return s.store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: req.Scope, WorkerID: req.WorkerID, AssignedAgentID: req.AssignedAgentID,
		Now: s.now(), LeaseDuration: req.LeaseDuration, AgingInterval: req.AgingInterval,
		MaxActiveForAgent: req.MaxActiveForAgent,
	})
}

func (s *AgentRunScheduler) RenewLease(ctx context.Context, scope Scope, runID, workerID string, leaseDuration time.Duration) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workerID) == "" {
		return nil, errors.New("run id and worker id are required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}
	return s.store.RenewAgentRunLease(ctx, scope, runID, workerID, s.now(), leaseDuration)
}

func agentRunEligible(run *AgentRun, claim AgentRunClaim) bool {
	if run.Scope != claim.Scope || claim.AssignedAgentID != "" && run.AssignedAgentID != claim.AssignedAgentID {
		return false
	}
	switch run.Status {
	case AgentRunStatusQueued:
		return !run.AvailableAt.After(claim.Now) && (run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now))
	case AgentRunStatusRunning:
		return run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now)
	default:
		return false
	}
}

func agentRunEffectivePriority(run *AgentRun, now time.Time, agingInterval time.Duration) int64 {
	if agingInterval <= 0 {
		agingInterval = time.Minute
	}
	entered := run.QueueEnteredAt
	if entered.IsZero() {
		entered = run.CreatedAt
	}
	age := now.Sub(entered)
	if age < 0 {
		age = 0
	}
	return int64(run.Priority) + int64(age/agingInterval)
}

func agentRunSchedulesBefore(left, right *AgentRun, now time.Time, agingInterval time.Duration) bool {
	leftScore := agentRunEffectivePriority(left, now, agingInterval)
	rightScore := agentRunEffectivePriority(right, now, agingInterval)
	if leftScore != rightScore {
		return leftScore > rightScore
	}
	if left.Deadline != nil || right.Deadline != nil {
		if left.Deadline == nil {
			return false
		}
		if right.Deadline == nil {
			return true
		}
		if !left.Deadline.Equal(*right.Deadline) {
			return left.Deadline.Before(*right.Deadline)
		}
	}
	if !left.QueueEnteredAt.Equal(right.QueueEnteredAt) {
		return left.QueueEnteredAt.Before(right.QueueEnteredAt)
	}
	return left.ID < right.ID
}
