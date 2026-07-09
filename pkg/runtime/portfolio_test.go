package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestPortfolioModelsRequireScopeAndOwner(t *testing.T) {
	service := NewPortfolioService(&stubPortfolioStore{})
	_, err := service.CreateObjective(context.Background(), CreateObjectiveRequest{Title: "Ship", Goal: "Ship product"})
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("error = %v, want ErrInvalidScope", err)
	}
	_, err = service.CreateObjective(context.Background(), CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "a"}, Title: "Ship", Goal: "Ship product",
	})
	if !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("error = %v, want ErrInvalidOwner", err)
	}
}

type stubPortfolioStore struct{}

func (*stubPortfolioStore) CreateObjective(context.Context, *Objective) error { return nil }
func (*stubPortfolioStore) GetObjective(context.Context, Scope, string) (*Objective, error) {
	return nil, nil
}
func (*stubPortfolioStore) ListObjectives(context.Context, ObjectiveFilter) ([]*Objective, error) {
	return nil, nil
}
func (*stubPortfolioStore) UpdateObjective(context.Context, *Objective, int64) error { return nil }
func (*stubPortfolioStore) CreateAgentRun(context.Context, *AgentRun) error          { return nil }
func (*stubPortfolioStore) GetAgentRun(context.Context, Scope, string) (*AgentRun, error) {
	return nil, nil
}
func (*stubPortfolioStore) ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error) {
	return nil, nil
}
