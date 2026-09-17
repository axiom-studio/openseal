package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestDesktopLifecycleRequiresHostPolicyAndOwnerApproval(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "desktop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(governedAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	api.SetWorkforceLifecycleAuthorizer(DesktopLifecycleAuthorizer{Scope: scope})
	created, _, err := api.authoringChanges.Create(context.Background(), authoring.CreateChangeSetRequest{
		Scope: scope, Prompt: "Create research agents", Catalog: authoring.CapabilityCatalog{},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "research-live", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}, Environment: "local"},
		Actor:     authoring.ChangeSetActor{Type: "user", ID: "local-operator"}, IdempotencyKey: "create",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/authoring/workforce/change-sets/" + created.ID
	post := func(path string, request interface{}, key string, expected int) *authoring.ChangeSet {
		t.Helper()
		response := performAgentRunRequest(t, api.Handler(), http.MethodPost, base+path, mustJSON(t, request), key)
		if response.Code != expected {
			t.Fatalf("%s = %d %s", path, response.Code, response.Body.String())
		}
		if expected >= 400 {
			return nil
		}
		var result authoring.ChangeSet
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return &result
	}
	evaluation := authoring.SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: authoring.ChangeSetActor{Type: "policy_evaluator", ID: "forged"}}
	evaluated := post("/evaluations", evaluation, "evaluate", http.StatusCreated)
	if evaluated.Status != authoring.ChangeSetAwaitingApproval || len(evaluated.Evaluations) != 1 {
		t.Fatalf("evaluation skipped approval: %#v", evaluated)
	}
	policy := evaluated.Evaluations[0]
	if policy.Actor.ID != "local-desktop-policy" || len(policy.ApprovalRequirements) != 1 || policy.ApprovalRequirements[0].Role != desktopOwnerRole {
		t.Fatalf("client controlled policy: %#v", policy)
	}
	// An altered renderer policy body cannot remove the host's requirement, including on replay.
	evaluation.Allowed = false
	evaluation.Findings = []authoring.ChangeSetPolicyFinding{{PolicyID: "forged", Code: "deny", Message: "client supplied"}}
	replay := post("/evaluations", evaluation, "evaluate", http.StatusOK)
	if replay.Revision != evaluated.Revision {
		t.Fatal("evaluation replay mutated state")
	}
	apply := authoring.ApplyChangeSetRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: evaluated.Revision, CandidateDigest: created.CandidateDigest, Reason: "Install reviewed proposal"}
	post("/apply", apply, "premature", http.StatusForbidden)
	approval := authoring.ResolveChangeSetApprovalRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: evaluated.Revision, EvaluationID: policy.ID, PolicyID: desktopInstallPolicy, Role: "forged-admin", Approved: true, Reason: "Reviewed the exact proposal", Actor: authoring.ChangeSetActor{Type: "user", ID: "forged"}}
	post("/approvals", approval, "forged", http.StatusForbidden)
	approval.Role = desktopOwnerRole
	approved := post("/approvals", approval, "approve", http.StatusCreated)
	if approved.Status != authoring.ChangeSetReady || approved.ApprovalDecisions[0].Actor.ID != "local-operator" {
		t.Fatalf("approval not governed: %#v", approved)
	}
	replay = post("/approvals", approval, "approve", http.StatusOK)
	if replay.Revision != approved.Revision {
		t.Fatal("approval replay mutated state")
	}
	changedApproval := approval
	changedApproval.Approved = false
	post("/approvals", changedApproval, "approve", http.StatusConflict)
	post("/approvals", approval, "second-approval", http.StatusForbidden)
	apply.ExpectedRevision = approved.Revision
	apply.CandidateDigest = "stale-digest"
	post("/apply", apply, "stale", http.StatusConflict)
	apply.CandidateDigest = created.CandidateDigest
	applied := post("/apply", apply, "apply", http.StatusCreated)
	if applied.ApplyReceipt == nil || applied.Status != authoring.ChangeSetApplied || len(applied.ApplyReceipt.Resources) < 2 {
		t.Fatalf("installation failed: %#v", applied)
	}
	replay = post("/apply", apply, "apply", http.StatusOK)
	if replay.ApplyReceipt.ID != applied.ApplyReceipt.ID {
		t.Fatal("apply replay created another installation")
	}
}

func TestDesktopAuthorityRejectsForeignScopesAndUnrecognizedOperations(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	authority := DesktopLifecycleAuthorizer{Scope: scope}
	for _, candidate := range []*authoring.ChangeSet{nil, {Scope: capability.ScopeReference{Kind: "tenant", ID: "default"}}, {Scope: capability.ScopeReference{Kind: "local", ID: "other"}}} {
		if _, err := authority.AuthorizeWorkforceLifecycle(context.Background(), kernelapi.OperationApply, candidate); err == nil {
			t.Fatal("foreign scope authorized")
		}
	}
	if _, err := authority.AuthorizeWorkforceLifecycle(context.Background(), "invented", &authoring.ChangeSet{Scope: scope}); err == nil {
		t.Fatal("unknown operation authorized")
	}
}

