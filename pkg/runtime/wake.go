package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

type WakeSignal struct {
	ID        string
	Scope     models.Scope
	Type      string
	Reference string
	Payload   map[string]interface{}
	At        time.Time
	Actor     models.ActivityActor
}

type WokenRun struct {
	Run   *models.AgentRun
	Event *models.ActivityEvent
}

type WakeResult struct {
	Runs       []WokenRun
	NextWakeAt *time.Time
}

type AgentRunWakeService struct {
	portfolio models.PortfolioStore
	activity  *activity.RunActivityService
	now       func() time.Time
}

func NewAgentRunWakeService(portfolio models.PortfolioStore, activityStore models.RunActivityStore, opts ...models.ServiceOption) *AgentRunWakeService {
	return &AgentRunWakeService{
		portfolio: portfolio, activity: activity.NewRunActivityService(portfolio, activityStore, opts...), now: models.ClockFrom(opts),
	}
}

func (s *AgentRunWakeService) Wake(ctx context.Context, signal WakeSignal) (*WakeResult, error) {
	if s == nil || s.portfolio == nil || s.activity == nil {
		return nil, errors.New("agent run wake service is not configured")
	}
	if err := signal.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(signal.ID) == "" || strings.TrimSpace(signal.Type) == "" {
		return nil, errors.New("wake signal id and type are required")
	}
	if signal.At.IsZero() {
		signal.At = s.now()
	}
	if signal.Actor.Type == "" {
		signal.Actor = models.ActivityActor{Type: "event", ID: signal.Type}
	}
	result := &WakeResult{Runs: make([]WokenRun, 0)}
	const pageSize = 200
	waiting := make([]*models.AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		runs, err := s.portfolio.ListAgentRuns(ctx, models.AgentRunFilter{
			Scope: signal.Scope,
			Statuses: []models.AgentRunStatus{
				models.AgentRunStatusSleeping, models.AgentRunStatusWaitingForDependency, models.AgentRunStatusWaitingForAgent,
				models.AgentRunStatusWaitingForApproval, models.AgentRunStatusWaitingForEvent,
			},
			Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		waiting = append(waiting, runs...)
		if len(runs) < pageSize {
			break
		}
	}
	for _, run := range waiting {
		if run.LastWakeSignalID == signal.ID || !wakeConditionMatches(run.WakeCondition, signal) {
			continue
		}
		woken, event, err := s.wakeOne(ctx, run, signal)
		if err != nil {
			return nil, err
		}
		if woken != nil {
			result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
		}
	}
	return result, nil
}

func (s *AgentRunWakeService) WakeDueTimers(ctx context.Context, scope models.Scope, at time.Time) (*WakeResult, error) {
	if s == nil || s.portfolio == nil || s.activity == nil {
		return nil, errors.New("agent run wake service is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = s.now()
	}
	maintenance, err := s.wakeCompletedSkillRuntimeMaintenances(ctx, scope, at)
	if err != nil {
		return nil, err
	}
	// A builder waiting on a try_agent trial has a deadline too: past it,
	// the resume cancels the trial and reports timed_out.
	runs, err := s.portfolio.ListAgentRuns(ctx, models.AgentRunFilter{
		Scope: scope, Statuses: []models.AgentRunStatus{models.AgentRunStatusSleeping, models.AgentRunStatusWaitingForEvent},
	})
	if err != nil {
		return nil, err
	}
	result := maintenance
	for _, run := range runs {
		condition := run.WakeCondition
		if condition == nil || condition.WakeAt == nil {
			continue
		}
		if !(run.Status == models.AgentRunStatusSleeping && condition.Type == "timer") &&
			!(run.Status == models.AgentRunStatusWaitingForEvent && condition.Type == AgentTrialWakeType) {
			continue
		}
		if at.Before(*condition.WakeAt) {
			if result.NextWakeAt == nil || condition.WakeAt.Before(*result.NextWakeAt) {
				next := *condition.WakeAt
				result.NextWakeAt = &next
			}
			continue
		}
		signal := WakeSignal{
			ID: fmt.Sprintf("timer:%s:%d", run.ID, condition.WakeAt.UnixNano()), Scope: scope,
			Type: "timer", Reference: condition.Reference, At: at,
			Actor: models.ActivityActor{Type: "scheduler", ID: "timer"},
		}
		woken, event, err := s.wakeOne(ctx, run, signal)
		if err != nil {
			return nil, err
		}
		if woken != nil {
			result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
		}
	}
	return result, nil
}

func (s *AgentRunWakeService) wakeOne(ctx context.Context, run *models.AgentRun, signal WakeSignal) (*models.AgentRun, *models.ActivityEvent, error) {
	woken, event, err := s.activity.TransitionRun(ctx, signal.Scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusQueued,
		Summary: fmt.Sprintf("Run woken by %s signal", signal.Type), Actor: signal.Actor,
		Payload: signal.Payload, CorrelationID: signal.ID, WakeSignalID: signal.ID, EventType: "run.woken",
		OccurredAt: &signal.At,
	})
	if errors.Is(err, models.ErrRevisionConflict) {
		current, getErr := s.portfolio.GetAgentRun(ctx, signal.Scope, run.ID)
		if getErr != nil {
			return nil, nil, getErr
		}
		if current.LastWakeSignalID == signal.ID || current.Status == models.AgentRunStatusQueued {
			return nil, nil, nil
		}
	}
	return woken, event, err
}

func wakeConditionMatches(condition *models.WakeCondition, signal WakeSignal) bool {
	if condition == nil || condition.Type != signal.Type {
		return false
	}
	if condition.WakeAt != nil && signal.At.Before(*condition.WakeAt) {
		return false
	}
	if condition.Reference != "" && condition.Reference != signal.Reference {
		return false
	}
	for key, expected := range condition.Predicate {
		actual, ok := signal.Payload[key]
		if !ok || !reflect.DeepEqual(actual, expected) {
			return false
		}
	}
	return true
}

// AgentTrialWakeType is the wake a parent waits on while its trial runs.
const AgentTrialWakeType = "agent_trial"
