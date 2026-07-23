package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryAgentRequestCreatesCredentialIsolatedChildLineage(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: RunSourceManual,
		Context: map[string]interface{}{"privateCredentialRef": "binding:source-only", "private": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
		Goal:      "Create the launch brief", SemanticRole: "launch-editor",
		SharedContext: map[string]interface{}{"release": "2026.07"}, IdempotencyKey: "launch-brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
		Goal:      "Create the launch brief", SemanticRole: "launch-editor",
		SharedContext: map[string]interface{}{"release": "2026.07"}, IdempotencyKey: "launch-brief",
	})
	if err != nil || duplicate.Request.ID != created.Request.ID {
		t.Fatalf("idempotent request = %#v, %v", duplicate, err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child == nil || accepted.Child.ParentRunID != source.ID || accepted.Child.RootRunID != source.RootRunID || accepted.Child.Owner != source.Owner || accepted.Child.AssignedAgentID != "marketing" {
		t.Fatalf("child lineage = %#v", accepted.Child)
	}
	if accepted.Child.Context["privateCredentialRef"] != nil || accepted.Child.Context["private"] != nil || accepted.Child.Context["release"] != "2026.07" {
		t.Fatalf("child context = %#v", accepted.Child.Context)
	}
	persistedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != AgentRunStatusWaitingForDependency || persistedSource.WakeCondition == nil || persistedSource.WakeCondition.Reference != created.Request.ID {
		t.Fatalf("source = %#v, %v", persistedSource, err)
	}
	activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 10})
	if err != nil || len(activity) != 3 {
		t.Fatalf("team activity = %#v, %v", activity, err)
	}
}

func TestMemoryAgentRequestNegotiationAndSafety(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "acme"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	base := CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "analyst"}, Goal: "Measure impact",
	}
	unsafe := base
	unsafe.SharedContext = map[string]interface{}{"nested": map[string]interface{}{"access_token": "no"}}
	if _, err := service.CreateAgentRequest(ctx, unsafe); !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe context error = %v", err)
	}
	created, err := service.CreateAgentRequest(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	question, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: AgentRequestDecisionRequestClarification, Principal: base.Recipient, Message: "Which cohort?",
	})
	if err != nil {
		t.Fatal(err)
	}
	clarified, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: question.Request.Revision,
		Decision: AgentRequestDecisionProvideClarification, Principal: base.Requester, Message: "Platform engineers.",
	})
	if err != nil || clarified.Request.Status != AgentRequestStatusPending {
		t.Fatalf("clarification = %#v, %v", clarified, err)
	}
}
