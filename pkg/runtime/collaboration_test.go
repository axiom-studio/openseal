package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type collaborationTeamStoreStub struct {
	deployment *kernelteam.Deployment
	definition *kernelteam.Definition
}

type collaborationAgentCatalogStore struct {
	*MemoryStore
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

func activeCollaborationTeam(scope Scope, id string, assignments ...kernelteam.RosterAssignment) collaborationTeamStoreStub {
	return collaborationTeamStoreStub{deployment: &kernelteam.Deployment{
		ID: id, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
		Roster: assignments, Revision: 1,
	}}
}

func TestCollaborationSnapshotsTeamDelegationPolicyAndRequiresAcceptance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	store := NewMemoryStore()
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: "developer",
		Goal: "Coordinate the release", Source: RunSourceManual,
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
		Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer"}, SourceRunID: source.ID,
		Goal: "Review the release", AcceptancePolicy: AgentRequestAcceptancePreauthorized, IdempotencyKey: "review-release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Request.AcceptancePolicy != AgentRequestAcceptanceRecipientReview {
		t.Fatalf("acceptance policy = %q", created.Request.AcceptancePolicy)
	}
	policy := created.Request.DelegationPolicy
	if policy == nil || policy.TeamDeploymentID != "delivery-team" || policy.TeamDefinitionID != "delivery" || policy.DelegationDepth != 1 ||
		policy.TeamDefinitionVersion != "3" || policy.MaximumDepth != 3 || policy.MaximumConcurrent != 2 ||
		!policy.AllowPeerDelegation || !policy.RequireAcceptance || !policy.RequireCompletionReview {
		t.Fatalf("delegation policy snapshot = %#v", policy)
	}
	replayed, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer"}, SourceRunID: source.ID,
		Goal: "Review the release", AcceptancePolicy: AgentRequestAcceptancePreauthorized, IdempotencyKey: "review-release",
	})
	if err != nil || replayed.Request.ID != created.Request.ID {
		t.Fatalf("policy-governed replay = %#v, %v", replayed, err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer"},
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
	if submitted.Request.Status != AgentRequestStatusCompletionReview || submitted.Request.CompletionSubmittedAt == nil ||
		submitted.Source.Status != AgentRunStatusWaitingForDependency || submitted.Child.Status != AgentRunStatusCompleted {
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
	if reviewed.Request.Status != AgentRequestStatusCompleted || reviewed.Request.ReviewedAt == nil || reviewed.Request.CompletedAt == nil ||
		reviewed.Source.Status != AgentRunStatusQueued || reviewed.Request.CompletionReviewer == nil || *reviewed.Request.CompletionReviewer != submitted.Request.Requester {
		t.Fatalf("reviewed completion = %#v / %#v", reviewed.Request, reviewed.Source)
	}
}

func TestTeamCompletionReviewQuorumEscalatesAndResolvesDisagreement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "review-quorum"}
	store := NewMemoryStore()
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: "lead",
		Goal: "Coordinate reviewed delivery", Source: RunSourceManual,
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
		Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeTeam, ID: "delivery-team"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "worker"}, SourceRunID: source.ID, Goal: "Produce reviewed evidence",
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
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer-a"}, Approve: true,
		Summary: "Evidence is complete", IdempotencyKey: "review-a",
	})
	if err != nil || first.Request.Status != AgentRequestStatusCompletionReview || len(first.Request.CompletionReviews) != 1 {
		t.Fatalf("first quorum decision = %#v, %v", first, err)
	}
	second, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: first.Request.ID, ExpectedRevision: first.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer-b"}, Approve: false,
		Summary: "Evidence misses one criterion", IdempotencyKey: "review-b",
	})
	if err != nil || !second.Request.ReviewDisagreement || second.Request.Status != AgentRequestStatusCompletionReview {
		t.Fatalf("disagreement = %#v, %v", second, err)
	}
	resolved, err := service.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: second.Request.ID, ExpectedRevision: second.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer-c"}, Approve: true, ResolveDisagreement: true,
		Summary: "Escalation review confirms the evidence", IdempotencyKey: "review-c",
	})
	if err != nil || resolved.Request.Status != AgentRequestStatusCompleted || resolved.Request.ReviewDisagreement ||
		len(resolved.Request.CompletionReviews) != 3 || resolved.Source.Status != AgentRunStatusQueued {
		t.Fatalf("resolved disagreement = %#v, %v", resolved, err)
	}
}

