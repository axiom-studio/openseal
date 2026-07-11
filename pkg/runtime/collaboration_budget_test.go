package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestBudgetedDelegationRequiresNarrowExplicitAllocation(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "delegation-budget"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship and announce", Budget: &BudgetPolicy{MaxTurns: 10, MaxTotalTokens: 1000, MaxCostMicros: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	request := CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "Prepare launch copy",
		IdempotencyKey: "launch-copy",
	}
	if _, err := service.CreateAgentRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "explicit child budget") {
		t.Fatalf("missing allocation error = %v", err)
	}
	request.BudgetAllocation = &BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 400, MaxCostMicros: 300_000}
	created, err := service.CreateAgentRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Budget == nil || accepted.Child.Budget.MaxTurns != 4 ||
		accepted.Child.Budget.MaxTotalTokens != 400 || accepted.Child.BudgetUsage != (BudgetUsage{}) ||
		accepted.Child.BudgetState != BudgetStateActive {
		t.Fatalf("child budget = %#v", accepted.Child)
	}
	persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.BudgetAllocations[created.Request.ID].MaxTurns != 4 {
		t.Fatalf("source allocation = %#v, %v", persistedSource, err)
	}
	request.IdempotencyKey = "second-launch-copy"
	request.Recipient.ID = "marketing-two"
	request.BudgetAllocation = &BudgetPolicy{MaxTurns: 7, MaxTotalTokens: 300, MaxCostMicros: 200_000}
	if _, err := service.CreateAgentRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "exceeds parent remaining capacity") {
		t.Fatalf("sequential over-allocation error = %v", err)
	}
}

func TestGroupedDelegationCannotOverAllocateParentBudget(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "group-budget"}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Research", Budget: &BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewCollaborationService(store).CreateAgentRequestGroup(ctx, CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
		Policy:    RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Requests: []AgentRequestGroupSpec{
			{DependencyID: "one", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "one"}, Goal: "One", BudgetAllocation: &BudgetPolicy{MaxTurns: 6}},
			{DependencyID: "two", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "two"}, Goal: "Two", BudgetAllocation: &BudgetPolicy{MaxTurns: 5}},
		},
		IdempotencyKey: "overallocated-group",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds parent remaining capacity") {
		t.Fatalf("group allocation error = %v", err)
	}
}
