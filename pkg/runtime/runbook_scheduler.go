package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

type RunbookScheduleResult struct {
	Scopes        int `json:"scopes"`
	Examined      int `json:"examined"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Backpressured int `json:"backpressured"`
	Suspended     int `json:"suspended"`
	Initialized   int `json:"initialized"`
}

// RunbookScheduler projects due Runbook trigger occurrences into durable Runs.
// The persisted activation cursor plus deterministic Run idempotency makes the
// operation safe across replicas, crashes, and retries without one timer or
// goroutine per Agent.
type RunbookScheduler struct {
	store interface {
		RunCommandStore
		PortfolioStore
		RunbookActivationStore
		SourceMonitorStore
	}
	now func() time.Time
}

func NewRunbookScheduler(store interface {
	RunCommandStore
	PortfolioStore
	RunbookActivationStore
	SourceMonitorStore
}) *RunbookScheduler {
	return &RunbookScheduler{store: store, now: time.Now}
}

func (s *RunbookScheduler) ReconcileAll(ctx context.Context, limitPerScope int) (*RunbookScheduleResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook scheduler store is not configured")
	}
	scopes, err := s.store.ListRunbookActivationScopes(ctx)
	if err != nil {
		return nil, err
	}
	total := &RunbookScheduleResult{Scopes: len(scopes)}
	for _, scope := range scopes {
		result, err := s.ReconcileScope(ctx, scope, limitPerScope)
		if err != nil {
			return total, err
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

func (s *RunbookScheduler) ReconcileScope(ctx context.Context, scope Scope, limit int) (*RunbookScheduleResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook scheduler store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	activations, err := s.store.ListRunbookActivations(ctx, RunbookActivationFilter{
		Scope: scope, Statuses: []RunbookActivationStatus{RunbookActivationActive}, TriggerKinds: []runbook.TriggerKind{runbook.TriggerSchedule}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	result := &RunbookScheduleResult{}
	for _, activation := range activations {
		result.Examined++
		if activation.NextOccurrenceBase == nil {
			if err := s.initialize(ctx, activation, now); err != nil && !errors.Is(err, ErrRunbookActivationRevision) {
				return result, err
			}
			result.Initialized++
			continue
		}
		if activation.NextRunAt.After(now) {
			continue
		}
		base := activation.NextOccurrenceBase.UTC()
		idempotencyKey := fmt.Sprintf("runbook-schedule:%s:%s", activation.ID, base.Format(time.RFC3339Nano))
		existing, err := s.store.GetAgentRun(ctx, scope, runIDForIdempotencyKey(scope, idempotencyKey))
		if err != nil {
			return result, err
		}
		if existing != nil {
			result.Replayed++
			if err := s.advance(ctx, activation, base); err != nil && !errors.Is(err, ErrRunbookActivationRevision) {
				return result, err
			}
			continue
		}
		objective, err := s.store.GetObjective(ctx, scope, activation.ObjectiveID)
		if err != nil {
			return result, err
		}
		if objective == nil {
			return result, fmt.Errorf("Runbook activation %s Objective %s: %w", activation.ID, activation.ObjectiveID, ErrObjectiveNotFound)
		}
		if objective.Owner != activation.Owner {
			return result, fmt.Errorf("Runbook activation %s owner drifted from Objective %s", activation.ID, objective.ID)
		}
		if objective.Status != ObjectiveStatusActive {
			result.Suspended++
			continue
		}
		backpressured, err := s.backpressured(ctx, activation)
		if err != nil {
			return result, err
		}
		if backpressured {
			result.Backpressured++
			continue
		}
		contextValues := cloneMap(activation.Input)
		if contextValues == nil {
			contextValues = map[string]interface{}{}
		}
		windowStart, windowEnd, err := activation.Trigger.Schedule.Window(base)
		if err != nil {
			return result, fmt.Errorf("Runbook activation %s schedule window: %w", activation.ID, err)
		}
		contextValues["scheduledFor"] = activation.NextRunAt.UTC().Format(time.RFC3339Nano)
		contextValues["scheduleWindowStart"] = windowStart.Format(time.RFC3339Nano)
		contextValues["scheduleWindowEnd"] = windowEnd.Format(time.RFC3339Nano)
		contextValues["runbookActivationId"] = activation.ID
		contextValues["runbookDefinitionId"] = activation.DefinitionID
		contextValues["runbookDefinitionVersion"] = activation.DefinitionVersion
		contextValues["runbookTriggerId"] = activation.TriggerID
		if activation.Trigger.Evidence != nil {
			projectID, _ := contextValues["projectId"].(string)
			projectID = strings.TrimSpace(projectID)
			if projectID == "" {
				return result, fmt.Errorf("Runbook activation %s evidence projection requires projectId input", activation.ID)
			}
			snapshot, snapshotErr := buildEvidenceSnapshot(ctx, s.store, scope, projectID, now, activation.Trigger.Evidence)
			if snapshotErr != nil {
				return result, fmt.Errorf("Runbook activation %s evidence projection: %w", activation.ID, snapshotErr)
			}
			projected, snapshotErr := evidenceSnapshotContext(snapshot)
			if snapshotErr != nil {
				return result, snapshotErr
			}
			if projected != nil {
				contextValues[EvidenceSnapshotContextKey] = projected
			}
		}
		created, err := NewRunCommandService(s.store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, ObjectiveID: objective.ID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID,
			Entrypoint: activation.Trigger.Entrypoint, ConcurrencyKey: "runbook:" + activation.ID,
			Goal: objective.Goal, Source: RunSourceSchedule, Priority: objective.Priority, Context: contextValues,
			Policy: cloneMap(activation.Policy), Budget: cloneBudgetPolicy(activation.Budget), IdempotencyKey: idempotencyKey,
			Actor: ActivityActor{Type: "service", ID: "runbook-scheduler"}, Visibility: runbookScheduleVisibility(activation),
		})
		if err != nil {
			return result, fmt.Errorf("schedule Runbook activation %s: %w", activation.ID, err)
		}
		if created.Event == nil {
			result.Replayed++
		} else {
			result.Scheduled++
		}
		if err := s.advance(ctx, activation, base); err != nil && !errors.Is(err, ErrRunbookActivationRevision) {
			return result, err
		}
	}
	return result, nil
}

func (s *RunbookScheduler) initialize(ctx context.Context, activation *RunbookActivation, now time.Time) error {
	base, err := activation.Trigger.Schedule.NextBase(now)
	if err != nil {
		return fmt.Errorf("Runbook activation %s schedule: %w", activation.ID, err)
	}
	due, err := activation.Trigger.Schedule.DueAt(runbookTriggerKey(activation), base)
	if err != nil {
		return err
	}
	updated := cloneRunbookActivation(activation)
	updated.NextOccurrenceBase, updated.NextRunAt = &base, &due
	updated.Revision++
	updated.UpdatedAt = now.UTC()
	return s.store.UpdateRunbookActivation(ctx, updated, activation.Revision)
}

func (s *RunbookScheduler) advance(ctx context.Context, activation *RunbookActivation, base time.Time) error {
	nextBase, err := activation.Trigger.Schedule.NextBase(base)
	if err != nil {
		return fmt.Errorf("Runbook activation %s next schedule: %w", activation.ID, err)
	}
	nextDue, err := activation.Trigger.Schedule.DueAt(runbookTriggerKey(activation), nextBase)
	if err != nil {
		return err
	}
	updated := cloneRunbookActivation(activation)
	updated.NextOccurrenceBase, updated.NextRunAt = &nextBase, &nextDue
	updated.Revision++
	updated.UpdatedAt = s.now().UTC()
	return s.store.UpdateRunbookActivation(ctx, updated, activation.Revision)
}

func (s *RunbookScheduler) backpressured(ctx context.Context, activation *RunbookActivation) (bool, error) {
	maximum := activation.MaximumConcurrent
	if maximum == 0 {
		maximum = 1
	}
	runs, err := s.store.ListAgentRuns(ctx, AgentRunFilter{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID,
		Statuses: []AgentRunStatus{
			AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusSleeping,
			AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent, AgentRunStatusWaitingForApproval,
			AgentRunStatusWaitingForEvent,
		},
		Limit: 500,
	})
	if err != nil {
		return false, err
	}
	count := 0
	for _, run := range runs {
		if id, _ := run.Context["runbookActivationId"].(string); strings.TrimSpace(id) == activation.ID {
			count++
		}
	}
	return count >= maximum, nil
}

func runbookTriggerKey(activation *RunbookActivation) string {
	return activation.Scope.key() + ":" + activation.ID + ":" + activation.TriggerID + ":" + fmt.Sprint(activation.Revision)
}

func runbookScheduleVisibility(activation *RunbookActivation) ActivityVisibility {
	if activation != nil && activation.Owner.Type == OwnerTypeTeam {
		return ActivityVisibilityTeam
	}
	return ActivityVisibilityScope
}
