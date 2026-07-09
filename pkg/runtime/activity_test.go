package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestRunTransitionValidation(t *testing.T) {
	store := newActivityStubStore()
	service := NewRunActivityService(store, store)
	scope := Scope{Kind: "local", ID: "test"}
	run := &AgentRun{ID: "run", RootRunID: "run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Status: AgentRunStatusQueued, Revision: 1}
	store.run = run

	_, _, err := service.TransitionRun(context.Background(), scope, run.ID, RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusCompleted})
	if !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("illegal transition error = %v", err)
	}
	_, _, err = service.TransitionRun(context.Background(), scope, run.ID, RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.TransitionRun(context.Background(), scope, run.ID, RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusCompleted})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale transition error = %v", err)
	}
}

type activityStubStore struct {
	run    *AgentRun
	events []*ActivityEvent
}

func newActivityStubStore() *activityStubStore                                 { return &activityStubStore{} }
func (s *activityStubStore) CreateObjective(context.Context, *Objective) error { return nil }
func (s *activityStubStore) GetObjective(context.Context, Scope, string) (*Objective, error) {
	return nil, nil
}
func (s *activityStubStore) ListObjectives(context.Context, ObjectiveFilter) ([]*Objective, error) {
	return nil, nil
}
func (s *activityStubStore) UpdateObjective(context.Context, *Objective, int64) error { return nil }
func (s *activityStubStore) CreateAgentRun(context.Context, *AgentRun) error          { return nil }
func (s *activityStubStore) GetAgentRun(context.Context, Scope, string) (*AgentRun, error) {
	return cloneAgentRun(s.run), nil
}
func (s *activityStubStore) ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error) {
	return nil, nil
}
func (s *activityStubStore) UpdateAgentRunWithEvent(_ context.Context, run *AgentRun, expected int64, event *ActivityEvent, _ *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if s.run.Revision != expected {
		return nil, ErrRevisionConflict
	}
	s.run = cloneAgentRun(run)
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event, nil
}
func (s *activityStubStore) AppendActivity(_ context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return event, nil
}
func (s *activityStubStore) ListActivity(context.Context, ActivityFilter) ([]*ActivityEvent, error) {
	return s.events, nil
}
