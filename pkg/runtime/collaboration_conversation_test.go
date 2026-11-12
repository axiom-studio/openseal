package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration/collaborationtest"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type countingConversationStore struct {
	*sqlite.Store
	conversationReads int
}

func (s *countingConversationStore) GetConversation(ctx context.Context, scope models.Scope, id string) (*models.Conversation, error) {
	s.conversationReads++
	return s.Store.GetConversation(ctx, scope, id)
}

func TestAgentRequestConversationProjectionTreatsDeletedConversationAsTerminal(t *testing.T) {
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "deleted-conversation-projection"}
	store := &countingConversationStore{Store: kerneltest.NewStore(t)}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = collaboration.NewCollaborationService(store).CreateAgentRequest(ctx, collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "requester"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"},
		Goal:      "Review work", ConversationRefs: []string{"deleted-channel"}, IdempotencyKey: "deleted-channel-request",
	}); err != nil {
		t.Fatal(err)
	}
	projector, err := collaboration.NewAgentRequestConversationProjector(store)
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
	scope := models.Scope{Kind: "tenant", ID: "conversation-projection"}
	store := &collaborationtest.TeamStore{
		Store: kerneltest.NewStore(t),
		Deployment: &kernelteam.Deployment{
			ID: "release-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "release-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
			Roster: []kernelteam.RosterAssignment{
				{ID: "reviewer", AgentDeploymentID: "reviewer-agent", Approver: true},
				{ID: "specialist", AgentDeploymentID: "specialist-agent"},
			},
		},
		Definition: &kernelteam.Definition{
			ID: "release-team", Version: "1",
			Delegation: kernelteam.DelegationPolicy{
				MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true,
				RequireAcceptance: true, RequireCompletionReview: true,
			},
		},
	}
	seedTestSpace(t, store, scope)
	conversation, _, err := runtimeconversation.NewConversationService(store).CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "release-team"},
		Title: "Release coordination", IdempotencyKey: "release-coordination",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "reviewer-agent",
		Goal: "Coordinate release evidence", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	created, err := service.CreateAgentRequest(t.Context(), collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindHandoff,
		Requester:   models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "release-team"},
		Recipient:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "specialist-agent"},
		SourceRunID: source.ID, Goal: "Prepare the release evidence", SemanticRole: "specialist",
		AcceptanceCriteria: map[string]interface{}{"reviewed": true}, ConversationRefs: []string{conversation.ID},
		IdempotencyKey: "release-evidence-handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := collaboration.NewAgentRequestConversationProjector(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(t.Context(), collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: collaboration.AgentRequestDecisionAccept, Principal: created.Request.Recipient,
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "specialist-agent"}, AssignedAgentID: "specialist-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	completed, err := service.CompleteAgentRequest(t.Context(), collaboration.CompleteAgentRequestRequest{
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
	reviewed, err := service.ReviewAgentRequestCompletion(t.Context(), collaboration.ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: completed.Request.ID, ExpectedRevision: completed.Request.Revision,
		ExpectedChildRevision: completed.Child.Revision, Principal: completed.Request.Requester,
		Actor: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer-agent"}, Approve: true,
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
	messages, err := runtimeconversation.NewConversationService(store).ListChannelMessages(t.Context(), models.ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	root, acceptance, completion, decision := messages[0], messages[1], messages[2], messages[3]
	if root.Intent != models.MessageIntentHandoff || !root.RequiresResponse || len(root.References) != 2 ||
		root.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRequest, ID: reviewed.Request.ID}) {
		t.Fatalf("handoff = %#v", root)
	}
	if acceptance.Intent != models.MessageIntentAcknowledgment || acceptance.ResolvesMessageID != root.ID || acceptance.ThreadRootID != root.ID {
		t.Fatalf("acceptance = %#v", acceptance)
	}
	if completion.Intent != models.MessageIntentProposal || !completion.RequiresResponse || completion.ThreadRootID != root.ID {
		t.Fatalf("completion = %#v", completion)
	}
	if decision.Intent != models.MessageIntentDecision || decision.ResolvesMessageID != completion.ID ||
		decision.ThreadRootID != root.ID || decision.Sender.ID != "reviewer-agent" {
		t.Fatalf("decision = %#v", decision)
	}
	resolved := map[string]bool{}
	for _, message := range messages {
		resolved[message.ResolvesMessageID] = true
	}
	for _, message := range messages {
		if message.RequiresResponse && !resolved[message.ID] {
			t.Fatalf("open message = %#v", message)
		}
	}
}

func TestAgentRequestEscalationIsDurableAndProjected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "escalation-projection"}
	store := kerneltest.NewStore(t)
	seedTestSpace(t, store, scope)
	conversation, _, err := runtimeconversation.NewConversationService(store).CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "coordinator"},
		Title: "Incident coordination", IdempotencyKey: "incident-coordination",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "coordinator"}, AssignedAgentID: "coordinator",
		Goal: "Restore the service", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := collaboration.NewCollaborationService(store)
	created, err := service.CreateAgentRequest(ctx, collaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindEscalation, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "coordinator"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "incident-commander"},
		Goal:      "Resolve the production blocker", ConversationRefs: []string{conversation.ID},
		IdempotencyKey: "escalate-production-blocker",
	})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := collaboration.NewAgentRequestConversationProjector(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projector.Reconcile(ctx, scope); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(ctx, collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: collaboration.AgentRequestDecisionAccept, Principal: created.Request.Recipient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Source != models.RunSourceEscalation || accepted.Source.Status != models.AgentRunStatusWaitingForDependency ||
		accepted.Source.WakeCondition == nil || accepted.Source.WakeCondition.Reference != created.Request.ID {
		t.Fatalf("accepted escalation = %#v / %#v", accepted.Source, accepted.Child)
	}
	completed, err := service.CompleteAgentRequest(ctx, collaboration.CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Production blocker resolved", CompletionKey: "escalation-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Request.Status != models.AgentRequestStatusCompleted || completed.Source.Status != models.AgentRunStatusQueued {
		t.Fatalf("completed escalation = %#v / %#v", completed.Request, completed.Source)
	}
	if _, err = projector.Reconcile(ctx, scope); err != nil {
		t.Fatal(err)
	}
	messages, err := runtimeconversation.NewConversationService(store).ListChannelMessages(ctx, models.ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Intent != models.MessageIntentEscalation ||
		messages[1].Intent != models.MessageIntentAcknowledgment || messages[2].Intent != models.MessageIntentUpdate ||
		messages[1].ThreadRootID != messages[0].ID || messages[2].ThreadRootID != messages[0].ID {
		t.Fatalf("escalation messages = %#v", messages)
	}
	events, err := store.ListActivity(ctx, models.ActivityFilter{
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
