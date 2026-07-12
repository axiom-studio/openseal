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
	completed, err := engine.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
		Summary: "Launch plan delivered", CompletionKey: "launch-plan-v1",
	})
	if err != nil || completed.Request.Status != AgentRequestStatusCompleted || completed.Source.Status != AgentRunStatusQueued || completed.Child.Status != AgentRunStatusCompleted {
		t.Fatalf("completed request = %#v, %v", completed, err)
	}
	requests, err := engine.ListAgentRequests(ctx, AgentRequestFilter{
		Scope: scope, Recipient: &CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil || len(requests) != 1 || requests[0].Status != AgentRequestStatusCompleted {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
}

func TestPublicAgentRequestGroupFacadeJoinsTeamWork(t *testing.T) {
	t.Parallel()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "grouped-work"}
	source, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
		Goal: "Synthesize research", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.CreateAgentRequestGroup(ctx, CreateAgentRequestGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "lead"},
		Policy:    RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Requests: []AgentRequestGroupSpec{
			{DependencyID: "one", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "one"}, Goal: "Research one"},
			{DependencyID: "two", Kind: AgentRequestKindRequest, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "two"}, Goal: "Research two"},
		},
		IdempotencyKey: "public-group-v1", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil || len(created.Requests) != 2 {
		t.Fatalf("created group = %#v, %v", created, err)
	}
	for index, request := range created.Requests {
		accepted, err := engine.RespondAgentRequest(ctx, RespondAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
			Decision: AgentRequestDecisionAccept, Principal: request.Recipient,
		})
		if err != nil {
			t.Fatal(err)
		}
		completed, err := engine.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
			Scope: scope, RequestID: request.ID, ExpectedRevision: accepted.Request.Revision,
			ExpectedChildRevision: accepted.Child.Revision, Principal: request.Recipient,
			Summary: "Done", CompletionKey: "complete-" + request.DependencyID,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := AgentRunStatusWaitingForDependency
		if index == len(created.Requests)-1 {
			want = AgentRunStatusQueued
		}
		if completed.Source.Status != want {
			t.Fatalf("completion %d source status = %s, want %s", index, completed.Source.Status, want)
		}
	}
}
