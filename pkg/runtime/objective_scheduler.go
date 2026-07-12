package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ObjectiveScheduleResult struct {
	Scopes        int `json:"scopes"`
	Examined      int `json:"examined"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Backpressured int `json:"backpressured"`
	Suspended     int `json:"suspended"`
	Initialized   int `json:"initialized"`
}

// ObjectiveScheduler projects due recurring Objectives into the same durable
// Run primitive used by chat, events, handoffs, and manual work.
type ObjectiveScheduler struct {
	store interface {
		RunCommandStore
		ObjectiveScopeStore
	}
	initiatives InitiativeStore
	now         func() time.Time
}

func NewObjectiveScheduler(store interface {
	RunCommandStore
	ObjectiveScopeStore
}) *ObjectiveScheduler {
	initiatives, _ := any(store).(InitiativeStore)
	return &ObjectiveScheduler{store: store, initiatives: initiatives, now: time.Now}
}

func (s *ObjectiveScheduler) ReconcileAll(ctx context.Context, limitPerScope int) (*ObjectiveScheduleResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("objective scheduler store is not configured")
	}
	scopes, err := s.store.ListObjectiveScopes(ctx)
	if err != nil {
		return nil, err
	}
	total := &ObjectiveScheduleResult{Scopes: len(scopes)}
	for _, scope := range scopes {
		result, reconcileErr := s.ReconcileScope(ctx, scope, limitPerScope)
		if reconcileErr != nil {
			return total, reconcileErr
		}
		total.Examined += result.Examined
		total.Scheduled += result.Scheduled
		total.Replayed += result.Replayed
		total.Backpressured += result.Backpressured
		total.Suspended += result.Suspended
		total.Initialized += result.Initialized
	}
	return total, nil
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
				Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Summary: "Objective schedule initialized",
			}); updateErr != nil && !errors.Is(updateErr, ErrRevisionConflict) {
				return result, updateErr
			}
			result.Initialized++
			continue
		}
		if objective.NextEvaluationAt.After(now) {
			continue
		}
		active, monitorErr := s.monitorInitiativeActive(ctx, objective)
		if monitorErr != nil {
			return result, monitorErr
		}
		if !active {
			if deferErr := s.deferObjective(ctx, objective, now); deferErr != nil && !errors.Is(deferErr, ErrRevisionConflict) {
				return result, deferErr
			}
			result.Suspended++
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
		contextValues := map[string]interface{}{"scheduledFor": scheduledFor.Format(time.RFC3339Nano)}
		entrypoint := ""
		policy := map[string]interface{}(nil)
		if objective.Cadence.RunTemplate != nil {
			entrypoint = strings.TrimSpace(objective.Cadence.RunTemplate.Entrypoint)
			for key, value := range cloneMap(objective.Cadence.RunTemplate.Context) {
				contextValues[key] = value
			}
			policy = cloneMap(objective.Cadence.RunTemplate.Policy)
			if invocation := objective.Cadence.RunTemplate.Capability; invocation != nil {
				contextValues["capabilityInvocation"] = map[string]interface{}{
					"skillId": invocation.SkillID, "skillVersion": invocation.SkillVersion,
					"action": invocation.Action, "inputs": cloneMap(invocation.Inputs),
				}
			}
		}
		created, createErr := NewRunCommandService(s.store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: objective.Scope, ObjectiveID: objective.ID, Owner: objective.Owner,
			AssignedAgentID: objective.Cadence.AssignedAgentID, Entrypoint: entrypoint, ConcurrencyKey: "objective:" + objective.ID,
			Goal: objective.Goal, Source: RunSourceSchedule, Priority: objective.Priority,
			Context: contextValues, Policy: policy,
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
			Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Summary: "Objective schedule advanced",
		}); updateErr != nil && !errors.Is(updateErr, ErrRevisionConflict) {
			return result, updateErr
		}
	}
	return result, nil
}

func (s *ObjectiveScheduler) monitorInitiativeActive(ctx context.Context, objective *Objective) (bool, error) {
	if objective == nil || objective.Cadence == nil || objective.Cadence.RunTemplate == nil {
		return true, nil
	}
	contextValues := objective.Cadence.RunTemplate.Context
	monitorID, hasMonitor := contextValues["sourceMonitorId"].(string)
	monitorID = strings.TrimSpace(monitorID)
	if !hasMonitor || monitorID == "" {
		return true, nil
	}
	initiativeID, ok := contextValues["initiativeId"].(string)
	initiativeID = strings.TrimSpace(initiativeID)
	if !ok || initiativeID == "" {
		return false, fmt.Errorf("objective %s source monitor %s has no initiative provenance", objective.ID, monitorID)
	}
	if s.initiatives == nil {
		return false, fmt.Errorf("objective %s source monitor cannot run without Initiative persistence", objective.ID)
	}
	initiative, err := s.initiatives.GetInitiative(ctx, objective.Scope, initiativeID)
	if err != nil {
		return false, fmt.Errorf("objective %s source monitor initiative: %w", objective.ID, err)
	}
	monitor, found := initiativeSourceMonitor(initiative, monitorID)
	if !found || monitor.ObjectiveID != objective.ID || monitor.AssignedAgentID != objective.Cadence.AssignedAgentID {
		return false, fmt.Errorf("objective %s source monitor provenance has drifted", objective.ID)
	}
	if invocation := objective.Cadence.RunTemplate.Capability; invocation == nil || monitor.SkillID != invocation.SkillID || monitor.SkillVersion != invocation.SkillVersion || monitor.Action != invocation.Action {
		return false, fmt.Errorf("objective %s source monitor capability has drifted", objective.ID)
	}
	if policyRef, _ := objective.Cadence.RunTemplate.Policy["sourcePolicyRef"].(string); monitor.SourcePolicyRef != strings.TrimSpace(policyRef) {
		return false, fmt.Errorf("objective %s source monitor policy has drifted", objective.ID)
	}
	return initiative.Status == InitiativeStatusActive, nil
}

func (s *ObjectiveScheduler) deferObjective(ctx context.Context, objective *Objective, now time.Time) error {
	next, err := objective.Cadence.Next(now)
	if err != nil {
		return fmt.Errorf("objective %s cadence: %w", objective.ID, err)
	}
	_, err = NewPortfolioService(s.store).UpdateObjective(ctx, objective.Scope, objective.ID, UpdateObjectiveRequest{
		ExpectedRevision: objective.Revision, NextEvaluationAt: &next,
		Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Summary: "Source monitor schedule deferred while Initiative is not active",
	})
	return err
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
