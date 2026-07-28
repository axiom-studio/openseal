package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ObjectiveScheduleResult struct {
	Scopes          int `json:"scopes"`
	Examined        int `json:"examined"`
	Scheduled       int `json:"scheduled"`
	Replayed        int `json:"replayed"`
	Backpressured   int `json:"backpressured"`
	Suspended       int `json:"suspended"`
	BudgetExhausted int `json:"budgetExhausted"`
	Initialized     int `json:"initialized"`
}

// ObjectiveScheduler projects due recurring Objectives into the same durable
// Run primitive used by chat, events, handoffs, and manual work.
type ObjectiveScheduler struct {
	store interface {
		RunCommandStore
		ObjectiveScopeStore
	}
	initiatives  InitiativeStore
	observations SourceMonitorStore
	now          func() time.Time
}

func NewObjectiveScheduler(store interface {
	RunCommandStore
	ObjectiveScopeStore
}) *ObjectiveScheduler {
	initiatives, _ := any(store).(InitiativeStore)
	observations, _ := any(store).(SourceMonitorStore)
	return &ObjectiveScheduler{store: store, initiatives: initiatives, observations: observations, now: time.Now}
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
		total.BudgetExhausted += result.BudgetExhausted
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
			next, nextErr := objective.Cadence.NextFor(objective.ID, now)
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
		scheduledFor := objective.NextEvaluationAt.UTC()
		idempotencyKey := fmt.Sprintf("objective-schedule:%s:%s", objective.ID, scheduledFor.Format(time.RFC3339Nano))
		// A process may stop after atomically creating the Run and before moving
		// the Objective cursor. Advance that exact durable schedule occurrence
		// before backpressure checks and without rebuilding its evidence snapshot.
		existing, loadErr := s.store.GetAgentRun(ctx, scope, runIDForIdempotencyKey(scope, idempotencyKey))
		if loadErr != nil {
			return result, loadErr
		}
		if existing != nil {
			result.Replayed++
			if advanceErr := s.advanceObjective(ctx, scope, objective, scheduledFor); advanceErr != nil {
				return result, advanceErr
			}
			continue
		}
		initiative, lineageErr := s.resolveInitiative(ctx, objective)
		if lineageErr != nil {
			return result, lineageErr
		}
		if initiative != nil && initiative.Status != InitiativeStatusActive {
			if deferErr := s.deferObjective(ctx, objective, now, ObjectiveScheduleSuspended, "Initiative is not active"); deferErr != nil && !errors.Is(deferErr, ErrRevisionConflict) {
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
			if deferErr := s.deferObjective(ctx, objective, now, ObjectiveScheduleBackpressured, "Maximum concurrent Runs are already active"); deferErr != nil && !errors.Is(deferErr, ErrRevisionConflict) {
				return result, deferErr
			}
			result.Backpressured++
			continue
		}
		if budgetErr := validateObjectiveRunBudget(objective, objective.Cadence.RunBudget); budgetErr != nil {
			if !errors.Is(budgetErr, ErrBudgetExhausted) {
				return result, budgetErr
			}
			if conditionErr := s.conditionObjective(ctx, objective, now, ObjectiveScheduleBudgetExhausted, "Objective budget cannot allocate another Run"); conditionErr != nil && !errors.Is(conditionErr, ErrRevisionConflict) {
				return result, conditionErr
			}
			result.BudgetExhausted++
			continue
		}
		contextValues := map[string]interface{}{"scheduledFor": scheduledFor.Format(time.RFC3339Nano)}
		if objective.Cadence.JitterSeconds > 0 {
			windowStart, windowEnd, windowErr := objective.Cadence.OccurrenceWindow(scheduledFor)
			if windowErr != nil {
				return result, fmt.Errorf("objective %s cadence window: %w", objective.ID, windowErr)
			}
			contextValues["scheduleWindowStart"] = windowStart.Format(time.RFC3339Nano)
			contextValues["scheduleWindowEnd"] = windowEnd.Format(time.RFC3339Nano)
		}
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
		if initiative != nil {
			contextValues["initiativeId"] = initiative.ID
			if monitorID, _ := contextValues["sourceMonitorId"].(string); strings.TrimSpace(monitorID) != "" {
				if err := s.validateSourceMonitorLineage(objective, initiative, strings.TrimSpace(monitorID)); err != nil {
					return result, err
				}
			} else {
				var projection *ObjectiveEvidenceProjection
				if objective.Cadence.RunTemplate != nil {
					projection = objective.Cadence.RunTemplate.EvidenceProjection
				}
				snapshot, snapshotErr := buildEvidenceSnapshot(ctx, s.observations, scope, initiative.ID, scheduledFor, projection)
				if snapshotErr != nil {
					return result, fmt.Errorf("objective %s evidence projection: %w", objective.ID, snapshotErr)
				}
				projected, projectionErr := evidenceSnapshotContext(snapshot)
				if projectionErr != nil {
					return result, fmt.Errorf("objective %s evidence projection: %w", objective.ID, projectionErr)
				}
				if projected != nil {
					contextValues[EvidenceSnapshotContextKey] = projected
				}
			}
		}
		assignedAgentID := strings.TrimSpace(objective.Cadence.AssignedAgentID)
		if assignedAgentID == "" && objective.Owner.Type == OwnerTypeAgent {
			assignedAgentID = objective.Owner.ID
		}
		created, createErr := NewRunCommandService(s.store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: objective.Scope, ObjectiveID: objective.ID, Owner: objective.Owner,
			AssignedAgentID: assignedAgentID, Entrypoint: entrypoint, ConcurrencyKey: "objective:" + objective.ID,
			Goal: objective.Goal, Source: RunSourceSchedule, Priority: objective.Priority,
			Context: contextValues, Policy: policy,
			Budget:         objective.Cadence.RunBudget,
			IdempotencyKey: idempotencyKey,
			Actor:          ActivityActor{Type: "service", ID: "objective-scheduler"}, Visibility: objectiveScheduleVisibility(objective),
		})
		if createErr != nil {
			if errors.Is(createErr, ErrBudgetExhausted) {
				if conditionErr := s.conditionObjective(ctx, objective, now, ObjectiveScheduleBudgetExhausted, "Objective budget cannot allocate another Run"); conditionErr != nil && !errors.Is(conditionErr, ErrRevisionConflict) {
					return result, conditionErr
				}
				result.BudgetExhausted++
				continue
			}
			return result, fmt.Errorf("schedule objective %s: %w", objective.ID, createErr)
		}
		if created.Event == nil {
			result.Replayed++
		} else {
			result.Scheduled++
		}
		if advanceErr := s.advanceObjective(ctx, scope, objective, scheduledFor); advanceErr != nil {
			return result, advanceErr
		}
	}
	return result, nil
}

