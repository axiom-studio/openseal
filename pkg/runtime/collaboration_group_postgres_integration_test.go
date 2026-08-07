//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

func TestPostgresGroupedAgentRequestsResolveAtomicallyAcrossReplicas(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_grouped_collaboration_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	scope := Scope{Kind: "tenant", ID: "market-research"}
	source, err := NewPortfolioService(primary).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
		Goal: "Synthesize competitor pain points", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
		Policy:    RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Requests: []AgentRequestGroupSpec{
			{DependencyID: "reddit", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "reddit-researcher"}, Goal: "Analyze Reddit"},
			{DependencyID: "forums", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "forum-research"}, Goal: "Analyze forums"},
		},
		IdempotencyKey: "competitor-wave-1", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	}
	created, err := NewCollaborationService(primary).CreateAgentRequestGroup(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*PostgresStore{primary, replica}
	accepted := make([]*AgentRequestResult, len(created.Requests))
	for index, request := range created.Requests {
		result, err := NewCollaborationService(stores[index%len(stores)]).RespondAgentRequest(ctx, RespondAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
			Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
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
			result, err := NewCollaborationService(stores[index%len(stores)]).CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
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

	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	group, err := NewDependencyCoordinator(restarted).GetRunDependencyGroup(ctx, scope, created.Group.Group.ID)
	if err != nil || group.Status != RunDependencyGroupSatisfied || group.WakeSignalID == "" {
		t.Fatalf("restarted group = %#v, %v", group, err)
	}
	persistedSource, err := NewPortfolioService(restarted).GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != AgentRunStatusQueued || persistedSource.LastWakeSignalID != group.WakeSignalID {
		t.Fatalf("restarted source = %#v, %v", persistedSource, err)
	}
	replayed, err := NewCollaborationService(restarted).CreateAgentRequestGroup(ctx, create)
	if err != nil || !replayed.Group.Replayed || len(replayed.Events) != 0 {
		t.Fatalf("replayed group = %#v, %v", replayed, err)
	}
}

func TestPostgresTeamWorkBidsUseCASAcrossReplicasAndSurviveRestart(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_team_bids_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scopeRef := capability.ScopeReference{Kind: "tenant", ID: "coordination"}
	scope := Scope{Kind: scopeRef.Kind, ID: scopeRef.ID}
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
		Roles:        []kernelteam.RoleSlot{{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review", RequiredDefinitionIDs: []string{"reviewer"}}},
		Coordination: kernelteam.CoordinationPolicy{MaximumSpeakersPerRound: 1, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:   kernelteam.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 2, RequireAcceptance: true},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "review-team", Scope: scopeRef, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: kernelteam.DeploymentActive, Roster: []kernelteam.RosterAssignment{
			{ID: "a", RoleID: "reviewer", AgentDeploymentID: "reviewer-a"},
			{ID: "b", RoleID: "reviewer", AgentDeploymentID: "reviewer-b"},
		},
	}, "user", "operator", "Team bidding test"); err != nil {
		t.Fatal(err)
	}
	source, err := NewPortfolioService(primary).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Coordinate review", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewCollaborationService(primary).CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"}, Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "review-team"},
		SemanticRole: "reviewer", Goal: "Review evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*PostgresStore{primary, replica}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for index, agentID := range []string{"reviewer-a", "reviewer-b"} {
		wait.Add(1)
		go func(index int, agentID string) {
			defer wait.Done()
			_, bidErr := NewCollaborationService(stores[index]).SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
				AgentDeploymentID: agentID, Decision: AgentRequestBidAccept, IdempotencyKey: agentID + "-accept",
			})
			if bidErr == nil {
				successes.Add(1)
			} else if !errors.Is(bidErr, ErrRevisionConflict) {
				t.Errorf("unexpected bid error: %v", bidErr)
			}
		}(index, agentID)
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent successful bids = %d, want 1", successes.Load())
	}
	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, err := NewCollaborationService(restarted).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || len(persisted.Bids) != 1 || persisted.Revision != 2 {
		t.Fatalf("restarted bid = %#v, %v", persisted, err)
	}
}
