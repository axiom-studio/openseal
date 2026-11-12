// The control tests' own copies of the SQLite session store opener, the Team
// catalog, the accepted Run preparer, the JSON helper and the listed action
// call records that runtime's tests use.

package runtime

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// openControlSessionStore opens a SQLite store on a fresh file, closed when
// the test ends.
func openControlSessionStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// controlTeamCatalog serves one Team deployment and its Agents.
type controlTeamCatalog struct {
	deployment     *kernelagent.AgentDeployment
	deployments    []*kernelagent.AgentDeployment
	definitions    map[string]*kernelagent.AgentDefinition
	teamDeployment *kernelteam.Deployment
}

func (c *controlTeamCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return c.teamDeployment, nil
}

func (c *controlTeamCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return nil, nil
}

func (c *controlTeamCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.deployment, nil
}

func (c *controlTeamCatalog) ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	if c.deployments != nil {
		return c.deployments, nil
	}
	if c.deployment == nil {
		return nil, nil
	}
	return []*kernelagent.AgentDeployment{c.deployment}, nil
}

func (c *controlTeamCatalog) GetAgentDefinition(_ context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	return c.definitions[id+"@"+version], nil
}

func (c *controlTeamCatalog) ActivateSkills(context.Context, skill.ScopeReference, string, skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return nil, nil
}

func (c *controlTeamCatalog) GetActionCall(context.Context, models.Scope, string) (*models.ActionCall, error) {
	return nil, models.ErrActionNotFound
}

// controlAcceptedRunPreparer accepts runbook Runs of one definition version
// and records their execution.
type controlAcceptedRunPreparer struct {
	calls   int
	id      string
	version string
	err     error
}

func (p *controlAcceptedRunPreparer) PrepareAcceptedRunExecution(_ context.Context, request *models.CreateAgentRunRequest) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	request.Context = maps.Clone(request.Context)
	if request.Context == nil {
		request.Context = map[string]interface{}{}
	}
	delete(request.Context, models.AcceptedRunExecutionContextKey)
	if request.Entrypoint == "" {
		return nil
	}
	if id, exists := request.Context["runbookDefinitionId"]; exists && id != p.id {
		return models.ErrAcceptedRunExecution
	}
	if version, exists := request.Context["runbookDefinitionVersion"]; exists && version != p.version {
		return models.ErrAcceptedRunExecution
	}
	if method, ok := request.Plan["runbook"].(map[string]interface{}); ok && (method["id"] != p.id || method["version"] != p.version) {
		return models.ErrAcceptedRunExecution
	}
	request.Context[models.AcceptedRunExecutionContextKey] = &models.AcceptedRunExecution{
		Scope: request.Scope, DeploymentID: request.AssignedAgentID, DefinitionID: "immutable-agent", DefinitionVersion: "1",
		RunbookID: p.id, RunbookVersion: p.version, Entrypoint: request.Entrypoint,
		SkillDependencies: []models.SkillRuntimeReference{{SkillID: "publisher", SkillVersion: "1.0.0", SourceIdentity: "verified::publisher"}},
	}
	return nil
}

func controlJSON(t testing.TB, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// controlActionCallRecords holds the action calls a test states and lists
// them as the stores do: oldest first, then by ID, filtered by scope, Run
// and status, one page at a time.
type controlActionCallRecords struct {
	calls []*models.ActionCall
}

// put stores call as given, replacing the call with the same ID.
func (r *controlActionCallRecords) put(call *models.ActionCall) {
	for index, current := range r.calls {
		if current.ID == call.ID {
			r.calls[index] = models.CloneActionCall(call)
			return
		}
	}
	r.calls = append(r.calls, models.CloneActionCall(call))
}

// get returns a copy of a stored call.
func (r *controlActionCallRecords) get(t testing.TB, id string) *models.ActionCall {
	t.Helper()
	for _, call := range r.calls {
		if call.ID == id {
			return models.CloneActionCall(call)
		}
	}
	t.Fatalf("no action call %s", id)
	return nil
}

func (r *controlActionCallRecords) ListActionCalls(_ context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	matched := make([]*models.ActionCall, 0)
	for _, call := range r.calls {
		if call.Scope == filter.Scope && (filter.RunID == "" || call.RunID == filter.RunID) &&
			(len(filter.Status) == 0 || slices.Contains(filter.Status, call.Status)) {
			matched = append(matched, models.CloneActionCall(call))
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	offset, limit := max(filter.Offset, 0), filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset >= len(matched) {
		return []*models.ActionCall{}, nil
	}
	matched = matched[offset:]
	return matched[:min(limit, len(matched))], nil
}
