package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCollaborationRequestLifecycleAcrossPortableStores(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (CollaborationKernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (CollaborationKernelStore, func()) { return NewMemoryStore(100), func() {} }},
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
				Context: map[string]interface{}{"sourceOnly": true, "apiKey": "must-not-transfer"},
				Budget:  map[string]interface{}{"tokens": 1000}, Policy: map[string]interface{}{"risk": "guarded"},
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
				AcceptanceCriteria:   map[string]interface{}{"format": "markdown"},
				ArtifactRequirements: []ArtifactRequirement{{Name: "launch-brief", Type: "document", Required: true}},
				SharedContext:        map[string]interface{}{"release": "2026.07"}, ConversationRefs: []string{"team:gtm:42"},
				IdempotencyKey: "release-launch-brief",
			}
			created, err := service.CreateAgentRequest(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if created.Request.Status != AgentRequestStatusPending || len(created.Events) != 1 || created.Events[0].TeamID != "product" || created.Events[0].Visibility != ActivityVisibilityTeam {
				t.Fatalf("created request = %#v, events = %#v", created.Request, created.Events)
			}
			duplicate, err := service.CreateAgentRequest(ctx, create)
			if err != nil || duplicate.Request.ID != created.Request.ID || len(duplicate.Events) != 0 {
				t.Fatalf("idempotent request = %#v, %v", duplicate, err)
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
			if accepted.Child.Owner != source.Owner || accepted.Child.AssignedAgentID != "marketing" || accepted.Child.Source != RunSourceRequest {
				t.Fatalf("child ownership = %#v", accepted.Child)
			}
			if accepted.Child.Context["sourceOnly"] != nil || accepted.Child.Context["apiKey"] != nil || accepted.Child.Context["release"] != "2026.07" {
				t.Fatalf("child context leaked source state: %#v", accepted.Child.Context)
			}
			persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
			if err != nil || persistedSource.Status != AgentRunStatusWaitingForDependency || persistedSource.WakeCondition == nil || persistedSource.WakeCondition.Reference != created.Request.ID {
				t.Fatalf("source after acceptance = %#v, %v", persistedSource, err)
			}
			child, err := portfolio.GetAgentRun(ctx, scope, accepted.Child.ID)
			if err != nil || child.AssignedAgentID != "marketing" {
				t.Fatalf("persisted child = %#v, %v", child, err)
			}
			activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 20})
			if err != nil || len(activity) != 5 {
				t.Fatalf("team activity = %#v, %v", activity, err)
			}
		})
	}
}

func TestCollaborationHandoffTransfersOwnership(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(100)
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
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, Goal: "Own launch and lead follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}) || accepted.Child.AssignedAgentID != "" || accepted.Child.Source != RunSourceHandoff {
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

func TestCollaborationRejectsCredentialTransferAndUnauthorizedRequesters(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(10)
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
	unauthorized := base
	unauthorized.Requester = CollaborationParty{Type: OwnerTypeAgent, ID: "intruder"}
	if _, err := service.CreateAgentRequest(ctx, unauthorized); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("unauthorized requester error = %v", err)
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
