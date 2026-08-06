package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestAgentRequestConversationProjectionShowsHandoffAndIndependentReview(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "conversation-projection"}
	store := &agentRequestInboxTeamStore{
		MemoryStore: NewMemoryStore(),
		deployment: &kernelteam.Deployment{
			ID: "release-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "release-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
			Roster: []kernelteam.RosterAssignment{
				{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: "reviewer-agent"},
				{ID: "specialist", RoleID: "specialist", AgentDeploymentID: "specialist-agent"},
			},
		},
		definition: &kernelteam.Definition{
			ID: "release-team", Version: "1",
			Approvals: kernelteam.ApprovalPolicy{ApproverRoleIDs: []string{"reviewer"}},
			Delegation: kernelteam.DelegationPolicy{
				MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true,
				RequireAcceptance: true, RequireCompletionReview: true,
			},
		},
	}
	conversation, _, err := NewConversationService(store).CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"},
		Title: "Release coordination", IdempotencyKey: "release-coordination",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "reviewer-agent",
		Goal: "Coordinate release evidence", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff,
		Requester:   CollaborationParty{Type: OwnerTypeTeam, ID: "release-team"},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: "specialist-agent"},
		SourceRunID: source.ID, Goal: "Prepare the release evidence", SemanticRole: "specialist",
		AcceptanceCriteria: map[string]interface{}{"reviewed": true}, ConversationRefs: []string{conversation.ID},
		IdempotencyKey: "release-evidence-handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewAgentRequestConversationProjector(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "specialist-agent"}, AssignedAgentID: "specialist-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	completed, err := service.CompleteAgentRequest(t.Context(), CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Evidence is ready", AcceptanceEvidence: map[string]interface{}{"reviewed": true},
		CompletionKey: "release-evidence-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	reviewed, err := service.ReviewAgentRequestCompletion(t.Context(), ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: completed.Request.ID, ExpectedRevision: completed.Request.Revision,
		ExpectedChildRevision: completed.Child.Revision, Principal: completed.Request.Requester,
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer-agent"}, Approve: true,
		Summary: "Evidence satisfies the review criteria", IdempotencyKey: "release-evidence-review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	// Reconciliation is replay-safe after every lifecycle transition.
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	messages, err := NewConversationService(store).ListChannelMessages(t.Context(), ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	root, acceptance, completion, decision := messages[0], messages[1], messages[2], messages[3]
	if root.Intent != MessageIntentHandoff || !root.RequiresResponse || len(root.References) != 2 ||
		root.References[0] != (ConversationReference{Kind: ConversationReferenceRequest, ID: reviewed.Request.ID}) {
		t.Fatalf("handoff = %#v", root)
	}
	if acceptance.Intent != MessageIntentAcknowledgment || acceptance.ResolvesMessageID != root.ID || acceptance.ThreadRootID != root.ID {
		t.Fatalf("acceptance = %#v", acceptance)
	}
	if completion.Intent != MessageIntentProposal || !completion.RequiresResponse || completion.ThreadRootID != root.ID {
		t.Fatalf("completion = %#v", completion)
	}
	if decision.Intent != MessageIntentDecision || decision.ResolvesMessageID != completion.ID ||
		decision.ThreadRootID != root.ID || decision.Sender.ID != "reviewer-agent" {
		t.Fatalf("decision = %#v", decision)
	}
	if open := openConversationMessages(messages); len(open) != 0 {
		t.Fatalf("open messages = %#v", open)
	}
}
