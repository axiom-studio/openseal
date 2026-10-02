package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

const AgentActionListWorkspace = "list_workspace"
const AgentActionConfigureWorkspace = "configure_workspace"
const workspaceGitCredentialBinding = "WORKSPACE_GIT"

func agentListWorkspaceAction() skill.Action {
	return skill.Action{Name: AgentActionListWorkspace, Description: "Read the current Agent's workspace Git permissions and available bound Git connections. Lists opaque binding names only, never tokens. Use before requesting clone, commit or push access.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false}, OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": true}}
}

func agentConfigureWorkspaceAction() skill.Action {
	return skill.Action{Name: AgentActionConfigureWorkspace, Description: "Propose repository-scoped Git clone, local commit and optional push access for the current Agent's default workspace. Use list_workspace first; copy an available connectionBindingId and connectionField exactly. Default repositoryAccess=credential enables every repository the account can access (repositories empty); selected requires explicit host/owner/repository names. The user reviews and approves this permission change. Request a GitHub Skill connection through normal Skill setup first if none exists. To revoke Git access set gitEnabled and pushEnabled false and repositories empty. Preserves storage and unrelated workspace settings.", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2}, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"expectedDeploymentRevision", "gitEnabled", "pushEnabled", "repositories", "rationale"}, "properties": map[string]interface{}{
		"expectedConnectionRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
		"expectedDeploymentRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
		"gitEnabled":                 map[string]interface{}{"type": "boolean"}, "pushEnabled": map[string]interface{}{"type": "boolean"},
		"repositoryAccess":    map[string]interface{}{"type": "string", "enum": []interface{}{"credential", "selected"}},
		"repositories":        map[string]interface{}{"type": "array", "maxItems": 64, "uniqueItems": true, "items": map[string]interface{}{"type": "string", "minLength": 1}},
		"connectionBindingId": map[string]interface{}{"type": "string", "minLength": 1}, "connectionField": map[string]interface{}{"type": "string", "minLength": 1},
		"rationale": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1000},
	}}, OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": true}}
}

func (v *AgentBehaviorActionValidator) SetWorkspaceCatalog(c *skill.Catalog)  { v.workspaceCatalog = c }
func (d *AgentBehaviorActionDispatcher) SetWorkspaceCatalog(c *skill.Catalog) { d.workspaceCatalog = c }
func isAgentConfigureWorkspaceAction(bound *skill.BoundAction) bool {
	return isAgentManagementAction(bound) && bound.Action.Name == AgentActionConfigureWorkspace
}

type workspaceActionArguments struct {
	ExpectedConnectionRevision int64    `json:"expectedConnectionRevision,omitempty"`
	ExpectedDeploymentRevision int64    `json:"expectedDeploymentRevision"`
	GitEnabled                 bool     `json:"gitEnabled"`
	PushEnabled                bool     `json:"pushEnabled"`
	RepositoryAccess           string   `json:"repositoryAccess,omitempty"`
	Repositories               []string `json:"repositories"`
	ConnectionBindingID        string   `json:"connectionBindingId,omitempty"`
	ConnectionField            string   `json:"connectionField,omitempty"`
	Rationale                  string   `json:"rationale"`
}

func listAgentWorkspace(ctx context.Context, agents *kernelagent.Registry, catalog *skill.Catalog, run *AgentRun) (map[string]interface{}, error) {
	id, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, err
	}
	dep, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id)
	if err != nil {
		return nil, err
	}
	kernelagent.EnsureDefaultWorkspace(dep)
	spec, err := workspace.Select(dep.DefaultWorkspaceID, dep.Workspaces)
	if err != nil {
		return nil, err
	}
	connections := []map[string]interface{}{}
	if catalog != nil {
		bindings, err := catalog.ListBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings {
			if b.Disabled {
				continue
			}
			for key, ref := range b.Credentials {
				if ref.Kind == "github_token" {
					connections = append(connections, map[string]interface{}{"bindingId": b.ID, "field": key, "skillId": b.SkillID})
				}
			}
		}
	}
	sort.Slice(connections, func(i, j int) bool {
		return connections[i]["bindingId"].(string)+":"+connections[i]["credentialName"].(string) < connections[j]["bindingId"].(string)+":"+connections[j]["credentialName"].(string)
	})
	return map[string]interface{}{"resourceType": "agent_workspace", "workspace": spec, "connections": connections, "expectedDeploymentRevision": dep.Revision}, nil
}