func TestDesktopOwnerApprovalDoesNotBypassReadiness(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "not-ready.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(governedAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	api.SetWorkforceLifecycleAuthorizer(DesktopLifecycleAuthorizer{Scope: scope})
	created, _, err := api.authoringChanges.Create(context.Background(), authoring.CreateChangeSetRequest{Scope: scope, Prompt: "Create research agents without deployment placement", Catalog: authoring.CapabilityCatalog{}, Actor: authoring.ChangeSetActor{Type: "user", ID: "local-operator"}, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	// Creation deliberately fills safe default deployment identities. Remove a
	// required field from persisted placement to exercise apply's final check,
	// rather than assuming omitted request placement remains empty.
	previousRevision := created.Revision
	created.Placement.Environment = ""
	created.Revision++
	created, err = store.UpdateChangeSet(context.Background(), created, previousRevision)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/authoring/workforce/change-sets/" + created.ID
	evaluation := performAgentRunRequest(t, api.Handler(), http.MethodPost, base+"/evaluations", mustJSON(t, authoring.SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true}), "evaluate")
	var evaluated authoring.ChangeSet
	if evaluation.Code != http.StatusCreated || json.NewDecoder(evaluation.Body).Decode(&evaluated) != nil {
		t.Fatalf("evaluation=%d %s", evaluation.Code, evaluation.Body.String())
	}
	decision := performAgentRunRequest(t, api.Handler(), http.MethodPost, base+"/approvals", mustJSON(t, authoring.ResolveChangeSetApprovalRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: evaluated.Revision, EvaluationID: evaluated.Evaluations[0].ID, PolicyID: desktopInstallPolicy, Role: desktopOwnerRole, Approved: true, Reason: "Review completed"}), "approve")
	var approved authoring.ChangeSet
	if decision.Code != http.StatusCreated || json.NewDecoder(decision.Body).Decode(&approved) != nil {
		t.Fatalf("approval=%d %s", decision.Code, decision.Body.String())
	}
	response := performAgentRunRequest(t, api.Handler(), http.MethodPost, base+"/apply", mustJSON(t, authoring.ApplyChangeSetRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: approved.Revision, CandidateDigest: created.CandidateDigest, Reason: "Attempt installation"}), "apply")
	if response.Code < 400 {
		t.Fatalf("incomplete placement applied: %d %s", response.Code, response.Body.String())
	}
	persisted, err := store.GetChangeSet(context.Background(), scope, created.ID)
	if err != nil || persisted.ApplyReceipt != nil {
		t.Fatalf("failed readiness wrote receipt: %#v %v", persisted, err)
	}
}

func TestDesktopCannotApplyReadyProposalWithoutItsOwnReview(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	authority := DesktopLifecycleAuthorizer{Scope: scope}
	proposal := &authoring.ChangeSet{Scope: scope, Status: authoring.ChangeSetReady, CandidateDigest: "candidate", Evaluations: []authoring.ChangeSetEvaluation{{ID: "foreign", CandidateDigest: "candidate", Allowed: true, Actor: authoring.ChangeSetActor{Type: "policy_evaluator", ID: "another-host"}}}}
	if _, err := authority.AuthorizeWorkforceLifecycle(context.Background(), kernelapi.OperationApply, proposal); err == nil {
		t.Fatal("foreign policy readiness replaced owner review")
	}
	proposal.Evaluations[0].Actor.ID = "local-desktop-policy"
	if _, err := authority.AuthorizeWorkforceLifecycle(context.Background(), kernelapi.OperationApply, proposal); err == nil {
		t.Fatal("policy without owner decision authorized apply")
	}
}

func TestDesktopActionAuthorityPreservesCheckpointEligibility(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	host := DesktopApprovalAuthorizer{Scope: scope}
	principal := runtime.ApprovalPrincipal{Type: "user", ID: "local-operator"}
	for _, eligible := range []runtime.ApprovalPrincipal{principal, {Type: "role", ID: "operator"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &runtime.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []runtime.ApprovalPrincipal{eligible}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []runtime.ApprovalPrincipal{{Type: "user", ID: "alice"}, {Type: "role", ID: "admin"}} {
		if err := host.AuthorizeApproval(t.Context(), principal, &runtime.ApprovalCheckpoint{Scope: scope, EligibleApprovers: []runtime.ApprovalPrincipal{other}}); err == nil {
			t.Fatal("accepted unrelated eligible reviewer")
		}
		if err := host.ValidateApprovalRequest(scope, other); err == nil {
			t.Fatal("accepted impersonated reviewer")
		}
	}
	if err := host.ValidateApprovalRequest(runtime.Scope{Kind: "local", ID: "other"}, principal); err == nil {
		t.Fatal("accepted foreign scope")
	}
}
