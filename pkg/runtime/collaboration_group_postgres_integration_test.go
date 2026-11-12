//go:build integration

package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestPostgresGroupedAgentRequestsResolveAtomicallyAcrossReplicas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := kerneltest.OpenPostgres(t, "openseal_grouped_collaboration_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	scope := models.Scope{Kind: "tenant", ID: "market-research"}
	source, err := portfolio.NewPortfolioService(primary).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
		Goal: "Synthesize competitor pain points", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := collaboration.CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"},
		Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Requests: []collaboration.AgentRequestGroupSpec{
			{DependencyID: "reddit", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reddit-researcher"}, Goal: "Analyze Reddit"},
			{DependencyID: "forums", Kind: models.AgentRequestKindRequest, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "forum-research"}, Goal: "Analyze forums"},
		},
		IdempotencyKey: "competitor-wave-1", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	}
	created, err := collaboration.NewCollaborationService(primary).CreateAgentRequestGroup(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{primary, replica}
	accepted := make([]*collaboration.AgentRequestResult, len(created.Requests))
	for index, request := range created.Requests {
		result, err := collaboration.NewCollaborationService(stores[index%len(stores)]).RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
			Decision: collaboration.AgentRequestDecisionAccept, Principal: request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		accepted[index] = result
	}

	var failures atomic.Int32
	var wakes atomic.Int32
	var wait sync.WaitGroup
	for index := range accepted {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := collaboration.NewCollaborationService(stores[index%len(stores)]).CompleteAgentRequest(ctx, collaboration.CompleteAgentRequestRequest{
				Scope: scope, RequestID: accepted[index].Request.ID, ExpectedRevision: accepted[index].Request.Revision,
				ExpectedChildRevision: accepted[index].Child.Revision, Principal: accepted[index].Request.Recipient,
				Summary: "Findings delivered", CompletionKey: "complete-" + accepted[index].Request.DependencyID,
			})
			if err != nil {
				failures.Add(1)
				return
			}
			for _, event := range result.Events {
				if event.EventType == "dependency.group_satisfied" {
					wakes.Add(1)
				}
			}
		}()
	}
	wait.Wait()
	if failures.Load() != 0 || wakes.Load() != 1 {
		t.Fatalf("completion failures = %d, observed wakes = %d", failures.Load(), wakes.Load())
	}

	restarted, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	group, err := portfolio.NewDependencyCoordinator(restarted).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.WakeSignalID == "" {
		t.Fatalf("restarted group = %#v, %v", group, err)
	}
	persistedSource, err := portfolio.NewPortfolioService(restarted).GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != models.AgentRunStatusQueued || persistedSource.LastWakeSignalID != group.WakeSignalID {
		t.Fatalf("restarted source = %#v, %v", persistedSource, err)
	}
	replayed, err := collaboration.NewCollaborationService(restarted).CreateAgentRequestGroup(ctx, create)
	if err != nil || !replayed.Group.Replayed || len(replayed.Events) != 0 {
		t.Fatalf("replayed group = %#v, %v", replayed, err)
	}
}

func TestPostgresTeamWorkBidsUseCASAcrossReplicasAndSurviveRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := kerneltest.OpenPostgres(t, "openseal_team_bids_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scopeRef := capability.ScopeReference{Kind: "tenant", ID: "coordination"}
	scope := models.Scope{Kind: scopeRef.Kind, ID: scopeRef.ID}
	agents := kernelagent.NewRegistryWithStore(primary)
	definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review work", SystemPrompt: "Review work.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"reviewer-a", "reviewer-b"} {
		if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: id, Scope: scopeRef, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
		}, "user", "operator", "Team bidding test"); err != nil {
			t.Fatal(err)
		}
	}
	teams := kernelteam.NewRegistryWithStore(primary, agents)
	teamDefinition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "review-team", Version: "1", DisplayName: "Review Team", Purpose: "Review work",
		Delegation: kernelteam.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 2, RequireAcceptance: true},
		Approvals:  kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "review-team", Scope: scopeRef, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: kernelteam.DeploymentActive, Roster: []kernelteam.RosterAssignment{
			{ID: "a", AgentDeploymentID: "reviewer-a"},
			{ID: "b", AgentDeploymentID: "reviewer-b"},
		},
	}, "user", "operator", "Team bidding test"); err != nil {
		t.Fatal(err)
	}
	source, err := portfolio.NewPortfolioService(primary).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Coordinate review", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := collaboration.NewCollaborationService(primary).CreateAgentRequest(ctx, collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"}, Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "review-team"},
		Goal: "Review evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{primary, replica}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for index, agentID := range []string{"reviewer-a", "reviewer-b"} {
		wait.Add(1)
		go func(index int, agentID string) {
			defer wait.Done()
			_, bidErr := collaboration.NewCollaborationService(stores[index]).SubmitAgentRequestBid(ctx, collaboration.SubmitAgentRequestBidRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
				AgentDeploymentID: agentID, Decision: models.AgentRequestBidAccept, IdempotencyKey: agentID + "-accept",
			})
			if bidErr == nil {
				successes.Add(1)
			} else if !errors.Is(bidErr, models.ErrRevisionConflict) {
				t.Errorf("unexpected bid error: %v", bidErr)
			}
		}(index, agentID)
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent successful bids = %d, want 1", successes.Load())
	}
	restarted, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, err := collaboration.NewCollaborationService(restarted).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || len(persisted.Bids) != 1 || persisted.Revision != 2 {
		t.Fatalf("restarted bid = %#v, %v", persisted, err)
	}
}
