package collaboration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type collaborationTeamStoreStub struct {
	deployment *kernelteam.Deployment
	definition *kernelteam.Definition
}

type collaborationAgentCatalogStore struct {
	*sqlite.Store
	deployments map[string]*kernelagent.AgentDeployment
	definitions map[string]*kernelagent.AgentDefinition
}

func (s *collaborationAgentCatalogStore) GetDeployment(_ context.Context, _ capability.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	deployment := s.deployments[id]
	if deployment == nil {
		return nil, kernelagent.ErrDeploymentNotFound
	}
	return deployment, nil
}

func (s *collaborationAgentCatalogStore) GetDefinition(_ context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	definition := s.definitions[id+"@"+version]
	if definition == nil {
		return nil, kernelagent.ErrDefinitionNotFound
	}
	return definition, nil
}

func (s collaborationTeamStoreStub) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if s.deployment == nil || s.deployment.Scope != scope || s.deployment.ID != id {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return s.deployment, nil
}

func (s collaborationTeamStoreStub) GetTeamDefinition(_ context.Context, id, version string) (*kernelteam.Definition, error) {
	if s.definition == nil || s.definition.ID != id || s.definition.Version != version {
		return nil, kernelteam.ErrDefinitionNotFound
	}
	return s.definition, nil
}

func activeCollaborationTeam(scope models.Scope, id string, assignments ...kernelteam.RosterAssignment) collaborationTeamStoreStub {
	return collaborationTeamStoreStub{deployment: &kernelteam.Deployment{
		ID: id, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
		Roster: assignments, Revision: 1,
	}}
}

func TestCollaborationSnapshotsTeamDelegationPolicyAndRequiresAcceptance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	store := conversationtest.NewStore(t)
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: "developer",
		Goal: "Coordinate the release", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "delivery-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
			DefinitionID: "delivery", ActiveVersion: "3", Revision: 1,
		},
		definition: &kernelteam.Definition{
			ID: "delivery", Version: "3", Delegation: kernelteam.DelegationPolicy{
				MaximumDepth: 3, MaximumConcurrent: 2, AllowPeerDelegation: true,
				RequireAcceptance: true, RequireCompletionReview: true,
			},
		},
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer"}, SourceRunID: source.ID,
		Goal: "Review the release", AcceptancePolicy: models.AgentRequestAcceptancePreauthorized, IdempotencyKey: "review-release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Request.AcceptancePolicy != models.AgentRequestAcceptanceRecipientReview {
		t.Fatalf("acceptance policy = %q", created.Request.AcceptancePolicy)
	}
	policy := created.Request.DelegationPolicy
	if policy == nil || policy.TeamDeploymentID != "delivery-team" || policy.TeamDefinitionID != "delivery" || policy.DelegationDepth != 1 ||
		policy.TeamDefinitionVersion != "3" || policy.MaximumDepth != 3 || policy.MaximumConcurrent != 2 ||
		!policy.AllowPeerDelegation || !policy.RequireAcceptance || !policy.RequireCompletionReview {
		t.Fatalf("delegation policy snapshot = %#v", policy)
	}
	replayed, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer"}, SourceRunID: source.ID,
		Goal: "Review the release", AcceptancePolicy: models.AgentRequestAcceptancePreauthorized, IdempotencyKey: "review-release",
	})
	if err != nil || replayed.Request.ID != created.Request.ID {
		t.Fatalf("policy-governed replay = %#v, %v", replayed, err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Release review completed", CompletionKey: "completion-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Request.Status != models.AgentRequestStatusCompletionReview || submitted.Request.CompletionSubmittedAt == nil ||
		submitted.Source.Status != models.AgentRunStatusWaitingForDependency || submitted.Child.Status != models.AgentRunStatusCompleted {
		t.Fatalf("completion review checkpoint = %#v / %#v / %#v", submitted.Request, submitted.Source, submitted.Child)
	}
	if _, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: submitted.Request.ID, ExpectedRevision: submitted.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Recipient,
		Approve: true, Summary: "self review", IdempotencyKey: "review-self",
	}); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("recipient self-review error = %v", err)
	}
	reviewed, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: submitted.Request.ID, ExpectedRevision: submitted.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Approve: true, Summary: "Independent review passed", IdempotencyKey: "review-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Request.Status != models.AgentRequestStatusCompleted || reviewed.Request.ReviewedAt == nil || reviewed.Request.CompletedAt == nil ||
		reviewed.Source.Status != models.AgentRunStatusQueued || reviewed.Request.CompletionReviewer == nil || *reviewed.Request.CompletionReviewer != submitted.Request.Requester {
		t.Fatalf("reviewed completion = %#v / %#v", reviewed.Request, reviewed.Source)
	}
}

