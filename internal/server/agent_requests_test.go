package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestAgentRequestAPIAndClientCompletePortableLifecycle(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore(100)
	scope := runtime.Scope{Kind: "local", ID: "workspace"}
	source, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship a durable feature", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	kernel := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	document, err := kernel.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.AgentRequestsCapabilityID, kernelapi.AgentRequestsCapabilityVersion)
	expectedCapability := kernelapi.AgentRequestsCapability()
	if !ok || capability.ID != expectedCapability.ID || capability.Version != expectedCapability.Version || capability.Available != expectedCapability.Available || !slices.Equal(capability.Operations, expectedCapability.Operations) {
		t.Fatalf("agent request capability = %#v", capability)
	}

	requester := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "developer"}
	recipient := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "marketing"}
	create := kernelapi.CreateAgentRequestRequest{
		Scope: scope, Kind: runtime.AgentRequestKindRequest, Requester: requester, Recipient: recipient,
		SourceRunID: source.ID, Goal: "Turn the release into a launch brief", SharedContext: map[string]interface{}{"release": "2026.07"},
	}
	created, err := kernel.CreateAgentRequest(t.Context(), create, "launch-brief-request")
	if err != nil {
		t.Fatal(err)
	}
	if created.Request == nil || created.Request.Status != runtime.AgentRequestStatusPending || created.Request.IdempotencyKey != "launch-brief-request" {
		t.Fatalf("created request = %#v", created)
	}
	replayed, err := kernel.CreateAgentRequest(t.Context(), create, "launch-brief-request")
	if err != nil || replayed.Request.ID != created.Request.ID || len(replayed.Events) != 0 {
		t.Fatalf("replayed request = %#v, %v", replayed, err)
	}

	listed, err := kernel.ListAgentRequests(t.Context(), runtime.AgentRequestFilter{Scope: scope, Recipient: &recipient, Statuses: []runtime.AgentRequestStatus{runtime.AgentRequestStatusPending}, Limit: 25})
	if err != nil || len(listed) != 1 || listed[0].ID != created.Request.ID {
		t.Fatalf("listed requests = %#v, %v", listed, err)
	}
	restored, err := kernel.GetAgentRequest(t.Context(), scope, created.Request.ID)
	if err != nil || restored.Revision != created.Request.Revision {
		t.Fatalf("restored request = %#v, %v", restored, err)
	}

	accepted, err := kernel.RespondAgentRequest(t.Context(), scope, created.Request.ID, kernelapi.RespondAgentRequestRequest{
		ExpectedRevision: restored.Revision, Decision: runtime.AgentRequestDecisionAccept, Principal: recipient,
		Message: "I will return a reviewed brief.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Request.Status != runtime.AgentRequestStatusAccepted || accepted.Child == nil || accepted.Child.ParentRunID != source.ID {
		t.Fatalf("accepted request = %#v", accepted)
	}

	completed, err := kernel.CompleteAgentRequest(t.Context(), scope, created.Request.ID, kernelapi.CompleteAgentRequestRequest{
		ExpectedRevision: accepted.Request.Revision, ExpectedChildRevision: accepted.Child.Revision,
		Principal: recipient, Summary: "Delivered the reviewed launch brief.",
	}, "launch-brief-completion")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Request.Status != runtime.AgentRequestStatusCompleted || completed.Source.Status != runtime.AgentRunStatusQueued || completed.Child.Status != runtime.AgentRunStatusCompleted {
		t.Fatalf("completed request = %#v", completed)
	}
}

func TestAgentRequestAPIRejectsUnsafeSharedContext(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore(10)
	scope := runtime.Scope{Kind: "local", ID: "workspace"}
	source, err := runtime.NewPortfolioService(store).CreateAgentRun(context.Background(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	kernel := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	_, err = kernel.CreateAgentRequest(t.Context(), kernelapi.CreateAgentRequestRequest{
		Scope: scope, Kind: runtime.AgentRequestKindRequest,
		Requester:   runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "developer"},
		Recipient:   runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "marketing"},
		SourceRunID: source.ID, Goal: "Draft", SharedContext: map[string]interface{}{"apiKey": "must-not-cross"},
	}, "unsafe-request")
	var apiError *client.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 400 {
		t.Fatalf("unsafe shared context error = %#v", err)
	}
}
