package activity_test

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"testing"
)

func TestRunTransitionValidation(t *testing.T) {
	store := newActivityStubStore()
	service := activity.NewRunActivityService(store, store)
	scope := models.Scope{Kind: "local", ID: "test"}
	run := &models.AgentRun{ID: "run", RootRunID: "run", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "work", Status: models.AgentRunStatusQueued, Revision: 1}
	store.run = run

	_, _, err := service.TransitionRun(context.Background(), scope, run.ID, activity.RunTransitionRequest{ExpectedRevision: 1, Status: models.AgentRunStatusCompleted})
	if !errors.Is(err, models.ErrInvalidRunTransition) {
		t.Fatalf("illegal transition error = %v", err)
	}
	_, _, err = service.TransitionRun(context.Background(), scope, run.ID, activity.RunTransitionRequest{ExpectedRevision: 1, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.TransitionRun(context.Background(), scope, run.ID, activity.RunTransitionRequest{ExpectedRevision: 1, Status: models.AgentRunStatusCompleted})
	if !errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("stale transition error = %v", err)
	}
}

type activityStubStore struct {
	run    *models.AgentRun
	events []*models.ActivityEvent
}

func (s *activityStubStore) CreateAgentRunWithEvent(_ context.Context, run *models.AgentRun, event *models.ActivityEvent) (*models.ActivityEvent, error) {
	s.run = models.CloneAgentRun(run)
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event, nil
}

func newActivityStubStore() *activityStubStore                                        { return &activityStubStore{} }
func (s *activityStubStore) CreateObjective(context.Context, *models.Objective) error { return nil }
func (s *activityStubStore) GetObjective(context.Context, models.Scope, string) (*models.Objective, error) {
	return nil, nil
}
func (s *activityStubStore) ListObjectives(context.Context, models.ObjectiveFilter) ([]*models.Objective, error) {
	return nil, nil
}
func (s *activityStubStore) UpdateObjective(context.Context, *models.Objective, int64) error {
	return nil
}
func (s *activityStubStore) CreateAgentRun(context.Context, *models.AgentRun) error { return nil }
func (s *activityStubStore) GetAgentRun(context.Context, models.Scope, string) (*models.AgentRun, error) {
	return models.CloneAgentRun(s.run), nil
}
func (s *activityStubStore) ListAgentRuns(context.Context, models.AgentRunFilter) ([]*models.AgentRun, error) {
	return nil, nil
}
func (s *activityStubStore) UpdateAgentRunWithEvent(_ context.Context, run *models.AgentRun, expected int64, event *models.ActivityEvent, _ *models.AgentRunLeaseGuard) (*models.ActivityEvent, error) {
	if s.run.Revision != expected {
		return nil, models.ErrRevisionConflict
	}
	s.run = models.CloneAgentRun(run)
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event, nil
}
func (s *activityStubStore) AppendActivity(_ context.Context, event *models.ActivityEvent) (*models.ActivityEvent, error) {
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event, nil
}
func (s *activityStubStore) ListActivity(context.Context, models.ActivityFilter) ([]*models.ActivityEvent, error) {
	return s.events, nil
}