func TestTeamCompletionReviewQuorumEscalatesAndResolvesDisagreement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "review-quorum"}
	store := conversationtest.NewStore(t)
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: "lead",
		Goal: "Coordinate reviewed delivery", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "delivery-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
			DefinitionID: "delivery", ActiveVersion: "1", Revision: 1,
		},
		definition: &kernelteam.Definition{ID: "delivery", Version: "1", Delegation: kernelteam.DelegationPolicy{
			MaximumDepth: 2, MaximumConcurrent: 2, AllowPeerDelegation: true, RequireAcceptance: true,
			RequireCompletionReview: true, CompletionReviewQuorum: 2, EscalateOnDisagreement: true,
		}},
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "delivery-team"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "worker"}, SourceRunID: source.ID, Goal: "Produce reviewed evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Evidence ready", CompletionKey: "reviewed-evidence-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: submitted.Request.ID, ExpectedRevision: submitted.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer-a"}, Approve: true,
		Summary: "Evidence is complete", IdempotencyKey: "review-a",
	})
	if err != nil || first.Request.Status != models.AgentRequestStatusCompletionReview || len(first.Request.CompletionReviews) != 1 {
		t.Fatalf("first quorum decision = %#v, %v", first, err)
	}
	second, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: first.Request.ID, ExpectedRevision: first.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer-b"}, Approve: false,
		Summary: "Evidence misses one criterion", IdempotencyKey: "review-b",
	})
	if err != nil || !second.Request.ReviewDisagreement || second.Request.Status != models.AgentRequestStatusCompletionReview {
		t.Fatalf("disagreement = %#v, %v", second, err)
	}
	resolved, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: second.Request.ID, ExpectedRevision: second.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer-c"}, Approve: true, ResolveDisagreement: true,
		Summary: "Escalation review confirms the evidence", IdempotencyKey: "review-c",
	})
	if err != nil || resolved.Request.Status != models.AgentRequestStatusCompleted || resolved.Request.ReviewDisagreement ||
		len(resolved.Request.CompletionReviews) != 3 || resolved.Source.Status != models.AgentRunStatusQueued {
		t.Fatalf("resolved disagreement = %#v, %v", resolved, err)
	}
}

func TestCollaborationEnforcesSnapshottedTeamDelegationLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	newService := func(policy kernelteam.DelegationPolicy) (*sqlite.Store, *portfolio.PortfolioService, *CollaborationService) {
		store := conversationtest.NewStore(t)
		portfolio := portfolio.NewPortfolioService(store)
		service := NewCollaborationService(store)
		service.teams = collaborationTeamStoreStub{
			deployment: &kernelteam.Deployment{
				ID: "delivery-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
				DefinitionID: "delivery", ActiveVersion: "3", Revision: 1,
			},
			definition: &kernelteam.Definition{ID: "delivery", Version: "3", Delegation: policy},
		}
		return store, portfolio, service
	}
	createSource := func(t *testing.T, portfolio *portfolio.PortfolioService, assigned, key string, parent *models.AgentRun) *models.AgentRun {
		t.Helper()
		request := models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: assigned,
			Goal: "Coordinate delivery", Source: models.RunSourceManual, IdempotencyKey: key,
		}
		if parent != nil {
			request.ParentRunID = parent.ID
			request.Context = map[string]interface{}{"collaboration": map[string]interface{}{"requestId": "request-parent"}}
		}
		run, err := portfolio.CreateAgentRun(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	request := func(source *models.AgentRun, recipient, key string) CreateAgentRequestRequest {
		return CreateAgentRequestRequest{
			Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: source.AssignedAgentID},
			Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: recipient}, SourceRunID: source.ID,
			Goal: "Review delivery", IdempotencyKey: key,
		}
	}

	t.Run("peer delegation", func(t *testing.T) {
		_, portfolio, service := newService(kernelteam.DelegationPolicy{MaximumDepth: 3, MaximumConcurrent: 3})
		source := createSource(t, portfolio, "developer", "peer-source", nil)
		if _, err := service.CreateAgentRequest(ctx, request(source, "reviewer", "peer-request")); !errors.Is(err, ErrAgentRequestUnauthorized) {
			t.Fatalf("peer delegation error = %v", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		_, portfolio, service := newService(kernelteam.DelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 3, AllowPeerDelegation: true})
		parent := createSource(t, portfolio, "lead", "depth-parent", nil)
		child := createSource(t, portfolio, "developer", "depth-child", parent)
		if _, err := service.CreateAgentRequest(ctx, request(child, "reviewer", "depth-request")); !errors.Is(err, models.ErrAgentRequestAssignment) || !strings.Contains(err.Error(), "depth 2") {
			t.Fatalf("depth error = %v", err)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		_, portfolio, service := newService(kernelteam.DelegationPolicy{MaximumDepth: 3, MaximumConcurrent: 1, AllowPeerDelegation: true})
		first := createSource(t, portfolio, "developer", "concurrent-source-1", nil)
		if _, err := service.CreateAgentRequest(ctx, request(first, "reviewer", "concurrent-request-1")); err != nil {
			t.Fatal(err)
		}
		second := createSource(t, portfolio, "developer", "concurrent-source-2", nil)
		if _, err := service.CreateAgentRequest(ctx, request(second, "reviewer", "concurrent-request-2")); !errors.Is(err, models.ErrAgentRequestAssignment) || !strings.Contains(err.Error(), "reaching maximum 1") {
			t.Fatalf("concurrency error = %v", err)
		}
	})
}

func TestTeamWorkOfferBidsSelectDeterministicallyAndSurviveRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "team-offers.db")
	store, err := sqlite.New(database)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "team-offers"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
		Goal: "Coordinate release analysis", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	teamStore := activeCollaborationTeam(scope, "reviewers",
		kernelteam.RosterAssignment{ID: "z", AgentDeploymentID: "z-agent"},
		kernelteam.RosterAssignment{ID: "a", AgentDeploymentID: "a-agent"},
		kernelteam.RosterAssignment{ID: "m", AgentDeploymentID: "m-agent"})
	service := NewCollaborationService(store)
	service.teams = teamStore
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "coordinator"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "reviewers"},
		Goal:      "Review release evidence", IdempotencyKey: "release-review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(created.Request.Candidates); got != 3 {
		t.Fatalf("candidate count = %d, want 3: %#v", got, created.Request.Candidates)
	}
	bid, err := service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1, AgentDeploymentID: "z-agent",
		Decision: models.AgentRequestBidAccept, Reason: "available", IdempotencyKey: "z-accept",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision, AgentDeploymentID: "z-agent",
		Decision: models.AgentRequestBidAccept, Reason: "available", IdempotencyKey: "z-accept",
	})
	if err != nil || replayed.Request.Revision != bid.Request.Revision {
		t.Fatalf("idempotent bid = %#v, %v", replayed, err)
	}
	bid, err = service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision, AgentDeploymentID: "a-agent",
		Decision: models.AgentRequestBidAccept, Reason: "available", IdempotencyKey: "a-accept",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := sqlite.New(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedService := NewCollaborationService(restarted)
	restartedService.teams = teamStore
	accepted, err := restartedService.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "reviewers"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "a-agent" || accepted.Request.AssignmentDecision == nil ||
		!strings.Contains(accepted.Request.AssignmentDecision.Reason, "rank 1") || accepted.Child.AssignedAgentID != "a-agent" {
		t.Fatalf("deterministic assignment = %#v / %#v", accepted.Request, accepted.Child)
	}
}

