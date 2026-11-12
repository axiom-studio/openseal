package conversationtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// RunsByID is a Run reader over the Runs a test puts in it, for code that
// only reads Runs.
type RunsByID map[string]*models.AgentRun

func (r RunsByID) GetAgentRun(_ context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	run, ok := r[runID]
	if !ok || run.Scope != scope {
		return nil, models.ErrRunNotFound
	}
	return models.CloneAgentRun(run), nil
}

// ApprovalModeActor is the person who changes approval modes in tests.
var ApprovalModeActor = models.ActivityActor{Type: "user", ID: "user-7"}

// ApprovalModeConversation creates conversation "chat" of agent assistant
// in tenant one.
func ApprovalModeConversation(t *testing.T, store *sqlite.Store) *models.Conversation {
	return ApprovalModeConversationWithKey(t, store, "chat")
}

// ApprovalModeConversationWithKey creates conversation key of agent
// assistant in tenant one.
func ApprovalModeConversationWithKey(t *testing.T, store *sqlite.Store, key string) *models.Conversation {
	t.Helper()
	SeedSpace(t, store, models.Scope{Kind: "tenant", ID: "one"})
	created, _, err := conversation.NewService(store).CreateConversation(context.Background(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"}, Title: "Chat " + key, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// SetApprovalMode sets the approval mode of target at its revision as
// ApprovalModeActor.
func SetApprovalMode(t *testing.T, service *conversation.ApprovalModeService, target *models.Conversation, mode models.ConversationApprovalMode) *conversation.SetConversationApprovalModeResult {
	t.Helper()
	result, err := service.SetApprovalMode(context.Background(), conversation.SetConversationApprovalModeRequest{
		Scope: target.Scope, ConversationID: target.ID, ExpectedRevision: target.Revision, Mode: mode, Actor: ApprovalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// ApprovalModeCatalog binds assistant in tenant one to a workspace Skill
// with a write (note), an external (send) and an always-reviewed (pay)
// action.
func ApprovalModeCatalog(t *testing.T) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	catalog := skill.NewCatalog()
	object := map[string]interface{}{"type": "object"}
	definition := &skill.Definition{
		ID: "workspace", Version: "1.0.0", Name: "Workspace", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://workspace.invalid"},
		Actions: map[string]skill.Action{
			"note": {Name: "note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
			"send": {Name: "send", Description: "Send a message", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional},
			"pay": {Name: "pay", Description: "Pay for the checkout", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden, InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"amount": map[string]interface{}{"type": "string"}, "currency": map[string]interface{}{"type": "string"},
						"merchant": map[string]interface{}{"type": "string"}, "cardToken": map[string]interface{}{"type": "string", "x-sensitive": true},
					},
				}},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "workspace-binding", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "assistant",
		SkillID: "workspace", SkillVersion: "1.0.0", AllowedActions: []string{"note", "send", "pay"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// approvalModeRuns numbers the Runs ProposeForApprovalMode creates.
var approvalModeRuns int

// ProposeForApprovalMode proposes action in a new Run of target and parks
// the Run on its approval.
func ProposeForApprovalMode(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.Coordinator, target *models.Conversation, action string) *models.ApprovalCheckpoint {
	t.Helper()
	approvalModeRuns++
	return ProposeForApprovalModeInConversation(t, store, coordinator, target.ID, action, fmt.Sprintf("run-%s-%d", action, approvalModeRuns))
}

// ProposeForApprovalModeInConversation proposes action as a loop step of a
// new Run of the conversation and parks the Run on its approval, as the loop
// does at step end.
func ProposeForApprovalModeInConversation(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.Coordinator, conversationID, action, key string) *models.ApprovalCheckpoint {
	t.Helper()
	result, f, err := ProposeLoopForApprovalMode(t, store, coordinator, conversationID, action, key, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval == nil {
		t.Fatalf("%s proposal did not wait for approval: %#v", action, result.Call)
	}
	f.AssertUntouched()
	f.ParkOnApproval(result.Approval.ID)
	return result.Approval
}

// ProposeLoopForApprovalMode proposes action as tool call key of a loop step
// of a new Run acting for conversationID.
func ProposeLoopForApprovalMode(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.Coordinator, conversationID, action, key string, arguments map[string]interface{}) (*models.ActionProposalResult, *actiontest.LoopProposalFixture, error) {
	t.Helper()
	return ProposeLoopForApprovalModeWithContext(t, store, coordinator, map[string]interface{}{"conversationId": conversationID}, action, key, arguments)
}

// ProposeLoopForApprovalModeWithContext is ProposeLoopForApprovalMode for a
// new Run with this Context.
func ProposeLoopForApprovalModeWithContext(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.Coordinator, runContext map[string]interface{}, action, key string, arguments map[string]interface{}) (*models.ActionProposalResult, *actiontest.LoopProposalFixture, error) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	runs := portfolio.NewService(store, models.WithClock(func() time.Time { return now }))
	run, err := runs.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"}, AssignedAgentID: "assistant", Goal: key, Source: models.RunSourceObjective,
		Context: runContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := actiontest.NewLoopProposalFixture(t, store, run, "worker", now)
	result, err := coordinator.Propose(ctx, f.Request(runtimeaction.ProposeActionRequest{
		DeploymentID: "assistant", SkillID: "workspace", SkillVersion: "1.0.0", Action: action, Arguments: arguments,
		Summary: "Run " + action + " for " + key,
	}, key))
	return result, f, err
}
