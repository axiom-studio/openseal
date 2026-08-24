package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type countingConversationStore struct {
	*MemoryStore
	conversationReads int
}

func (s *countingConversationStore) GetConversation(ctx context.Context, scope Scope, id string) (*Conversation, error) {
	s.conversationReads++
	return s.MemoryStore.GetConversation(ctx, scope, id)
}

func TestAgentRequestConversationProjectionTreatsDeletedConversationAsTerminal(t *testing.T) {
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "deleted-conversation-projection"}
	store := &countingConversationStore{MemoryStore: NewMemoryStore()}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewCollaborationService(store).CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "requester"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"},
		Goal:      "Review work", ConversationRefs: []string{"deleted-channel"}, IdempotencyKey: "deleted-channel-request",
	}); err != nil {
		t.Fatal(err)
	}
	projector, err := NewAgentRequestConversationProjector(store)
	if err != nil {
		t.Fatal(err)
	}
	if projected, err := projector.Reconcile(ctx, scope); err != nil || projected != 0 {
		t.Fatalf("first projection = %d, %v", projected, err)
	}
	if projected, err := projector.Reconcile(ctx, scope); err != nil || projected != 0 {
		t.Fatalf("replayed projection = %d, %v", projected, err)
	}
	if store.conversationReads != 1 {
		t.Fatalf("deleted conversation reads = %d, want 1", store.conversationReads)
	}
}

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

func TestAgentRequestEscalationIsDurableAndProjected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "escalation-projection"}
	store := NewMemoryStore()
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coordinator"},
		Title: "Incident coordination", IdempotencyKey: "incident-coordination",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
		Goal: "Restore the service", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindEscalation, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "coordinator"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "incident-commander"},
		Goal:      "Resolve the production blocker", ConversationRefs: []string{conversation.ID},
		IdempotencyKey: "escalate-production-blocker",
	})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewAgentRequestConversationProjector(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(ctx, scope); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Source != RunSourceEscalation || accepted.Source.Status != AgentRunStatusWaitingForDependency ||
		accepted.Source.WakeCondition == nil || accepted.Source.WakeCondition.Reference != created.Request.ID {
		t.Fatalf("accepted escalation = %#v / %#v", accepted.Source, accepted.Child)
	}
	completed, err := service.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Production blocker resolved", CompletionKey: "escalation-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Request.Status != AgentRequestStatusCompleted || completed.Source.Status != AgentRunStatusQueued {
		t.Fatalf("completed escalation = %#v / %#v", completed.Request, completed.Source)
	}
	if _, err = projector.Reconcile(ctx, scope); err != nil {
		t.Fatal(err)
	}
	messages, err := NewConversationService(store).ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Intent != MessageIntentEscalation ||
		messages[1].Intent != MessageIntentAcknowledgment || messages[2].Intent != MessageIntentUpdate ||
		messages[1].ThreadRootID != messages[0].ID || messages[2].ThreadRootID != messages[0].ID {
		t.Fatalf("escalation messages = %#v", messages)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{
		Scope: scope, RunIDs: []string{source.ID, accepted.Child.ID},
		EventTypes: []string{"escalation.requested", "escalation.accepted", "escalation.completed"}, Descending: true, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.EventType] = true
	}
	for _, eventType := range []string{"escalation.requested", "escalation.accepted", "escalation.completed"} {
		if !seen[eventType] {
			t.Fatalf("missing %s in %#v", eventType, events)
		}
	}
}