func resolveWorkspaceAction(ctx context.Context, agents *kernelagent.Registry, catalog *skill.Catalog, run *AgentRun, arguments map[string]interface{}) (*kernelagent.AgentDeployment, *kernelagent.AgentDeployment, workspaceActionArguments, error) {
	var a workspaceActionArguments
	if err := decodeAgentBehaviorArguments(arguments, &a); err != nil {
		return nil, nil, a, err
	}
	if a.ExpectedDeploymentRevision < 1 || strings.TrimSpace(a.Rationale) == "" || len(a.Rationale) > 1000 {
		return nil, nil, a, errors.New("workspace revision and rationale are required")
	}
	id, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, nil, a, err
	}
	dep, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id)
	if err != nil {
		return nil, nil, a, err
	}
	if dep.Revision != a.ExpectedDeploymentRevision {
		return nil, nil, a, kernelagent.ErrRevisionConflict
	}
	kernelagent.EnsureDefaultWorkspace(dep)
	encoded, _ := json.Marshal(dep)
	var next kernelagent.AgentDeployment
	_ = json.Unmarshal(encoded, &next)
	var selected *workspace.Spec
	for i := range next.Workspaces {
		if next.Workspaces[i].ID == next.DefaultWorkspaceID {
			selected = &next.Workspaces[i]
			break
		}
	}
	if selected == nil {
		return nil, nil, a, errors.New("default workspace is unavailable")
	}
	if !a.GitEnabled {
		if a.PushEnabled || len(a.Repositories) != 0 || a.ConnectionBindingID != "" || a.ConnectionField != "" {
			return nil, nil, a, errors.New("disabled Git cannot grant repository authority")
		}
		selected.Policy.Git = workspace.GitPolicy{RepositoryAccess: "disabled"}
		retained := []string{}
		for _, key := range selected.Policy.CredentialBindings {
			if key != workspaceGitCredentialBinding {
				retained = append(retained, key)
			}
		}
		selected.Policy.CredentialBindings = retained
	} else {
		if catalog == nil {
			return nil, nil, a, errors.New("workspace Git connection catalog is unavailable")
		}
		binding, err := catalog.GetBinding(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id, a.ConnectionBindingID)
		if err != nil || binding == nil || binding.Disabled {
			return nil, nil, a, errors.New("select an enabled Git connection bound to this Agent")
		}
		if binding.Revision != a.ExpectedConnectionRevision {
			return nil, nil, a, kernelagent.ErrRevisionConflict
		}
		ref, ok := binding.Credentials[a.ConnectionField]
		if !ok || ref.Kind != "github_token" {
			return nil, nil, a, errors.New("selected connection does not provide a GitHub token")
		}
		if next.Credentials == nil {
			next.Credentials = map[string]capability.CredentialReference{}
		}
		next.Credentials[workspaceGitCredentialBinding] = ref
		if !containsWorkspaceString(selected.Policy.CredentialBindings, workspaceGitCredentialBinding) {
			selected.Policy.CredentialBindings = append(selected.Policy.CredentialBindings, workspaceGitCredentialBinding)
		}
		if a.RepositoryAccess == "" {
			if len(a.Repositories) == 0 {
				a.RepositoryAccess = "credential"
			} else {
				a.RepositoryAccess = "selected"
			}
		}
		if a.RepositoryAccess != "selected" && a.RepositoryAccess != "credential" {
			return nil, nil, a, errors.New("invalid repository access mode")
		}
		if a.RepositoryAccess == "credential" && len(a.Repositories) != 0 {
			return nil, nil, a, errors.New("all-account access cannot include selected repositories")
		}
		hosts := []string{}
		for _, repo := range a.Repositories {
			host, _, ok := strings.Cut(repo, "/")
			if !ok {
				return nil, nil, a, errors.New("repositories must use host/owner/repository format")
			}
			if !containsWorkspaceString(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		if a.RepositoryAccess == "credential" {
			hosts = []string{"github.com"}
		}
		selected.Policy.Filesystem = workspace.AccessReadWrite
		selected.Policy.Git = workspace.GitPolicy{RepositoryAccess: a.RepositoryAccess, ConnectionBindingID: binding.ID, Enabled: true, PushEnabled: a.PushEnabled, CredentialBinding: workspaceGitCredentialBinding, CredentialKind: ref.Kind, AllowedHosts: hosts, AllowedRepositories: a.Repositories, MaxDurationSeconds: 300}
	}
	if err := next.Validate(); err != nil {
		return nil, nil, a, err
	}
	if reflect.DeepEqual(dep.Workspaces, next.Workspaces) && reflect.DeepEqual(dep.Credentials, next.Credentials) {
		return nil, nil, a, errors.New("workspace permissions are already configured")
	}
	return dep, &next, a, nil
}

func containsWorkspaceString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func workspaceActionProposal(ctx context.Context, agents *kernelagent.Registry, catalog *skill.Catalog, run *AgentRun, arguments map[string]interface{}) (map[string]interface{}, error) {
	current, next, a, err := resolveWorkspaceAction(ctx, agents, catalog, run, arguments)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"resourceType": "agent_workspace", "operation": AgentActionConfigureWorkspace, "deploymentId": next.ID, "expectedDeploymentRevision": current.Revision, "current": workspaceGitPermissionPreview(current), "changes": workspaceGitPermissionPreview(next), "rationale": a.Rationale}, nil
}