func TestCollaborationEnforcesSnapshottedTeamDelegationLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	newService := func(policy kernelteam.DelegationPolicy) (*MemoryStore, *PortfolioService, *CollaborationService) {
		store := NewMemoryStore()
		portfolio := NewPortfolioService(store)
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
	createSource := func(t *testing.T, portfolio *PortfolioService, assigned, key string, parent *AgentRun) *AgentRun {
		t.Helper()
		request := CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "delivery-team"}, AssignedAgentID: assigned,
			Goal: "Coordinate delivery", Source: RunSourceManual, IdempotencyKey: key,
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
	request := func(source *AgentRun, recipient, key string) CreateAgentRequestRequest {
		return CreateAgentRequestRequest{
			Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: source.AssignedAgentID},
			Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: recipient}, SourceRunID: source.ID,
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
		if _, err := service.CreateAgentRequest(ctx, request(child, "reviewer", "depth-request")); !errors.Is(err, ErrAgentRequestAssignment) || !strings.Contains(err.Error(), "depth 2") {
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
		if _, err := service.CreateAgentRequest(ctx, request(second, "reviewer", "concurrent-request-2")); !errors.Is(err, ErrAgentRequestAssignment) || !strings.Contains(err.Error(), "reaching maximum 1") {
			t.Fatalf("concurrency error = %v", err)
		}
	})
}

func TestCollaborationReservesTeamDelegationCapacityAtomically(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (CollaborationKernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (CollaborationKernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (CollaborationKernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "delegation-capacity.db"))
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
			scope := Scope{Kind: "tenant", ID: "atomic"}
			portfolio := NewPortfolioService(store)
			sources := make([]*AgentRun, 2)
			for index := range sources {
				var err error
				sources[index], err = portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
					Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, AssignedAgentID: "agent",
					Goal: "Coordinate", Source: RunSourceManual, IdempotencyKey: fmt.Sprintf("source-%d", index),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			teamStore := collaborationTeamStoreStub{
				deployment: &kernelteam.Deployment{
					ID: "team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
					DefinitionID: "team-definition", ActiveVersion: "1", Revision: 1,
				},
				definition: &kernelteam.Definition{ID: "team-definition", Version: "1", Delegation: kernelteam.DelegationPolicy{
					MaximumDepth: 2, MaximumConcurrent: 1, AllowPeerDelegation: true,
				}},
			}
			start := make(chan struct{})
			errorsByIndex := make([]error, len(sources))
			var group sync.WaitGroup
			for index, source := range sources {
				group.Add(1)
				go func(index int, source *AgentRun) {
					defer group.Done()
					<-start
					service := NewCollaborationService(store)
					service.teams = teamStore
					_, errorsByIndex[index] = service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
						Scope: scope, Kind: AgentRequestKindRequest,
						Requester:   CollaborationParty{Type: OwnerTypeAgent, ID: "agent"},
						Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: fmt.Sprintf("reviewer-%d", index)},
						SourceRunID: source.ID, Goal: "Review", IdempotencyKey: fmt.Sprintf("request-%d", index),
					})
				}(index, source)
			}
			close(start)
			group.Wait()
			succeeded, capacityRejected := 0, 0
			for _, err := range errorsByIndex {
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, ErrAgentRequestAssignment) && strings.Contains(err.Error(), "reaching maximum 1"):
					capacityRejected++
				default:
					t.Fatalf("unexpected concurrent creation error: %v", err)
				}
			}
			if succeeded != 1 || capacityRejected != 1 {
				t.Fatalf("concurrent results: success=%d capacity=%d errors=%v", succeeded, capacityRejected, errorsByIndex)
			}
		})
	}
}

