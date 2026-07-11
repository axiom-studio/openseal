package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type ObjectiveScheduleResult struct {
	Examined      int `json:"examined"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Backpressured int `json:"backpressured"`
	Initialized   int `json:"initialized"`
}

// ObjectiveScheduler projects due recurring Objectives into the same durable
// Run primitive used by chat, events, handoffs, and manual work.
type ObjectiveScheduler struct {
	store RunCommandStore
	now   func() time.Time
}

func NewObjectiveScheduler(store RunCommandStore) *ObjectiveScheduler {
	return &ObjectiveScheduler{store: store, now: time.Now}
}

func (s *ObjectiveScheduler) ReconcileScope(ctx context.Context, scope Scope, limit int) (*ObjectiveScheduleResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("objective scheduler store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	now := s.now().UTC()
	objectives, err := s.store.ListObjectives(ctx, ObjectiveFilter{
		Scope: scope, Statuses: []ObjectiveStatus{ObjectiveStatusActive}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	result := &ObjectiveScheduleResult{}
	for _, objective := range objectives {
		if objective == nil || objective.Cadence == nil {
			continue
		}
		result.Examined++
		if objective.NextEvaluationAt == nil {
			next, nextErr := objective.Cadence.Next(now)
			if nextErr != nil {
				return result, fmt.Errorf("objective %s cadence: %w", objective.ID, nextErr)
			}
			if _, updateErr := NewPortfolioService(s.store).UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{
				ExpectedRevision: objective.Revision, NextEvaluationAt: &next,
			}); updateErr != nil && !errors.Is(updateErr, ErrRevisionConflict) {
				return result, updateErr
			}
			result.Initialized++
			continue
		}
		if objective.NextEvaluationAt.After(now) {
			continue
		}
		backpressured, pressureErr := s.backpressured(ctx, objective)
		if pressureErr != nil {
			return result, pressureErr
		}
		if backpressured {
			result.Backpressured++
			continue
		}
		scheduledFor := objective.NextEvaluationAt.UTC()
		created, createErr := NewRunCommandService(s.store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: objective.Scope, ObjectiveID: objective.ID, Owner: objective.Owner,
			AssignedAgentID: objective.Cadence.AssignedAgentID, ConcurrencyKey: "objective:" + objective.ID,
			Goal: objective.Goal, Source: RunSourceSchedule, Priority: objective.Priority,
			Context:        map[string]interface{}{"scheduledFor": scheduledFor.Format(time.RFC3339Nano)},
			Budget:         objective.Cadence.RunBudget,
			IdempotencyKey: fmt.Sprintf("objective-schedule:%s:%s", objective.ID, scheduledFor.Format(time.RFC3339Nano)),
			Actor:          ActivityActor{Type: "service", ID: "objective-scheduler"}, Visibility: ActivityVisibilityScope,
		})
		if createErr != nil {
			return result, fmt.Errorf("schedule objective %s: %w", objective.ID, createErr)
		}
		if created.Event == nil {
			result.Replayed++
		} else {
			result.Scheduled++
		}
		next, nextErr := objective.Cadence.Next(scheduledFor)
		if nextErr != nil {
			return result, nextErr
		}
		current, loadErr := s.store.GetObjective(ctx, scope, objective.ID)
		if loadErr != nil {
			return result, loadErr
		}
		if current == nil {
			return result, ErrObjectiveNotFound
		}
		if _, updateErr := NewPortfolioService(s.store).UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{
			ExpectedRevision: current.Revision, NextEvaluationAt: &next,
		}); updateErr != nil && !errors.Is(updateErr, ErrRevisionConflict) {
			return result, updateErr
		}
	}
	return result, nil
}

func (s *ObjectiveScheduler) backpressured(ctx context.Context, objective *Objective) (bool, error) {
	maximum := objective.Cadence.MaximumConcurrent
	if maximum == 0 {
		maximum = 1
	}
	runs, err := s.store.ListAgentRuns(ctx, AgentRunFilter{
		Scope: objective.Scope, ObjectiveID: objective.ID,
		Statuses: []AgentRunStatus{
			AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusSleeping,
			AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent, AgentRunStatusWaitingForApproval,
			AgentRunStatusWaitingForEvent,
		},
		Limit: maximum,
	})
	return len(runs) >= maximum, err
}