func TestTeamWorkOfferFiltersMemberSkillsAndRecordsCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "team-authority"}
	kernel := conversationtest.NewStore(t)
	store := &collaborationAgentCatalogStore{
		Store: kernel,
		deployments: map[string]*kernelagent.AgentDeployment{
			"eligible": {ID: "eligible", DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2}},
			"busy":     {ID: "busy", DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}},
			"wrong":    {ID: "wrong", DefinitionID: "writer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 4}},
		},
		definitions: map[string]*kernelagent.AgentDefinition{
			"analyst@1": {ID: "analyst", Version: "1", Authority: kernelagent.AuthorityPolicy{AllowedSkillIDs: []string{"research"}}},
			"writer@1":  {ID: "writer", Version: "1", Authority: kernelagent.AuthorityPolicy{AllowedSkillIDs: []string{"writing"}}},
		},
	}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Coordinate analysis", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, agentID := range []string{"eligible", "busy"} {
		if _, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
			Goal: fmt.Sprintf("existing work %d", index), Source: models.RunSourceManual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewCollaborationService(store)
	service.teams = collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "analysis", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analysis", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Revision: 1, Roster: []kernelteam.RosterAssignment{
				{ID: "eligible", AgentDeploymentID: "eligible", RequiredSkillIDs: []string{"research"}},
				{ID: "busy", AgentDeploymentID: "busy", RequiredSkillIDs: []string{"research"}},
				{ID: "wrong", AgentDeploymentID: "wrong", RequiredSkillIDs: []string{"research"}},
			},
		},
		definition: &kernelteam.Definition{ID: "analysis", Version: "1"},
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"}, Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "analysis"},
		Goal: "Analyze evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Request.Candidates) != 2 || created.Request.Candidates[0].AgentDeploymentID != "busy" ||
		created.Request.Candidates[0].AvailableSlots != 0 || created.Request.Candidates[1].AgentDeploymentID != "eligible" ||
		created.Request.Candidates[1].AvailableSlots != 1 {
		t.Fatalf("authority and capacity candidates = %#v", created.Request.Candidates)
	}
	// A semantic role names one member by its roster id.
	second, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Coordinate more analysis", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	single, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: second.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "lead"}, Recipient: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "analysis"},
		SemanticRole: "eligible", Goal: "Analyze more evidence",
	})
	if err != nil || len(single.Request.Candidates) != 1 || single.Request.Candidates[0].RosterAssignmentID != "eligible" {
		t.Fatalf("member request = %#v, %v", single, err)
	}
	encoded, _ := json.Marshal(single.Request)
	if strings.Contains(string(encoded), "roleId") {
		t.Fatalf("request still carries a role id: %s", encoded)
	}
}