func TestCollaborationCompletionReviewSurvivesPortableStoreRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "completion-review.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "review"}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, AssignedAgentID: "author",
		Goal: "Produce reviewed work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	teamStore := collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
			DefinitionID: "team", ActiveVersion: "1", Revision: 1,
		},
		definition: &kernelteam.Definition{ID: "team", Version: "1", Delegation: kernelteam.DelegationPolicy{
			MaximumDepth: 2, MaximumConcurrent: 2, AllowPeerDelegation: true, RequireAcceptance: true, RequireCompletionReview: true,
		}},
	}
	service := NewCollaborationService(store)
	service.teams = teamStore
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "author"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "specialist"}, SourceRunID: source.ID,
		Goal: "Produce result", IdempotencyKey: "portable-review",
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
		Summary: "Portable result", CompletionKey: "portable-completion",
	})
	if err != nil || submitted.Request.Status != AgentRequestStatusCompletionReview {
		t.Fatalf("submitted review = %#v, %v", submitted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	reviewed, err := NewCollaborationService(restarted).ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: submitted.Request.ID, ExpectedRevision: submitted.Request.Revision,
		ExpectedChildRevision: submitted.Child.Revision, Principal: submitted.Request.Requester,
		Approve: true, Summary: "Restarted reviewer approved", IdempotencyKey: "portable-review-decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Request.Status != AgentRequestStatusCompleted || reviewed.Source.Status != AgentRunStatusQueued {
		t.Fatalf("review after restart = %#v / %#v", reviewed.Request, reviewed.Source)
	}
}