func (s *ObjectiveScheduler) resolveInitiative(ctx context.Context, objective *Objective) (*Initiative, error) {
	if s.initiatives == nil {
		return nil, nil
	}
	values, err := s.initiatives.ListInitiatives(ctx, InitiativeFilter{Scope: objective.Scope, ObjectiveID: objective.ID, Limit: 2})
	if err != nil {
		return nil, fmt.Errorf("objective %s Initiative membership: %w", objective.ID, err)
	}
	if len(values) > 1 {
		return nil, fmt.Errorf("objective %s belongs to multiple Initiatives in scope", objective.ID)
	}
	var declaredID string
	if objective.Cadence != nil && objective.Cadence.RunTemplate != nil {
		declaredID, _ = objective.Cadence.RunTemplate.Context["initiativeId"].(string)
		declaredID = strings.TrimSpace(declaredID)
	}
	if len(values) == 0 {
		if declaredID != "" {
			return nil, fmt.Errorf("objective %s declares Initiative %s but is not a member", objective.ID, declaredID)
		}
		return nil, nil
	}
	if declaredID != "" && declaredID != values[0].ID {
		return nil, fmt.Errorf("objective %s Initiative provenance has drifted", objective.ID)
	}
	return values[0], nil
}

func (s *ObjectiveScheduler) validateSourceMonitorLineage(objective *Objective, initiative *Initiative, monitorID string) error {
	monitor, found := initiativeSourceMonitor(initiative, monitorID)
	if !found || monitor.ObjectiveID != objective.ID || monitor.AssignedAgentID != objective.Cadence.AssignedAgentID {
		return fmt.Errorf("objective %s source monitor provenance has drifted", objective.ID)
	}
	if invocation := objective.Cadence.RunTemplate.Capability; invocation == nil || monitor.SkillID != invocation.SkillID || monitor.SkillVersion != invocation.SkillVersion || monitor.Action != invocation.Action {
		return fmt.Errorf("objective %s source monitor capability has drifted", objective.ID)
	}
	if policyRef, _ := objective.Cadence.RunTemplate.Policy["sourcePolicyRef"].(string); monitor.SourcePolicyRef != strings.TrimSpace(policyRef) {
		return fmt.Errorf("objective %s source monitor policy has drifted", objective.ID)
	}
	return nil
}

