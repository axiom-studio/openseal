package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

func TestActionApprovalAPIAndClientAreReadOnlyUntilAuthorityIsConfigured(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore()
	proposal := createActionApprovalFixture(t, store)
	api := NewServer(store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	kernel := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	document, err := kernel.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.ActionApprovalsCapabilityID, kernelapi.ActionApprovalsCapabilityVersion)
	expectedReadOnly := kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{})
	if !ok || !slices.Equal(capability.Operations, expectedReadOnly.Operations) || capability.Supports(kernelapi.OperationResolve) {
		t.Fatalf("read-only approval capability = %#v", capability)
	}
	actionCapability, ok := document.Find(kernelapi.ActionCallsCapabilityID, kernelapi.ActionCallsCapabilityVersion)
	if !ok || !actionCapability.Supports(kernelapi.OperationGet) || !actionCapability.Supports(kernelapi.OperationList) {
		t.Fatalf("action call capability = %#v", actionCapability)
	}
	calls, err := kernel.ListActionCalls(t.Context(), runtime.ActionFilter{
		Scope: proposal.Call.Scope, RunID: proposal.Call.RunID, Status: []runtime.ActionCallStatus{runtime.ActionCallStatusWaitingApproval}, Limit: 25,
	})
	if err != nil || len(calls) != 1 || calls[0].ID != proposal.Call.ID || calls[0].InvocationDigest == "" {
		t.Fatalf("listed action calls = %#v, %v", calls, err)
	}
	call, err := kernel.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
	if err != nil || call.ID != proposal.Call.ID || call.Revision != proposal.Call.Revision {
		t.Fatalf("restored action call = %#v, %v", call, err)
	}
	_, err = kernel.GetActionCall(t.Context(), runtime.Scope{Kind: "local", ID: "other"}, proposal.Call.ID)
	var apiError *client.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 404 {
		t.Fatalf("cross-scope action call error = %#v", err)
	}
	listed, err := kernel.ListActionApprovals(t.Context(), runtime.ApprovalFilter{
		Scope: proposal.Approval.Scope, Owner: &proposal.Run.Owner, Status: []runtime.ApprovalStatus{runtime.ApprovalStatusPending}, Limit: 25,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != proposal.Approval.ID {
		t.Fatalf("listed approvals = %#v, %v", listed, err)
	}
	restored, err := kernel.GetActionApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID)
	if err != nil || restored.Revision != proposal.Approval.Revision {
		t.Fatalf("restored approval = %#v, %v", restored, err)
	}
	_, err = kernel.ResolveActionApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, kernelapi.ResolveActionApprovalRequest{
		ExpectedRevision: proposal.Approval.Revision, Decision: runtime.ApprovalDecisionApprove, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "alice"},
	}, "decision-1")
	if !errors.As(err, &apiError) || apiError.StatusCode != 501 {
		t.Fatalf("unconfigured resolution error = %#v", err)
	}

	api.SetActionApprovalAuthorizer(runtime.ApprovalAuthorizerFunc(func(_ context.Context, principal runtime.ApprovalPrincipal, approval *runtime.ApprovalCheckpoint) error {
		if principal.ID != "alice" || approval.ID != proposal.Approval.ID {
			return errors.New("not authorized")
		}
		return nil
	}))
	document, err = kernel.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capability, ok = document.Find(kernelapi.ActionApprovalsCapabilityID, kernelapi.ActionApprovalsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationResolve) {
		t.Fatalf("governed approval capability = %#v", capability)
	}
	_, err = kernel.ResolveActionApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, kernelapi.ResolveActionApprovalRequest{
		ExpectedRevision: proposal.Approval.Revision, Decision: runtime.ApprovalDecisionApprove, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "mallory"},
	}, "unauthorized-decision")
	if !errors.As(err, &apiError) || apiError.StatusCode != 403 {
		t.Fatalf("unauthorized decision error = %#v", err)
	}
	resolved, err := kernel.ResolveActionApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, kernelapi.ResolveActionApprovalRequest{
		ExpectedRevision: proposal.Approval.Revision, Decision: runtime.ApprovalDecisionApprove, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "reviewed",
	}, "decision-1")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Resolved || resolved.Approval.Status != runtime.ApprovalStatusApproved || resolved.Call.Status != runtime.ActionCallStatusReady || resolved.Run.Status != runtime.AgentRunStatusWaitingForDependency {
		t.Fatalf("resolved approval = %#v", resolved)
	}
	replayed, err := kernel.ResolveActionApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, kernelapi.ResolveActionApprovalRequest{
		ExpectedRevision: proposal.Approval.Revision, Decision: runtime.ApprovalDecisionApprove, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "alice"},
	}, "decision-1")
	if err != nil || replayed.Resolved {
		t.Fatalf("replayed decision = %#v, %v", replayed, err)
	}
}

func createActionApprovalFixture(t *testing.T, store *runtime.MemoryStore) *runtime.ActionProposalResult {
	t.Helper()
	now := time.Now().UTC()
	scope := runtime.Scope{Kind: "local", ID: "workspace"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator",
		Goal: "Publish release", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	approval := &runtime.ApprovalCheckpoint{
		ID: "approval-publish", Scope: scope, RunID: run.ID, ActionCallID: "call-publish", Status: runtime.ApprovalStatusPending,
		Risk: skill.RiskLevelProduction, Summary: "Publish release announcement", ProposedAction: map[string]interface{}{"channel": "public"},
		EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "user", ID: "alice"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call := &runtime.ActionCall{
		ID: "call-publish", Scope: scope, RunID: run.ID, DeploymentID: "operator", SkillID: "publishing", SkillVersion: "1", Action: "publish",
		Status: runtime.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal,
		IdempotencyKey: "publish-release", ApprovalID: approval.ID, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	proposalRun := *run
	proposalRun.Status = runtime.AgentRunStatusWaitingForApproval
	proposalRun.WakeCondition = &runtime.WakeCondition{Type: "approval", Reference: approval.ID}
	proposalRun.Revision++
	proposalRun.UpdatedAt = now
	result, err := store.CreateActionProposal(t.Context(), runtime.ActionProposalRecord{
		Call: call, Approval: approval, Run: &proposalRun, ExpectedRunRevision: run.Revision,
		Event: &runtime.ActivityEvent{ID: "event-approval", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested", CreatedAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