func TestCollaborationRequestLifecycleAcrossPortableStores(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (CollaborationKernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (CollaborationKernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (CollaborationKernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "collaboration.db"))
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
			scope := Scope{Kind: "tenant", ID: "acme"}
			portfolio := NewPortfolioService(store)
			source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}, AssignedAgentID: "developer",
				Goal: "Ship the release", Source: RunSourceManual,
				Context: map[string]interface{}{"sourceOnly": true, "privateCredentialRef": "binding:must-not-transfer"},
				Budget:  &BudgetPolicy{MaxTotalTokens: 1000}, Policy: map[string]interface{}{"risk": "guarded"},
			})
			if err != nil {
				t.Fatal(err)
			}
			service := NewCollaborationService(store)
			create := CreateAgentRequestRequest{
				Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
				Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
				Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
				Goal:      "Turn the release into a launch brief", SemanticRole: "launch-editor",
				AcceptanceCriteria: map[string]interface{}{"format": "markdown"},
				ArtifactRequirements: []ArtifactRequirement{{
					Name: "launch-brief", Type: "document", Required: true,
					Schema: map[string]interface{}{
						"type": "object", "required": []interface{}{"wordCount"},
						"properties": map[string]interface{}{"wordCount": map[string]interface{}{"type": "integer", "minimum": 1}},
					},
				}},
				SharedContext:    map[string]interface{}{"release": "2026.07"},
				ChildCheckpoint:  map[string]interface{}{"phase": "draft", "audience": "platform-engineering"},
				ConversationRefs: []string{"team:gtm:42"},
				IdempotencyKey:   "release-launch-brief",
				BudgetAllocation: &BudgetPolicy{MaxTotalTokens: 500},
			}
			created, err := service.CreateAgentRequest(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if created.Request.Status != AgentRequestStatusPending || created.Source == nil ||
				created.Source.Status != AgentRunStatusWaitingForAgent || created.Source.WakeCondition == nil ||
				created.Source.WakeCondition.Type != "agent_request_decision" ||
				created.Source.WakeCondition.Reference != created.Request.ID ||
				len(created.Events) != 1 || created.Events[0].TeamID != "product" || created.Events[0].Visibility != ActivityVisibilityTeam {
				t.Fatalf("created request = %#v, events = %#v", created.Request, created.Events)
			}
			duplicate, err := service.CreateAgentRequest(ctx, create)
			if err != nil || duplicate.Request.ID != created.Request.ID || len(duplicate.Events) != 0 {
				t.Fatalf("idempotent request = %#v, %v", duplicate, err)
			}
			conflictingIntent := create
			conflictingIntent.SharedContext = map[string]interface{}{"release": "different"}
			if _, err := service.CreateAgentRequest(ctx, conflictingIntent); !errors.Is(err, ErrAgentRequestIdempotency) {
				t.Fatalf("conflicting request intent error = %v", err)
			}
			listed, err := service.ListAgentRequests(ctx, AgentRequestFilter{Scope: scope, Recipient: &create.Recipient})
			if err != nil || len(listed) != 1 || listed[0].ID != created.Request.ID {
				t.Fatalf("recipient requests = %#v, %v", listed, err)
			}

			question, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
				Decision: AgentRequestDecisionRequestClarification, Principal: create.Recipient,
				Message: "Which audience should this prioritize?",
			})
			if err != nil || question.Request.Status != AgentRequestStatusClarificationRequested || question.Child != nil {
				t.Fatalf("clarification request = %#v, %v", question, err)
			}
			clarified, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 2,
				Decision: AgentRequestDecisionProvideClarification, Principal: create.Requester,
				Message: "Prioritize existing platform engineering customers.",
			})
			if err != nil || clarified.Request.Status != AgentRequestStatusPending || clarified.Request.Revision != 3 {
				t.Fatalf("clarification response = %#v, %v", clarified, err)
			}
			accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 3,
				Decision: AgentRequestDecisionAccept, Principal: create.Recipient, Message: "I will draft and return the brief.",
			})
			if err != nil {
				t.Fatal(err)
			}
			if accepted.Request.Status != AgentRequestStatusAccepted || accepted.Child == nil || accepted.Child.ParentRunID != source.ID || accepted.Child.RootRunID != source.RootRunID {
				t.Fatalf("accepted request = %#v", accepted)
			}
			if accepted.Request.AcceptedAt == nil || accepted.Request.ResolvedAt != nil {
				t.Fatalf("accepted request timestamps = %#v", accepted.Request)
			}
			if accepted.Child.Owner != source.Owner || accepted.Child.AssignedAgentID != "marketing" || accepted.Child.Source != RunSourceRequest {
				t.Fatalf("child ownership = %#v", accepted.Child)
			}
			if accepted.Child.Context["sourceOnly"] != nil || accepted.Child.Context["privateCredentialRef"] != nil || accepted.Child.Context["release"] != "2026.07" {
				t.Fatalf("child context leaked source state: %#v", accepted.Child.Context)
			}
			if accepted.Child.Checkpoint["phase"] != "draft" || accepted.Child.Checkpoint["audience"] != "platform-engineering" {
				t.Fatalf("child checkpoint = %#v", accepted.Child.Checkpoint)
			}
			persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
			if err != nil || persistedSource.Status != AgentRunStatusWaitingForDependency || persistedSource.WakeCondition == nil || persistedSource.WakeCondition.Reference != created.Request.ID {
				t.Fatalf("source after acceptance = %#v, %v", persistedSource, err)
			}
			child, err := portfolio.GetAgentRun(ctx, scope, accepted.Child.ID)
			if err != nil || child.AssignedAgentID != "marketing" {
				t.Fatalf("persisted child = %#v, %v", child, err)
			}
			_, err = NewArtifactCatalog(store).Register(ctx, RegisterArtifactRequest{Artifact: &Artifact{
				ID: "artifact-launch-brief", Version: 1, Scope: scope, Name: "launch-brief.md",
				Type: "document", MediaType: "text/markdown", ContentRef: "artifact-store:launch-brief-v1",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 2048,
				Classification: ArtifactClassificationInternal, Metadata: map[string]interface{}{"wordCount": 420},
				Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "marketing"}, RunID: child.ID, RequestID: created.Request.ID},
				Evidence:   []EvidenceLink{{Relation: EvidenceRelationSupports, TargetKind: EvidenceTargetClaim, TargetRef: "review:editorial-7"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			completion := CompleteAgentRequestRequest{
				Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
				ExpectedChildRevision: child.Revision, Principal: create.Recipient,
				Actor:              CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
				Summary:            "Delivered the launch brief with audience-specific messaging.",
				AcceptanceEvidence: map[string]interface{}{"format": "markdown", "review": "passed"},
				Artifacts: []ArtifactReference{{
					ID: "artifact-launch-brief", Version: 1, RequirementName: "launch-brief",
				}},
				CompletionKey: "complete-launch-brief-v1",
			}
			completed, err := service.CompleteAgentRequest(ctx, completion)
			if err != nil {
				t.Fatal(err)
			}
			if completed.Request.Status != AgentRequestStatusCompleted || completed.Request.CompletedAt == nil || completed.Source.Status != AgentRunStatusQueued || completed.Child.Status != AgentRunStatusCompleted {
				t.Fatalf("completed request = %#v", completed)
			}
			if len(completed.Request.Artifacts) != 1 || completed.Request.Artifacts[0].ContentRef != "artifact-store:launch-brief-v1" || completed.Source.WakeCondition != nil {
				t.Fatalf("completion artifacts/source = %#v / %#v", completed.Request.Artifacts, completed.Source)
			}
			idempotentCompletion, err := service.CompleteAgentRequest(ctx, completion)
			if err != nil || idempotentCompletion.Request.Revision != completed.Request.Revision || len(idempotentCompletion.Events) != 0 {
				t.Fatalf("idempotent completion = %#v, %v", idempotentCompletion, err)
			}
			conflictingCompletion := completion
			conflictingCompletion.Summary = "A different result under the same key"
			if _, err := service.CompleteAgentRequest(ctx, conflictingCompletion); !errors.Is(err, ErrAgentRequestIdempotency) {
				t.Fatalf("conflicting completion error = %v", err)
			}
			activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 20})
			if err != nil || len(activity) != 7 || activity[0].EventType != "collaboration.completed" {
				t.Fatalf("team activity = %#v, %v", activity, err)
			}
		})
	}
}