func (s *ObjectiveScheduler) advanceObjective(ctx context.Context, scope Scope, objective *Objective, scheduledFor time.Time) error {
	next, err := objective.Cadence.NextFor(objective.ID, scheduledFor)
	if err != nil {
		return err
	}
	current, err := s.store.GetObjective(ctx, scope, objective.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return ErrObjectiveNotFound
	}
	_, err = NewPortfolioService(s.store).UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{
		ExpectedRevision: current.Revision, NextEvaluationAt: &next, ClearScheduleCondition: true,
		Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Visibility: objectiveScheduleVisibility(current), Summary: "Objective schedule advanced",
	})
	if errors.Is(err, ErrRevisionConflict) {
		return nil
	}
	return err
}

func (s *ObjectiveScheduler) deferObjective(ctx context.Context, objective *Objective, now time.Time, state ObjectiveScheduleState, reason string) error {
	next, err := objective.Cadence.NextFor(objective.ID, now)
	if err != nil {
		return fmt.Errorf("objective %s cadence: %w", objective.ID, err)
	}
	_, err = NewPortfolioService(s.store).UpdateObjective(ctx, objective.Scope, objective.ID, UpdateObjectiveRequest{
		ExpectedRevision: objective.Revision, NextEvaluationAt: &next, ScheduleCondition: scheduleCondition(objective.ScheduleCondition, state, reason, now),
		Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Visibility: objectiveScheduleVisibility(objective), Summary: "Objective schedule deferred: " + reason,
	})
	return err
}

func (s *ObjectiveScheduler) conditionObjective(ctx context.Context, objective *Objective, now time.Time, state ObjectiveScheduleState, reason string) error {
	if objective.ScheduleCondition != nil && objective.ScheduleCondition.State == state && objective.ScheduleCondition.Reason == reason {
		return nil
	}
	_, err := NewPortfolioService(s.store).UpdateObjective(ctx, objective.Scope, objective.ID, UpdateObjectiveRequest{
		ExpectedRevision: objective.Revision, ScheduleCondition: scheduleCondition(objective.ScheduleCondition, state, reason, now),
		Actor: ActivityActor{Type: "service", ID: "objective-scheduler"}, Visibility: objectiveScheduleVisibility(objective), Summary: "Objective schedule blocked: " + reason,
	})
	return err
}

func scheduleCondition(current *ObjectiveScheduleCondition, state ObjectiveScheduleState, reason string, now time.Time) *ObjectiveScheduleCondition {
	since := now.UTC()
	if current != nil && current.State == state && current.Reason == reason && !current.Since.IsZero() {
		since = current.Since
	}
	return &ObjectiveScheduleCondition{State: state, Reason: reason, Since: since, UpdatedAt: now.UTC()}
}

func objectiveScheduleVisibility(objective *Objective) ActivityVisibility {
	if objective != nil && objective.Owner.Type == OwnerTypeTeam {
		return ActivityVisibilityTeam
	}
	return ActivityVisibilityScope
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
