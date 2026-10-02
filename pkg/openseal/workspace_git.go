package openseal

import (
	"context"
	"errors"
	"reflect"
	"sort"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// ReconcileWorkspaceGitConnection applies the connected-account default only
// to unconfigured Git state, and refreshes or revokes an already managed
// connection. Explicit repository limits and opt-outs are retained. The caller
// must authorize account attachment; this method resolves the exact binding
// again within the deployment scope and stores an ordinary revisioned audit.
func (e *Engine) ReconcileWorkspaceGitConnection(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, actorType, actorID string) (*kernelagent.AgentDeployment, error) {
	if e == nil || e.skills == nil || e.agents == nil {
		return nil, errors.New("workspace connection runtime is unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		binding, err := e.skills.GetBinding(ctx, scope, deploymentID, bindingID)
		if err != nil || binding == nil {
			return nil, err
		}
		dep, err := e.agents.GetDeployment(ctx, scope, deploymentID)
		if err != nil {
			return nil, err
		}
		if dep.RolloutStatus == kernelagent.RolloutRetired {
			return nil, nil
		}
		changed, err := applyWorkspaceGitConnection(dep, binding)
		if err != nil || !changed {
			return nil, err
		}
		updated, _, err := e.agents.UpdateDeployment(ctx, dep, dep.Revision, actorType, actorID, "Reconcile workspace Git access with its connected account")
		if errors.Is(err, kernelagent.ErrRevisionConflict) {
			continue
		}
		return updated, err
	}
	return nil, kernelagent.ErrRevisionConflict
}

func applyWorkspaceGitConnection(dep *kernelagent.AgentDeployment, binding *skill.Binding) (bool, error) {
	if dep == nil || binding == nil || dep.ID != binding.DeploymentID || dep.Scope != binding.Scope {
		return false, errors.New("workspace connection must belong to this Agent and scope")
	}
	kernelagent.EnsureDefaultWorkspace(dep)
	var spec *workspace.Spec
	for i := range dep.Workspaces {
		if dep.Workspaces[i].ID == dep.DefaultWorkspaceID {
			spec = &dep.Workspaces[i]
			break
		}
	}
	if spec == nil {
		return false, errors.New("default workspace is unavailable")
	}
	git := spec.Policy.Git
	if git.RepositoryAccess == "disabled" || (git.Enabled && git.ConnectionBindingID != binding.ID) {
		return false, nil
	}
	keys := []string{}
	for key, ref := range binding.Credentials {
		if ref.Kind == "github_token" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if !binding.Disabled && len(keys) == 0 && git.ConnectionBindingID != binding.ID {
		return false, nil
	}
	if len(keys) > 1 {
		return false, errors.New("choose one Git connection field before configuring workspace access")
	}
	before := *spec
	const managedKey = "WORKSPACE_GIT"
	if binding.Disabled || len(keys) == 0 {
		if git.ConnectionBindingID != binding.ID {
			return false, nil
		}
		spec.Policy.Git = workspace.GitPolicy{RepositoryAccess: "disabled"}
		retained := []string{}
		for _, key := range spec.Policy.CredentialBindings {
			if key != managedKey {
				retained = append(retained, key)
			}
		}
		spec.Policy.CredentialBindings = retained
	} else {
		ref := binding.Credentials[keys[0]]
		if dep.Credentials == nil {
			dep.Credentials = map[string]capability.CredentialReference{}
		}
		oldRef := dep.Credentials[managedKey]
		dep.Credentials[managedKey] = ref
		if !git.Enabled {
			git = workspace.GitPolicy{RepositoryAccess: "credential", Enabled: true, PushEnabled: true, AllowedHosts: []string{"github.com"}, MaxDurationSeconds: 300}
		}
		git.ConnectionBindingID = binding.ID
		git.CredentialBinding = managedKey
		git.CredentialKind = ref.Kind
		spec.Policy.Git = git
		spec.Policy.Filesystem = workspace.AccessReadWrite
		found := false
		for _, key := range spec.Policy.CredentialBindings {
			found = found || key == managedKey
		}
		if !found {
			spec.Policy.CredentialBindings = append(spec.Policy.CredentialBindings, managedKey)
		}
		if oldRef != ref {
			return true, dep.Validate()
		}
	}
	return !reflect.DeepEqual(before, *spec), dep.Validate()
}