func TestCollaborationHandoffTransfersOwnership(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship feature", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = activeCollaborationTeam(scope, "gtm",
		kernelteam.RosterAssignment{ID: "marketer", RoleID: "lead", AgentDeploymentID: "marketing-agent"},
		kernelteam.RosterAssignment{ID: "marketer-two", RoleID: "lead", AgentDeploymentID: "marketing-agent-two"})
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, Goal: "Own launch and lead follow-up", SemanticRole: "lead",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"},
	}); err == nil || !strings.Contains(err.Error(), "explicit assigned Agent") {
		t.Fatalf("missing Team assignment error = %v", err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marketing-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "marketing-agent" || accepted.Child.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}) || accepted.Child.AssignedAgentID != "marketing-agent" || accepted.Child.Source != RunSourceHandoff {
		t.Fatalf("handoff child = %#v", accepted.Child)
	}
	persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != AgentRunStatusCompleted || persistedSource.CompletedAt == nil {
		t.Fatalf("handoff source = %#v, %v", persistedSource, err)
	}
	activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, TeamID: "gtm", Descending: true, Limit: 10})
	if err != nil || len(activity) != 3 {
		t.Fatalf("handoff team activity = %#v, %v", activity, err)
	}
}

func TestTeamWorkOfferBidsSelectDeterministicallyAndSurviveRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "team-offers.db")
	store, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "team-offers"}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
		Goal: "Coordinate release analysis", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	teamStore := activeCollaborationTeam(scope, "reviewers",
		kernelteam.RosterAssignment{ID: "z", RoleID: "reviewer", AgentDeploymentID: "z-agent"},
		kernelteam.RosterAssignment{ID: "a", RoleID: "reviewer", AgentDeploymentID: "a-agent"},
		kernelteam.RosterAssignment{ID: "m", RoleID: "reviewer", AgentDeploymentID: "m-agent"})
	service := NewCollaborationService(store)
	service.teams = teamStore
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "coordinator"},
		Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "reviewers"}, SemanticRole: "reviewer",
		Goal: "Review release evidence", IdempotencyKey: "release-review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(created.Request.Candidates); got != 3 {
		t.Fatalf("candidate count = %d, want 3: %#v", got, created.Request.Candidates)
	}
	bid, err := service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1, AgentDeploymentID: "z-agent",
		Decision: AgentRequestBidAccept, Reason: "available", IdempotencyKey: "z-accept",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision, AgentDeploymentID: "z-agent",
		Decision: AgentRequestBidAccept, Reason: "available", IdempotencyKey: "z-accept",
	})
	if err != nil || replayed.Request.Revision != bid.Request.Revision {
		t.Fatalf("idempotent bid = %#v, %v", replayed, err)
	}
	bid, err = service.SubmitAgentRequestBid(ctx, SubmitAgentRequestBidRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision, AgentDeploymentID: "a-agent",
		Decision: AgentRequestBidAccept, Reason: "available", IdempotencyKey: "a-accept",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedService := NewCollaborationService(restarted)
	restartedService.teams = teamStore
	accepted, err := restartedService.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: bid.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "reviewers"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "a-agent" || accepted.Request.AssignmentDecision == nil ||
		!strings.Contains(accepted.Request.AssignmentDecision.Reason, "rank 1") || accepted.Child.AssignedAgentID != "a-agent" {
		t.Fatalf("deterministic assignment = %#v / %#v", accepted.Request, accepted.Child)
	}
}

