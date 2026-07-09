package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type WakeSignal struct {
	ID        string
	Scope     Scope
	Type      string
	Reference string
	Payload   map[string]interface{}
	At        time.Time
	Actor     ActivityActor
}

type WokenRun struct {
	Run   *AgentRun
	Event *ActivityEvent
}

type WakeResult struct {
	Runs []WokenRun
}

type AgentRunWakeService struct {
	portfolio PortfolioStore
	activity  *RunActivityService
	now       func() time.Time
}

func NewAgentRunWakeService(portfolio PortfolioStore, activity RunActivityStore) *AgentRunWakeService {
	return &AgentRunWakeService{
		portfolio: portfolio, activity: NewRunActivityService(portfolio, activity), now: time.Now,
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
		signal.Actor = ActivityActor{Type: "event", ID: signal.Type}
	}
	result := &WakeResult{Runs: make([]WokenRun, 0)}
	const pageSize = 200
	waiting := make([]*AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		runs, err := s.portfolio.ListAgentRuns(ctx, AgentRunFilter{
			Scope: signal.Scope,
			Statuses: []AgentRunStatus{
				AgentRunStatusSleeping, AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent,
				AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent,
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
		woken, event, err := s.activity.TransitionRun(ctx, signal.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusQueued,
			Summary: fmt.Sprintf("Run woken by %s signal", signal.Type), Actor: signal.Actor,
			Payload: signal.Payload, CorrelationID: signal.ID, WakeSignalID: signal.ID, EventType: "run.woken",
			OccurredAt: &signal.At,
		})
		if errors.Is(err, ErrRevisionConflict) {
			current, getErr := s.portfolio.GetAgentRun(ctx, signal.Scope, run.ID)
			if getErr != nil {
				return nil, getErr
			}
			if current.LastWakeSignalID == signal.ID || current.Status == AgentRunStatusQueued {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
	}
	return result, nil
}

func wakeConditionMatches(condition *WakeCondition, signal WakeSignal) bool {
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
