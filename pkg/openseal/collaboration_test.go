package openseal

import (
	"context"
	"testing"
)

func TestPublicAgentRequestFacadeCreatesTraceableChildRun(t *testing.T) {
	t.Parallel()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "default"}
	source, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship the feature", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
		Goal:      "Prepare the launch", SemanticRole: "launch-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := engine.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child == nil || accepted.Child.ParentRunID != source.ID || accepted.Child.AssignedAgentID != "marketing" {
		t.Fatalf("accepted request = %#v", accepted)
	}
	requests, err := engine.ListAgentRequests(ctx, AgentRequestFilter{
		Scope: scope, Recipient: &CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil || len(requests) != 1 || requests[0].Status != AgentRequestStatusAccepted {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
}
