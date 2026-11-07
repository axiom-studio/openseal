package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Use real catalog activation, rather than handing the worker a preselected
// runner or action snapshot. Changing the active definition and binding while
// the Run sleeps must affect the resumed Turn.
type workflowContinuationCatalog struct {
	registry *kernelagent.Registry
	skills   *skill.Catalog
	outreach *OutreachService
}

func (c workflowContinuationCatalog) GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	return c.registry.GetDeployment(ctx, scope, id)
}
func (c workflowContinuationCatalog) ListAgentDeployments(ctx context.Context, filter kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	return c.registry.ListDeployments(ctx, filter)
}
func (c workflowContinuationCatalog) GetAgentDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	return c.registry.GetDefinition(ctx, id, version)
}
func (c workflowContinuationCatalog) ActivateSkills(ctx context.Context, scope skill.ScopeReference, id string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return c.skills.Activate(ctx, scope, id, host)
}
func (c workflowContinuationCatalog) GetOutreachThread(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	return c.outreach.Get(ctx, scope, id)
}
func (c workflowContinuationCatalog) ReconcileOutreachAction(ctx context.Context, scope Scope, id string, request ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	return c.outreach.ReconcileAction(ctx, scope, id, request)
}
func (c workflowContinuationCatalog) GetActionCall(context.Context, Scope, string) (*ActionCall, error) {
	return nil, ErrActionNotFound
}

// The fake implements only the provider verification boundary. Tenant,
// deployment, binding, account pins and durable event publication are all
// supplied and enforced by the ordinary gateway ingress service.
type workflowContinuationProvider struct {
	event NormalizedExternalConversationEvent
	calls int
}

func (p *workflowContinuationProvider) NormalizeExternalConversationGateway(_ context.Context, request ExternalConversationGatewayHostRequest) (*ExternalConversationGatewayHostResult, error) {
	p.calls++
	if request.Adapter == nil || request.Adapter.Binding == nil || request.Adapter.Binding.ID != "event-account" || request.Gateway.Provider != "test" || request.Request == nil || len(request.Request.Headers["X-Test-Signature"]) != 1 || request.Request.Headers["X-Test-Signature"][0] != "verified" {
		return nil, errors.New("provider signature or installed binding does not match")
	}
	return &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{InstallationID: "installation-one", ApplicationID: "application-one", Address: p.event.ExternalConversationID, Event: p.event}}}, nil
}

func TestDeferredWorkflowOriginRejectsForgedContext(t *testing.T) {
	store := NewMemoryStore()
	source, catalog, bound, args := workflowActionFixture(t, store)
	result, err := dispatchWorkflowForTest(t, store, catalog, source, bound, args, "verified-origin")
	if err != nil {
		t.Fatal(err)
	}
	work := result["run"].(*AgentRun)
	for _, change := range []string{"missing-source", "self-source", "foreign-owner", "foreign-agent", "foreign-scope", "foreign-conversation", "foreign-trigger", "wrong-kind", "wrong-flag"} {
		t.Run(change, func(t *testing.T) {
			candidate := cloneAgentRun(work)
			switch change {
			case "missing-source":
				candidate.Context["workflowSourceRunId"] = "missing-source"
			case "self-source":
				candidate.Context["workflowSourceRunId"] = work.ID
			case "foreign-owner":
				candidate.Owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "foreign-team"}
			case "foreign-agent":
				candidate.Owner.ID, candidate.AssignedAgentID = "foreign-agent", "foreign-agent"
			case "foreign-scope":
				candidate.Scope.ID = "foreign-tenant"
			case "foreign-conversation":
				candidate.Context[conversationRunContextConversationID] = "foreign-conversation"
			case "foreign-trigger":
				candidate.Context[conversationRunContextTriggerID] = "foreign-human-message"
			case "wrong-kind":
				candidate.Kind = RunKindConversation
			case "wrong-flag":
				candidate.Context[scheduledTaskContextKey] = false
			}
			origin, channel, human, err := conversationWorkOrigin(t.Context(), store, store, candidate)
			if err == nil || origin != nil || channel != nil || human != nil {
				t.Fatalf("forged origin exposed context: origin=%#v channel=%#v human=%#v error=%v", origin, channel, human, err)
			}
		})
	}
	t.Run("canonical-agent-message-is-not-a-human-trigger", func(t *testing.T) {
		conversationID := work.Context[conversationRunContextConversationID].(string)
		conversation, err := store.GetConversation(t.Context(), work.Scope, conversationID)
		if err != nil {
			t.Fatal(err)
		}
		posted, err := NewConversationService(store).PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: work.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: work.AssignedAgentID}, Intent: MessageIntentUpdate, Content: "Agent-authored text cannot become the pinned human request.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "agent-origin"})
		if err != nil {
			t.Fatal(err)
		}
		candidate := cloneAgentRun(work)
		candidate.Context[conversationRunContextTriggerID] = posted.Message.ID
		origin, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: work.Scope, Kind: RunKindConversation, Owner: work.Owner, AssignedAgentID: work.AssignedAgentID, Goal: "Handle the agent update", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: posted.Message.ID}})
		if err != nil {
			t.Fatal(err)
		}
		candidate.Context["workflowSourceRunId"] = origin.ID
		if _, _, _, err := conversationWorkOrigin(t.Context(), store, store, candidate); !errors.Is(err, ErrInvalidAgentRun) {
			t.Fatalf("non-human origin error=%v", err)
		}
	})
}