func TestTeamWorkOfferFiltersExactRoleSkillsAndRecordsCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "team-authority"}
	memory := NewMemoryStore()
	store := &collaborationAgentCatalogStore{
		MemoryStore: memory,
		deployments: map[string]*kernelagent.AgentDeployment{
			"eligible": {ID: "eligible", DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2}},
			"busy":     {ID: "busy", DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}},
			"wrong":    {ID: "wrong", DefinitionID: "writer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 4}},
		},
		definitions: map[string]*kernelagent.AgentDefinition{
			"analyst@1": {ID: "analyst", Version: "1", Authority: kernelagent.AuthorityPolicy{AllowedSkillIDs: []string{"research"}}},
			"writer@1":  {ID: "writer", Version: "1", Authority: kernelagent.AuthorityPolicy{AllowedSkillIDs: []string{"research"}}},
		},
	}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead",
		Goal: "Coordinate analysis", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, agentID := range []string{"eligible", "busy"} {
		if _, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
			Goal: fmt.Sprintf("existing work %d", index), Source: RunSourceManual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewCollaborationService(store)
	service.teams = collaborationTeamStoreStub{
		deployment: &kernelteam.Deployment{
			ID: "analysis", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analysis", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Revision: 1, Roster: []kernelteam.RosterAssignment{
				{ID: "eligible", RoleID: "analyst", AgentDeploymentID: "eligible"},
				{ID: "busy", RoleID: "analyst", AgentDeploymentID: "busy"},
				{ID: "wrong", RoleID: "analyst", AgentDeploymentID: "wrong"},
			},
		},
		definition: &kernelteam.Definition{ID: "analysis", Version: "1", Roles: []kernelteam.RoleSlot{{
			ID: "analyst", RequiredDefinitionIDs: []string{"analyst"}, RequiredSkillIDs: []string{"research"},
		}}},
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"}, Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "analysis"},
		SemanticRole: "analyst", Goal: "Analyze evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Request.Candidates) != 2 || created.Request.Candidates[0].AgentDeploymentID != "busy" ||
		created.Request.Candidates[0].AvailableSlots != 0 || created.Request.Candidates[1].AgentDeploymentID != "eligible" ||
		created.Request.Candidates[1].AvailableSlots != 1 {
		t.Fatalf("authority and capacity candidates = %#v", created.Request.Candidates)
	}
}

func TestAgentRequestSourceLifecycleIsEnforced(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		run  *AgentRun
		want bool
	}{
		{name: "queued", run: &AgentRun{Status: AgentRunStatusQueued}, want: true},
		{name: "planning", run: &AgentRun{Status: AgentRunStatusPlanning}, want: true},
		{name: "running", run: &AgentRun{Status: AgentRunStatusRunning}, want: true},
		{name: "already waiting", run: &AgentRun{Status: AgentRunStatusRunning, WakeCondition: &WakeCondition{Type: "event"}}},
		{name: "paused", run: &AgentRun{Status: AgentRunStatusPaused}},
		{name: "completed", run: &AgentRun{Status: AgentRunStatusCompleted}},
		{name: "failed", run: &AgentRun{Status: AgentRunStatusFailed}},
		{name: "canceled", run: &AgentRun{Status: AgentRunStatusCanceled}},
		{name: "missing run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CanStartAgentRequestFromRun(test.run); got != test.want {
				t.Fatalf("CanStartAgentRequestFromRun() = %t, want %t", got, test.want)
			}
		})
	}

	store := NewMemoryStore()
	portfolio := NewPortfolioService(store)
	activity := NewRunActivityService(store, store)
	service := NewCollaborationService(store)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}
	recipient := CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}

	terminal, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Already shipped", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err = activity.TransitionRun(ctx, scope, terminal.ID, RunTransitionRequest{ExpectedRevision: terminal.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err = activity.TransitionRun(ctx, scope, terminal.ID, RunTransitionRequest{ExpectedRevision: terminal.Revision, Status: AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []AgentRequestKind{AgentRequestKindRequest, AgentRequestKindHandoff, AgentRequestKindEscalation} {
		_, createErr := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
			Scope: scope, Kind: kind, SourceRunID: terminal.ID,
			Requester: CollaborationParty(owner), Recipient: recipient, Goal: "Start follow-up",
		})
		if !errors.Is(createErr, ErrInvalidAgentRequestState) || !strings.Contains(createErr.Error(), "completed") {
			t.Fatalf("create %s from terminal source error = %v", kind, createErr)
		}
	}

	active, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Ship next release", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: active.ID,
		Requester: CollaborationParty(owner), Recipient: recipient, Goal: "Prepare the release brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err = portfolio.GetAgentRun(ctx, scope, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	active, _, err = activity.TransitionRun(ctx, scope, active.ID, RunTransitionRequest{ExpectedRevision: active.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = activity.TransitionRun(ctx, scope, active.ID, RunTransitionRequest{ExpectedRevision: active.Revision, Status: AgentRunStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: recipient,
	})
	if !errors.Is(err, ErrInvalidAgentRequestState) || !strings.Contains(err.Error(), "completed") {
		t.Fatalf("accept after source completion error = %v", err)
	}
	restored, err := service.GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.Status != AgentRequestStatusPending || restored.ChildRunID != "" {
		t.Fatalf("request mutated after rejected acceptance: %#v, %v", restored, err)
	}
}