func workspaceGitPermissionPreview(dep *kernelagent.AgentDeployment) map[string]interface{} {
	for _, spec := range dep.Workspaces {
		if spec.ID != dep.DefaultWorkspaceID {
			continue
		}
		git := spec.Policy.Git
		repositories := "None"
		if git.Enabled && git.RepositoryAccess == "credential" {
			repositories = "All repositories available to the connected account"
		} else if git.Enabled {
			repositories = strings.Join(git.AllowedRepositories, ", ")
		}
		return map[string]interface{}{"cloneAndLocalCommits": git.Enabled, "pushCommits": git.PushEnabled, "repositories": repositories, "connectionBindingId": git.ConnectionBindingID}
	}
	return map[string]interface{}{}
}

func (d *AgentBehaviorActionDispatcher) configureWorkspace(ctx context.Context, input ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
	// Workspace authority changes always require a reviewed checkpoint, even
	// if an embedding host permits ordinary write actions autonomously.
	approval, err := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call)
	if err != nil {
		return nil, err
	}
	if approval.ActionCallID != input.Call.ID {
		return nil, errors.New("workspace approval does not belong to this action")
	}
	id, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, err
	}
	reason := "Workspace permission action " + input.Call.ID
	activations, err := d.agents.ListActivations(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id)
	if err != nil {
		return nil, err
	}
	for _, audit := range activations {
		if audit.Reason == reason {
			dep, err := d.agents.GetDeployment(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, id)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"resourceType": "agent_workspace", "deployment": dep, "activation": audit, "replayed": true}, nil
		}
	}
	_, next, _, err := resolveWorkspaceAction(ctx, d.agents, d.workspaceCatalog, run, input.Arguments)
	if err != nil {
		return nil, err
	}
	updated, audit, err := d.agents.UpdateDeployment(ctx, next, next.Revision, approval.DecisionBy.Type, approval.DecisionBy.ID, reason)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"resourceType": "agent_workspace", "deployment": updated, "activation": audit, "replayed": false}, nil
}
