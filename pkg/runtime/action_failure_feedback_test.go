package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// approvedFailedRun approves a governed external action and fails its
// execution with cause, as in Run c7a9efec: an approved Slack post failed with
// channel_not_found.
func approvedFailedRun(t *testing.T, store KernelStore, now time.Time, cause error) (*skill.Catalog, *ActionProposalResult, *ActionExecutionResult) {
	t.Helper()
	proposal := createApprovalForStore(t, store, now)
	approvals := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	approvals.now = func() time.Time { return now.Add(2 * time.Second) }
	if _, err := approvals.Resolve(t.Context(), ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "approve", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, _ := governedActionCatalog(t)
	dispatches := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return nil, cause
	}))
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	failed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
	if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || dispatches != 1 {
		t.Fatalf("approved action did not fail once: %#v %v dispatches=%d", failed, err, dispatches)
	}
	return catalog, proposal, failed
}

func releaseModelActions() []capability.ModelAction {
	return []capability.ModelAction{{
		Name: "release.deploy", SkillID: "release", Version: "1.0.0", Action: "deploy", DeploymentID: "release-agent",
		BindingID: "release-binding", BindingRevision: 1, SideEffect: capability.SideEffectExternal,
		ExternalOperationPolicy: capability.ExternalOperationForbidden,
	}}
}
