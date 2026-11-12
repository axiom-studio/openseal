package portfolio_test

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"testing"
)

func TestPortfolioModelsRequireScopeAndOwner(t *testing.T) {
	service := portfolio.NewPortfolioService(&stubPortfolioStore{})
	_, err := service.CreateObjective(context.Background(), portfolio.CreateObjectiveRequest{Title: "Ship", Goal: "Ship product"})
	if !errors.Is(err, models.ErrInvalidScope) {
		t.Fatalf("error = %v, want ErrInvalidScope", err)
	}
	_, err = service.CreateObjective(context.Background(), portfolio.CreateObjectiveRequest{
		Scope: models.Scope{Kind: "tenant", ID: "a"}, Title: "Ship", Goal: "Ship product",
	})
	if !errors.Is(err, models.ErrInvalidOwner) {
		t.Fatalf("error = %v, want ErrInvalidOwner", err)
	}
}

type stubPortfolioStore struct{}

func (*stubPortfolioStore) CreateObjective(context.Context, *models.Objective) error { return nil }
func (*stubPortfolioStore) GetObjective(context.Context, models.Scope, string) (*models.Objective, error) {
	return nil, nil
}
func (*stubPortfolioStore) ListObjectives(context.Context, models.ObjectiveFilter) ([]*models.Objective, error) {
	return nil, nil
}
func (*stubPortfolioStore) UpdateObjective(context.Context, *models.Objective, int64) error {
	return nil
}
func (*stubPortfolioStore) CreateAgentRun(context.Context, *models.AgentRun) error { return nil }
func (*stubPortfolioStore) GetAgentRun(context.Context, models.Scope, string) (*models.AgentRun, error) {
	return nil, nil
}
func (*stubPortfolioStore) ListAgentRuns(context.Context, models.AgentRunFilter) ([]*models.AgentRun, error) {
	return nil, nil
}
