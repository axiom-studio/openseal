package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestWorkspacePermissionsRequireApprovalAndPreserveAgent(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	store := NewMemoryStore()
	agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, AllowedActions: []string{AgentActionListWorkspace, AgentActionConfigureWorkspace}, MaximumRisk: skill.RiskLevelWrite}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, &skill.Definition{ID: "github", Version: "1.0.0", Name: "GitHub", Transport: skill.TransportReference{Kind: "tool", Endpoint: "github"}, Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read repository", InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "github-connection", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", SkillID: "github", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Credentials: map[string]capability.CredentialReference{"token": {Kind: "github_token", ID: "vault:own"}}}); err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Goal: "Enable repository access", Source: RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	validator, _ := NewAgentBehaviorActionValidator(agents)
	validator.SetWorkspaceCatalog(catalog)
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour}, nil
	})
	args := map[string]interface{}{"gitEnabled": true, "pushEnabled": true, "repositories": []interface{}{"github.com/team/project"}, "connectionBindingId": "github-connection", "connectionField": "token", "rationale": "Publish changes requested by the user"}
	proposal, err := NewActionCoordinator(store, store, catalog, policy, validator).Propose(ctx, ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher", SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, Action: AgentActionConfigureWorkspace, Arguments: args, IdempotencyKey: "grant-git", Summary: "Enable repository-scoped push"})
	if err != nil {
		t.Fatal(err)
	}
	dep, _ := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if dep.Workspaces[0].Policy.Git.Enabled || proposal.Approval == nil {
		t.Fatal("proposal granted Git without review")
	}
	dispatcher, _ := NewAgentBehaviorActionDispatcher(store, agents, nil)
	dispatcher.SetWorkspaceCatalog(catalog)
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher", AgentManagementSkillID, AgentManagementSkillVersion, AgentActionConfigureWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: proposal.Call, Bound: bound, Arguments: proposal.Call.Arguments}); err == nil {
		t.Fatal("unapproved workspace mutation succeeded")
	}
	resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "grant", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	input := ActionDispatchInput{Call: resolved.Call, Bound: bound, Arguments: resolved.Call.Arguments}
	result, err := dispatcher.DispatchAction(ctx, input)
	if err != nil || result["replayed"] != false {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	dep, _ = agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if dep.Revision != 2 || dep.ActiveVersion != "1.0.0" || dep.Workspaces[0].Storage.Capacity != workspace.DefaultStorageCapacity || !dep.Workspaces[0].Policy.Git.PushEnabled || dep.Workspaces[0].Policy.Commands.Enabled {
		t.Fatalf("unexpected deployment %#v", dep)
	}
	result, err = dispatcher.DispatchAction(ctx, input)
	if err != nil || result["replayed"] != true {
		t.Fatalf("replay=%#v err=%v", result, err)
	}
	bad := map[string]interface{}{"expectedDeploymentRevision": int64(2), "expectedConnectionRevision": int64(1), "gitEnabled": true, "pushEnabled": true, "repositories": []interface{}{"github.com/other/project"}, "connectionBindingId": "foreign", "connectionField": "token", "rationale": "Try foreign binding"}
	if _, _, _, err := resolveWorkspaceAction(ctx, agents, catalog, run, bad); err == nil {
		t.Fatal("foreign binding accepted")
	}
	bad["connectionBindingId"] = "github-connection"
	bad["expectedConnectionRevision"] = int64(2)
	if _, _, _, err := resolveWorkspaceAction(ctx, agents, catalog, run, bad); err == nil {
		t.Fatal("stale connection revision accepted")
	}
	bad["expectedConnectionRevision"] = int64(1)
	bad["repositories"] = []interface{}{"github.com/*"}
	if _, _, _, err := resolveWorkspaceAction(ctx, agents, catalog, run, bad); err == nil {
		t.Fatal("wildcard repository authority accepted")
	}
	bad["repositories"] = []interface{}{}
	bad["repositoryAccess"] = "credential"
	_, accountAccess, _, err := resolveWorkspaceAction(ctx, agents, catalog, run, bad)
	if err != nil || accountAccess.Workspaces[0].Policy.Git.RepositoryAccess != "credential" || len(accountAccess.Workspaces[0].Policy.Git.AllowedRepositories) != 0 {
		t.Fatalf("account repository access=%#v err=%v", accountAccess, err)
	}
	delete(bad, "repositoryAccess")
	bad["gitEnabled"] = false
	bad["pushEnabled"] = false
	bad["repositories"] = []interface{}{}
	delete(bad, "connectionBindingId")
	delete(bad, "connectionField")
	_, revoked, _, err := resolveWorkspaceAction(ctx, agents, catalog, run, bad)
	if err != nil || revoked.Workspaces[0].Policy.Git.Enabled || containsWorkspaceString(revoked.Workspaces[0].Policy.CredentialBindings, workspaceGitCredentialBinding) {
		t.Fatalf("revoke=%#v err=%v", revoked, err)
	}
}
