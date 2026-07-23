package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGroupedAgentRequestsJoinBeforeWakingAcrossPortableStores(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (CollaborationKernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (CollaborationKernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (CollaborationKernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "grouped-collaboration.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "research"}
			source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
				Goal: "Synthesize the market report", Source: RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			service := NewCollaborationService(store)
			create := CreateAgentRequestGroupRequest{
				Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
				Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
				Policy:    RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
				Requests: []AgentRequestGroupSpec{
					{DependencyID: "forums", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "forum-researcher"}, Goal: "Analyze forums"},
					{DependencyID: "reviews", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "review-research"}, Goal: "Analyze product reviews"},
				},
				IdempotencyKey: "research-wave-1", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
			}
			created, err := service.CreateAgentRequestGroup(ctx, create)
			if err != nil || len(created.Requests) != 2 || created.Group.Source.Status != AgentRunStatusWaitingForDependency {
				t.Fatalf("created group = %#v, %v", created, err)
			}
			replayed, err := service.CreateAgentRequestGroup(ctx, create)
			if err != nil || !replayed.Group.Replayed || len(replayed.Requests) != 2 || len(replayed.Events) != 0 {
				t.Fatalf("replayed group = %#v, %v", replayed, err)
			}

			accepted := make([]*AgentRequestResult, 0, 2)
			for _, request := range created.Requests {
				result, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
					Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
					Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
				})
				if err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, result)
			}
			waiting, err := NewPortfolioService(store).GetAgentRun(ctx, scope, source.ID)
			if err != nil || waiting.Status != AgentRunStatusWaitingForDependency || waiting.WakeCondition == nil || waiting.WakeCondition.Reference != created.Group.Group.ID {
				t.Fatalf("source after accepts = %#v, %v", waiting, err)
			}

			first, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
				Scope: scope, RequestID: accepted[0].Request.ID, ExpectedRevision: accepted[0].Request.Revision,
				ExpectedChildRevision: accepted[0].Child.Revision, Principal: accepted[0].Request.Recipient,
				Summary: "Forum findings delivered", CompletionKey: "forums-complete-v1",
			})
			if err != nil || first.Source.Status != AgentRunStatusWaitingForDependency {
				t.Fatalf("first completion = %#v, %v", first, err)
			}
			second, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
				Scope: scope, RequestID: accepted[1].Request.ID, ExpectedRevision: accepted[1].Request.Revision,
				ExpectedChildRevision: accepted[1].Child.Revision, Principal: accepted[1].Request.Recipient,
				Actor:   CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer"},
				Summary: "Review findings delivered", CompletionKey: "reviews-complete-v1",
			})
			if err != nil || second.Source.Status != AgentRunStatusQueued || second.Source.WakeCondition != nil {
				t.Fatalf("second completion = %#v, %v", second, err)
			}
			group, err := NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
			if err != nil || group.Status != RunDependencyGroupSatisfied || group.WakeSignalID == "" {
				t.Fatalf("resolved group = %#v, %v", group, err)
			}
			edges, err := NewDependencyCoordinator(store).ListRunDependencies(ctx, scope, group.ID)
			if err != nil || len(edges) != 2 || edges[0].State != RunDependencyStateSatisfied || edges[1].State != RunDependencyStateSatisfied {
				t.Fatalf("resolved edges = %#v, %v", edges, err)
			}
		})
	}
}

func TestGroupedAgentRequestsRecordLosingCompletionWithoutSecondWake(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "any-fan-in"}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Use the first valid answer", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequestGroup(ctx, CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
		Policy:    RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait},
		Requests: []AgentRequestGroupSpec{
			{DependencyID: "a", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "a"}, Goal: "Answer A"},
			{DependencyID: "b", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "b"}, Goal: "Answer B"},
		},
		IdempotencyKey: "any-v1", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make([]*AgentRequestResult, len(created.Requests))
	for index, request := range created.Requests {
		result, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
			Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		accepted[index] = result
	}
	for index, request := range accepted {
		result, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
			Scope: scope, RequestID: request.Request.ID, ExpectedRevision: request.Request.Revision,
			ExpectedChildRevision: request.Child.Revision, Principal: request.Request.Recipient,
			Summary: "Answer delivered", CompletionKey: "answer-" + request.Request.DependencyID,
		})
		if err != nil {
			t.Fatal(err)
		}
		wakeEvents := 0
		for _, event := range result.Events {
			if event.EventType == "dependency.group_satisfied" {
				wakeEvents++
			}
		}
		if (index == 0 && wakeEvents != 1) || (index == 1 && wakeEvents != 0) {
			t.Fatalf("completion %d wake events = %d", index, wakeEvents)
		}
	}
	group, err := NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != RunDependencyGroupSatisfied || group.Revision != 3 {
		t.Fatalf("group after losing completion = %#v, %v", group, err)
	}
}

func TestGroupedAgentRequestRejectionFailsRequiredFanIn(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "reject-fan-in"}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Collect required reviews", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequestGroup(ctx, CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
		Policy:    RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Requests: []AgentRequestGroupSpec{
			{DependencyID: "required", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewers"}, Goal: "Review"},
		},
		IdempotencyKey: "reject-v1", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Requests[0].ID, ExpectedRevision: created.Requests[0].Revision,
		Decision: AgentRequestDecisionReject, Principal: created.Requests[0].Recipient, Message: "No reviewer is available",
	})
	if err != nil || rejected.Request.Status != AgentRequestStatusRejected || rejected.Source.Status != AgentRunStatusQueued {
		t.Fatalf("rejected request = %#v, %v", rejected, err)
	}
	group, err := NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != RunDependencyGroupFailed || group.WakeSignalID == "" {
		t.Fatalf("failed group = %#v, %v", group, err)
	}
}
