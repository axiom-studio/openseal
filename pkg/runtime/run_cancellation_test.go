package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type cancellationStore interface {
	KernelStore
}

func TestCancelWaitingApprovalClosesAction(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) cancellationStore
	}{
		{name: "sqlite", store: func(t *testing.T) cancellationStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "cancel.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			assertCanceledApproval(t, fixture.store(t))
		})
	}
}

func TestCancelParentClosesDescendantApprovalAndAction(t *testing.T) {
	ctx := t.Context()
	store := newTestKernelStore(t)
	catalog, scope := cancellationActionCatalog(t)
	runs := agentrun.NewRunCommandService(store)
	root, err := runs.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "research-agent"},
		AssignedAgentID: "research-agent", Goal: "coordinate reviewed work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := runs.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ParentRunID: root.Run.ID, Owner: root.Run.Owner,
		AssignedAgentID: "research-agent", Goal: "perform reviewed action", Source: RunSourceHandoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop := actiontest.NewLoopProposalFixture(t, store, child.Run, "worker", time.Now().UTC())
	coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{
			Disposition: action.ActionDispositionRequireApproval, Reason: "review required", ApprovalTTL: time.Hour,
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "reviewer"}},
		}, nil
	}), nil)
	proposal, err := coordinator.Propose(ctx, loop.Request(action.ProposeActionRequest{
		DeploymentID: "research-agent", SkillID: "forum", SkillVersion: "1.0.0",
		Action: "reply", Arguments: map[string]interface{}{"body": "reviewed body"}, IdempotencyKey: "child-action",
		Summary: "Send reviewed reply",
	}, "reply"))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal = %#v", proposal)
	}
	loop.ParkOnApproval(proposal.Approval.ID)

	if _, err := runs.CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: scope, RunID: root.Run.ID, ExpectedRevision: root.Run.Revision,
		Kind: agentrun.AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "operator"},
	}); err != nil {
		t.Fatal(err)
	}
	resolvedChild, err := store.GetAgentRun(ctx, scope, child.Run.ID)
	if err != nil || resolvedChild.Status != AgentRunStatusCanceled {
		t.Fatalf("child = %#v err=%v", resolvedChild, err)
	}
	call, err := store.GetActionCall(ctx, scope, proposal.Call.ID)
	if err != nil || call.Status != ActionCallStatusCanceled || call.LeaseOwner != "" {
		t.Fatalf("call = %#v err=%v", call, err)
	}
	approval, err := store.GetApproval(ctx, scope, proposal.Approval.ID)
	if err != nil || approval.Status != ApprovalStatusCanceled {
		t.Fatalf("approval = %#v err=%v", approval, err)
	}
}

func assertCanceledApproval(t *testing.T, store cancellationStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	catalog, scope := cancellationActionCatalog(t)
	runs := agentrun.NewRunCommandService(store)
	created, err := runs.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "research-agent",
		Goal: "Send reviewed follow-up", Source: RunSourceRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop := actiontest.NewLoopProposalFixture(t, store, created.Run, "worker", now)
	coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{
			Disposition: action.ActionDispositionRequireApproval, Reason: "human review is required", ApprovalTTL: time.Hour,
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "reviewer"}},
		}, nil
	}), nil, models.WithClock(func() time.Time { return now }))
	proposal, err := coordinator.Propose(ctx, loop.Request(action.ProposeActionRequest{
		DeploymentID: "research-agent", SkillID: "forum", SkillVersion: "1.0.0", Action: "reply",
		Arguments: map[string]interface{}{"body": "reviewed body"}, IdempotencyKey: "reply-1", Summary: "Send reviewed reply",
	}, "reply"))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal = %#v", proposal)
	}
	// The loop parks the Run on the approval at step end.
	loop.ParkOnApproval(proposal.Approval.ID)
	parked := loop.Current()

	result, err := runs.CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: scope, RunID: parked.ID, ExpectedRevision: parked.Revision, Kind: agentrun.AgentRunCommandCancel,
		Actor: ActivityActor{Type: "user", ID: "operator"}, Summary: "Operator canceled the reviewed reply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusCanceled || result.Event == nil || result.Event.EventType != "run.canceled" {
		t.Fatalf("cancellation = %#v", result)
	}
	call, err := store.GetActionCall(ctx, scope, proposal.Call.ID)
	if err != nil || call.Status != ActionCallStatusCanceled || call.CompletedAt == nil {
		t.Fatalf("call = %#v err=%v", call, err)
	}
	approval, err := store.GetApproval(ctx, scope, proposal.Approval.ID)
	if err != nil || approval.Status != ApprovalStatusCanceled || approval.DecisionBy == nil || approval.DecisionBy.ID != "operator" {
		t.Fatalf("approval = %#v err=%v", approval, err)
	}
	if _, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "late-approval", Decision: ApprovalDecisionApprove,
		Principal: ApprovalPrincipal{Type: "role", ID: "reviewer"},
	}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("late approval error = %v", err)
	}
}

func cancellationActionCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "cancellation"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "forum", Version: "1.0.0", Name: "Forum", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://forum.invalid"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Post a reviewed reply", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"body": map[string]interface{}{"type": "string"}}, "required": []interface{}{"body"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "forum-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}
