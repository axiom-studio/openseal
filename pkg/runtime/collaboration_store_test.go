package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestAgentRequestCreatesCredentialIsolatedChildLineage(t *testing.T) {
	t.Parallel()
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "product"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: models.RunSourceManual,
		Context: map[string]interface{}{"privateCredentialRef": "binding:source-only", "private": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
		Goal:      "Create the launch brief", SemanticRole: "launch-editor",
		SharedContext: map[string]interface{}{"release": "2026.07"}, IdempotencyKey: "launch-brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.CreateAgentRequest(ctx, collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
		Goal:      "Create the launch brief", SemanticRole: "launch-editor",
		SharedContext: map[string]interface{}{"release": "2026.07"}, IdempotencyKey: "launch-brief",
	})
	if err != nil || duplicate.Request.ID != created.Request.ID {
		t.Fatalf("idempotent request = %#v, %v", duplicate, err)
	}
	accepted, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: collaboration.AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
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
	if err != nil || persistedSource.Status != models.AgentRunStatusWaitingForDependency || persistedSource.WakeCondition == nil || persistedSource.WakeCondition.Reference != created.Request.ID {
		t.Fatalf("source = %#v, %v", persistedSource, err)
	}
	activity, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, TeamID: "product", Descending: true, Limit: 10})
	if err != nil || len(activity) != 3 {
		t.Fatalf("team activity = %#v, %v", activity, err)
	}
}

func TestAgentRequestNegotiationAndSafety(t *testing.T) {
	t.Parallel()
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	base := collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "analyst"}, Goal: "Measure impact",
	}
	unsafe := base
	unsafe.SharedContext = map[string]interface{}{"nested": map[string]interface{}{"access_token": "no"}}
	if _, err := service.CreateAgentRequest(ctx, unsafe); !errors.Is(err, models.ErrUnsafeSharedContext) {
		t.Fatalf("unsafe context error = %v", err)
	}
	created, err := service.CreateAgentRequest(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	question, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: 1,
		Decision: collaboration.AgentRequestDecisionRequestClarification, Principal: base.Recipient, Message: "Which cohort?",
	})
	if err != nil {
		t.Fatal(err)
	}
	clarified, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: question.Request.Revision,
		Decision: collaboration.AgentRequestDecisionProvideClarification, Principal: base.Requester, Message: "Platform engineers.",
	})
	if err != nil || clarified.Request.Status != models.AgentRequestStatusPending {
		t.Fatalf("clarification = %#v, %v", clarified, err)
	}
}
