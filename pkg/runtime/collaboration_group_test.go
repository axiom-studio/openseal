package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestGroupedAgentRequestsJoinBeforeWakingAcrossPortableStores(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (models.CollaborationKernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.CollaborationKernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "grouped-collaboration.db"))
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
			scope := models.Scope{Kind: "tenant", ID: "research"}
			source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
				Goal: "Synthesize the market report", Source: models.RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			service := collaboration.NewCollaborationService(store)
			create := collaboration.CreateAgentRequestGroupRequest{
				Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
				Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"},
				Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
				Requests: []collaboration.AgentRequestGroupSpec{
					{DependencyID: "forums", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "forum-researcher"}, Goal: "Analyze forums"},
					{DependencyID: "reviews", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "review-research"}, Goal: "Analyze product reviews"},
				},
				IdempotencyKey: "research-wave-1", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
			}
			created, err := service.CreateAgentRequestGroup(ctx, create)
			if err != nil || len(created.Requests) != 2 || created.Group.Source.Status != models.AgentRunStatusWaitingForDependency {
				t.Fatalf("created group = %#v, %v", created, err)
			}
			replayed, err := service.CreateAgentRequestGroup(ctx, create)
			if err != nil || !replayed.Group.Replayed || len(replayed.Requests) != 2 || len(replayed.Events) != 0 {
				t.Fatalf("replayed group = %#v, %v", replayed, err)
			}

			accepted := make([]*collaboration.AgentRequestResult, 0, 2)
			for _, request := range created.Requests {
				result, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
					Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
					Decision: collaboration.AgentRequestDecisionAccept, Principal: request.Recipient,
				})
				if err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, result)
			}
			waiting, err := portfolio.NewPortfolioService(store).GetAgentRun(ctx, scope, source.ID)
			if err != nil || waiting.Status != models.AgentRunStatusWaitingForDependency || waiting.WakeCondition == nil || waiting.WakeCondition.Reference != created.Group.Group.ID {
				t.Fatalf("source after accepts = %#v, %v", waiting, err)
			}

			first, err := service.CompleteAgentRequest(ctx, collaboration.CompleteAgentRequestRequest{
				Scope: scope, RequestID: accepted[0].Request.ID, ExpectedRevision: accepted[0].Request.Revision,
				ExpectedChildRevision: accepted[0].Child.Revision, Principal: accepted[0].Request.Recipient,
				Summary: "Forum findings delivered", CompletionKey: "forums-complete-v1",
			})
			if err != nil || first.Source.Status != models.AgentRunStatusWaitingForDependency {
				t.Fatalf("first completion = %#v, %v", first, err)
			}
			second, err := service.CompleteAgentRequest(ctx, collaboration.CompleteAgentRequestRequest{
				Scope: scope, RequestID: accepted[1].Request.ID, ExpectedRevision: accepted[1].Request.Revision,
				ExpectedChildRevision: accepted[1].Child.Revision, Principal: accepted[1].Request.Recipient,
				Actor:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer"},
				Summary: "Review findings delivered", CompletionKey: "reviews-complete-v1",
			})
			if err != nil || second.Source.Status != models.AgentRunStatusQueued || second.Source.WakeCondition != nil {
				t.Fatalf("second completion = %#v, %v", second, err)
			}
			group, err := portfolio.NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
			if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.WakeSignalID == "" {
				t.Fatalf("resolved group = %#v, %v", group, err)
			}
			edges, err := portfolio.NewDependencyCoordinator(store).ListRunDependencies(ctx, scope, group.ID)
			if err != nil || len(edges) != 2 || edges[0].State != models.RunDependencyStateSatisfied || edges[1].State != models.RunDependencyStateSatisfied {
				t.Fatalf("resolved edges = %#v, %v", edges, err)
			}
		})
	}
}

func TestGroupedAgentRequestsRecordLosingCompletionWithoutSecondWake(t *testing.T) {
	t.Parallel()
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "any-fan-in"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Use the first valid answer", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	created, err := service.CreateAgentRequestGroup(ctx, collaboration.CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"},
		Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait},
		Requests: []collaboration.AgentRequestGroupSpec{
			{DependencyID: "a", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "a"}, Goal: "Answer A"},
			{DependencyID: "b", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "b"}, Goal: "Answer B"},
		},
		IdempotencyKey: "any-v1", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make([]*collaboration.AgentRequestResult, len(created.Requests))
	for index, request := range created.Requests {
		result, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
			Decision: collaboration.AgentRequestDecisionAccept, Principal: request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		accepted[index] = result
	}
	for index, request := range accepted {
		result, err := service.CompleteAgentRequest(ctx, collaboration.CompleteAgentRequestRequest{
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
	group, err := portfolio.NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.Revision != 3 {
		t.Fatalf("group after losing completion = %#v, %v", group, err)
	}
}

func TestGroupedAgentRequestRejectionFailsRequiredFanIn(t *testing.T) {
	t.Parallel()
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "reject-fan-in"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Collect required reviews", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	created, err := service.CreateAgentRequestGroup(ctx, collaboration.CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"},
		Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Requests: []collaboration.AgentRequestGroupSpec{
			{DependencyID: "required", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewers"}, Goal: "Review"},
		},
		IdempotencyKey: "reject-v1", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Requests[0].ID, ExpectedRevision: created.Requests[0].Revision,
		Decision: collaboration.AgentRequestDecisionReject, Principal: created.Requests[0].Recipient, Message: "No reviewer is available",
	})
	if err != nil || rejected.Request.Status != models.AgentRequestStatusRejected || rejected.Source.Status != models.AgentRunStatusQueued {
		t.Fatalf("rejected request = %#v, %v", rejected, err)
	}
	group, err := portfolio.NewDependencyCoordinator(store).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != models.RunDependencyGroupFailed || group.WakeSignalID == "" {
		t.Fatalf("failed group = %#v, %v", group, err)
	}
}