func TestCollaborationRejectsCredentialTransferAndUnauthorizedRequesters(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	base := CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "draft launch",
	}
	unsafe := base
	unsafe.SharedContext = map[string]interface{}{"nested": map[string]interface{}{"api_token": "secret"}}
	if _, err := service.CreateAgentRequest(ctx, unsafe); !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe context error = %v", err)
	}
	unsafeCheckpoint := base
	unsafeCheckpoint.ChildCheckpoint = map[string]interface{}{"nested": map[string]interface{}{"accessToken": "secret"}}
	if _, err := service.CreateAgentRequest(ctx, unsafeCheckpoint); !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe child checkpoint error = %v", err)
	}
	unauthorized := base
	unauthorized.Requester = CollaborationParty{Type: OwnerTypeAgent, ID: "intruder"}
	if _, err := service.CreateAgentRequest(ctx, unauthorized); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("unauthorized requester error = %v", err)
	}
}

func TestCollaborationCompletionRejectsInvalidAuthorityArtifactsAndEvidence(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "developer",
		Goal: "Launch", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	service.teams = activeCollaborationTeam(scope, "marketing", kernelteam.RosterAssignment{ID: "editor", RoleID: "publisher", AgentDeploymentID: "marketing-editor"})
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"},
		Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "marketing"}, Goal: "Publish launch brief",
		AcceptanceCriteria: map[string]interface{}{"reviewed": true},
		ArtifactRequirements: []ArtifactRequirement{{
			Name: "brief", Type: "document", Required: true,
			Schema: map[string]interface{}{"type": "object", "required": []interface{}{"reviewed"}, "properties": map[string]interface{}{"reviewed": map[string]interface{}{"const": true}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1, Decision: AgentRequestDecisionAccept,
		Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.AssignedAgentID != "marketing-editor" || accepted.Child.AssignedAgentID != "marketing-editor" {
		t.Fatalf("automatic unique-role assignment = %#v", accepted)
	}
	for _, artifact := range []*Artifact{
		{
			ID: "brief-1", Version: 1, Scope: scope, Name: "brief.md", Type: "document",
			ContentRef: "artifact-store:brief-1", Digest: digestFor("brief-1"), Classification: ArtifactClassificationInternal,
			Metadata:   map[string]interface{}{"reviewed": true},
			Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "marketing-editor"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
		},
		{
			ID: "brief-bad-schema", Version: 1, Scope: scope, Name: "brief.md", Type: "document",
			ContentRef: "artifact-store:brief-bad-schema", Digest: digestFor("brief-bad-schema"), Classification: ArtifactClassificationInternal,
			Metadata:   map[string]interface{}{"reviewed": false},
			Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "marketing-editor"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
		},
	} {
		if _, err := NewArtifactCatalog(store).Register(ctx, RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatal(err)
		}
	}
	base := CompleteAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "marketing"},
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing-editor"}, Summary: "Brief ready",
		AcceptanceEvidence: map[string]interface{}{"reviewed": true}, CompletionKey: "brief-v1",
		Artifacts: []ArtifactReference{{
			ID: "brief-1", Version: 1, RequirementName: "brief",
		}},
	}
	unauthorized := base
	unauthorized.Principal = CollaborationParty{Type: OwnerTypeAgent, ID: "marketing-editor"}
	if _, err := service.CompleteAgentRequest(ctx, unauthorized); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("unauthorized completion error = %v", err)
	}
	missingEvidence := base
	missingEvidence.AcceptanceEvidence = nil
	if _, err := service.CompleteAgentRequest(ctx, missingEvidence); err == nil || !strings.Contains(err.Error(), "acceptance evidence") {
		t.Fatalf("missing evidence error = %v", err)
	}
	unsafeURL := base
	unsafeURL.Artifacts = cloneArtifactReferences(base.Artifacts)
	unsafeURL.Artifacts[0].ContentRef = "https://store.test/brief?token=secret"
	if _, err := service.CompleteAgentRequest(ctx, unsafeURL); err == nil || !strings.Contains(err.Error(), "only id, version") {
		t.Fatalf("unsafe artifact reference error = %v", err)
	}
	badSchema := base
	badSchema.Artifacts = cloneArtifactReferences(base.Artifacts)
	badSchema.Artifacts[0].ID = "brief-bad-schema"
	if _, err := service.CompleteAgentRequest(ctx, badSchema); err == nil || !strings.Contains(err.Error(), "JSON schema") {
		t.Fatalf("artifact schema error = %v", err)
	}
	tamperedMetadata := base
	tamperedMetadata.Artifacts = cloneArtifactReferences(base.Artifacts)
	tamperedMetadata.Artifacts[0].Metadata = map[string]interface{}{"reviewed": true}
	if _, err := service.CompleteAgentRequest(ctx, tamperedMetadata); err == nil || !strings.Contains(err.Error(), "only id, version") {
		t.Fatalf("tampered artifact metadata error = %v", err)
	}
	unregistered := base
	unregistered.Artifacts = []ArtifactReference{{ID: "missing", Version: 1, RequirementName: "brief"}}
	if _, err := service.CompleteAgentRequest(ctx, unregistered); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("unregistered artifact error = %v", err)
	}
	completed, err := service.CompleteAgentRequest(ctx, base)
	if err != nil || completed.Request.Status != AgentRequestStatusCompleted || completed.Request.Artifacts[0].ID != "brief-1" {
		t.Fatalf("valid Team completion = %#v, %v", completed, err)
	}
}

func TestSQLiteCollaborationSurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "collaboration-restart.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewCollaborationService(store).CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "analyst"}, Goal: "measure launch impact",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewCollaborationService(reopened).GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil || restored.Goal != "measure launch impact" || restored.Revision != 1 {
		t.Fatalf("restored request = %#v, %v", restored, err)
	}
}

func TestTerminalFailedHandoffChildResolvesRequestAndSource(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Build the feature", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "Publish the launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: AgentRunStatusRunning,
		Summary: "Delegated run started", Actor: ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusFailed, Error: "provider exhausted retries",
		Summary: "Delegated run failed", Actor: ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveTerminalAgentRequestChild(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Request.Status != AgentRequestStatusFailed || resolved.Request.ResolutionReason != "provider exhausted retries" {
		t.Fatalf("resolved request = %#v", resolved.Request)
	}
	refreshedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, _ := refreshedSource.Output["collaborationResults"].(map[string]interface{})
	result, _ := results[created.Request.ID].(map[string]interface{})
	if refreshedSource.Status != AgentRunStatusCompleted || result["status"] != string(AgentRequestStatusFailed) ||
		result["reason"] != "provider exhausted retries" {
		t.Fatalf("source=%#v result=%#v", refreshedSource, result)
	}
}

func TestTerminalChildCompletesArtifactBearingRequestFromRegisteredOutput(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "artifact-delegation"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, Goal: "Create the launch report",
		ArtifactRequirements: []ArtifactRequirement{{Name: "launch-report", Type: "report", Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewArtifactCatalog(store).Register(ctx, RegisterArtifactRequest{Artifact: &Artifact{
		ID: "launch-report", Version: 1, Scope: scope, Name: "launch-report.pdf", Type: "report", MediaType: "application/pdf",
		ContentRef: "artifact-store:launch-report", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 4096, Classification: ArtifactClassificationInternal,
		Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "marketing"}, RunID: accepted.Child.ID, RequestID: created.Request.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: AgentRunStatusRunning,
		Summary: "Creating report", Actor: ActivityActor{Type: "worker", ID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completedChild, _, err := activity.TransitionRun(ctx, scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted,
		Summary: "Report created", Actor: ActivityActor{Type: "worker", ID: "worker-1"},
		Output: map[string]interface{}{
			"summary":      "Delivered the launch report.",
			"artifactRefs": []interface{}{map[string]interface{}{"id": "launch-report", "version": 1, "requirementName": "launch-report"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveTerminalAgentRequestChild(ctx, completedChild)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Request.Status != AgentRequestStatusCompleted || len(resolved.Request.Artifacts) != 1 ||
		resolved.Request.Artifacts[0].ID != "launch-report" || resolved.Request.Artifacts[0].ContentRef != "artifact-store:launch-report" ||
		resolved.Source.Status != AgentRunStatusQueued {
		t.Fatalf("artifact-bearing resolution = %#v", resolved)
	}
}
