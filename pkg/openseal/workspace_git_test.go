package openseal

import (
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestConnectedGitHubDefaultsToAccountRepositoriesAndPreservesOverrides(t *testing.T) {
	dep := &kernelagent.AgentDeployment{ID: "agent", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, DefinitionID: "definition", ActiveVersion: "1.0.0", RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1}
	b := &skill.Binding{ID: "github", Scope: dep.Scope, DeploymentID: dep.ID, Credentials: map[string]capability.CredentialReference{"token": {Kind: "github_token", ID: "own-reference"}}}
	changed, err := applyWorkspaceGitConnection(dep, b)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	git := dep.Workspaces[0].Policy.Git
	if !git.Enabled || !git.PushEnabled || git.RepositoryAccess != "credential" || len(git.AllowedRepositories) != 0 || dep.Workspaces[0].Policy.Commands.Enabled {
		t.Fatalf("default=%#v", git)
	}
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || changed {
		t.Fatalf("idempotent=%v %v", changed, err)
	}
	dep.Workspaces[0].Policy.Git.RepositoryAccess = "selected"
	dep.Workspaces[0].Policy.Git.AllowedRepositories = []string{"github.com/team/project"}
	dep.Workspaces[0].Policy.Git.PushEnabled = false
	b.Credentials["token"] = capability.CredentialReference{Kind: "github_token", ID: "replacement"}
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || !changed {
		t.Fatalf("replacement=%v %v", changed, err)
	}
	if dep.Workspaces[0].Policy.Git.PushEnabled || dep.Workspaces[0].Policy.Git.RepositoryAccess != "selected" || dep.Credentials["WORKSPACE_GIT"].ID != "replacement" {
		t.Fatal("account replacement broadened permissions")
	}
	foreign := *b
	foreign.Scope.ID = "other"
	if _, err := applyWorkspaceGitConnection(dep, &foreign); err == nil {
		t.Fatal("foreign tenant connection accepted")
	}
	delete(b.Credentials, "token")
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || !changed || dep.Workspaces[0].Policy.Git.Enabled || len(dep.Workspaces[0].Policy.CredentialBindings) != 0 {
		t.Fatalf("credential removal did not revoke access: changed=%v err=%v", changed, err)
	}
	dep.Workspaces[0].Policy.Git = workspace.GitPolicy{}
	b.Credentials["token"] = capability.CredentialReference{Kind: "github_token", ID: "replacement"}
	if _, err := applyWorkspaceGitConnection(dep, b); err != nil {
		t.Fatal(err)
	}
	b.Disabled = true
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || !changed || dep.Workspaces[0].Policy.Git.Enabled {
		t.Fatalf("disconnect=%v %v", changed, err)
	}
	b.Disabled = false
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || changed {
		t.Fatal("explicit opt-out was overridden")
	}
	dep.Workspaces[0].Policy.Git = workspace.GitPolicy{RepositoryAccess: "disabled"}
	if changed, err := applyWorkspaceGitConnection(dep, b); err != nil || changed {
		t.Fatal("explicit disabled mode was overridden")
	}
}
