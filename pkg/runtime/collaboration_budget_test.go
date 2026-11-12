package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestBudgetedDelegationRequiresNarrowExplicitAllocation(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "delegation-budget"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship and announce", Budget: &models.BudgetPolicy{MaxTurns: 10, MaxTotalTokens: 1000, MaxCostMicros: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	request := collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, Goal: "Prepare launch copy",
		IdempotencyKey: "launch-copy",
	}
	if _, err := service.CreateAgentRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "explicit child budget") {
		t.Fatalf("missing allocation error = %v", err)
	}
	request.BudgetAllocation = &models.BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 400, MaxCostMicros: 300_000}
	created, err := service.CreateAgentRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: collaboration.AgentRequestDecisionAccept, Principal: request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Budget == nil || accepted.Child.Budget.MaxTurns != 4 ||
		accepted.Child.Budget.MaxTotalTokens != 400 || accepted.Child.BudgetUsage != (models.BudgetUsage{}) ||
		accepted.Child.BudgetState != models.BudgetStateActive {
		t.Fatalf("child budget = %#v", accepted.Child)
	}
	persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.BudgetAllocations[created.Request.ID].MaxTurns != 4 {
		t.Fatalf("source allocation = %#v, %v", persistedSource, err)
	}
	request.IdempotencyKey = "second-launch-copy"
	request.Recipient.ID = "marketing-two"
	request.BudgetAllocation = &models.BudgetPolicy{MaxTurns: 7, MaxTotalTokens: 300, MaxCostMicros: 200_000}
	if _, err := service.CreateAgentRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "source run cannot start collaboration") {
		t.Fatalf("unsafe sequential fan-out error = %v", err)
	}
}

func TestGroupedDelegationCannotOverAllocateParentBudget(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "group-budget"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Research", Budget: &models.BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collaboration.NewCollaborationService(store).CreateAgentRequestGroup(ctx, collaboration.CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"},
		Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Requests: []collaboration.AgentRequestGroupSpec{
			{DependencyID: "one", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "one"}, Goal: "One", BudgetAllocation: &models.BudgetPolicy{MaxTurns: 6}},
			{DependencyID: "two", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "two"}, Goal: "Two", BudgetAllocation: &models.BudgetPolicy{MaxTurns: 5}},
		},
		IdempotencyKey: "overallocated-group",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds parent remaining capacity") {
		t.Fatalf("group allocation error = %v", err)
	}
}
