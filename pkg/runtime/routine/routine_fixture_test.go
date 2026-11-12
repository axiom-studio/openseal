// The routine tests' own copies of the listed action call records, the Run
// rewriter, the Runbook producer helpers, the chat roster Team catalogs and
// the Agent definition that runtime's tests use.

package routine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

// routineActionCalls holds the action calls a test states for the audit,
// listed as the stores list them: oldest first, then by ID, filtered by
// scope, Run and status, one page at a time.
type routineActionCalls struct {
	calls []*models.ActionCall
}

// put stores call as given, replacing the call with the same ID.
func (r *routineActionCalls) put(call *models.ActionCall) {
	for index, current := range r.calls {
		if current.ID == call.ID {
			r.calls[index] = models.CloneActionCall(call)
			return
		}
	}
	r.calls = append(r.calls, models.CloneActionCall(call))
}

func (r *routineActionCalls) ListActionCalls(_ context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
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
	return routineRecordPage(matched, filter.Offset, filter.Limit), nil
}

// routineRecordPage returns one page of a listed record set as the stores
// page it: from offset, at most limit records, 100 when limit is unset or
// over 500.
func routineRecordPage[T any](records []T, offset, limit int) []T {
	offset = max(offset, 0)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset >= len(records) {
		return []T{}
	}
	records = records[offset:]
	return records[:min(limit, len(records))]
}

// putRoutineRun writes run as given, without the scheduler's rules. A new Run
// is created; an existing one is rewritten through the store's update
// outside any lease, which moves its revision on by one (returned in the
// written Run).
func putRoutineRun(t testing.TB, store *sqlite.Store, run *models.AgentRun) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatalf("create run %s: %v", run.ID, err)
		}
		return run
	}
	next := models.CloneAgentRun(run)
	next.Revision = current.Revision + 1
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, RunID: run.ID, Actor: models.ActivityActor{Type: "system", ID: "test"},
		Summary: "Test rewrote the Run", Visibility: models.ActivityVisibilityScope, CreatedAt: run.UpdatedAt,
	}
	if _, err := store.UpdateAgentRunWithEvent(ctx, next, current.Revision, event, nil); err != nil {
		t.Fatalf("rewrite run %s: %v", run.ID, err)
	}
	return next
}

func routineRunbookLiteral(value interface{}) runbook.Value {
	encoded, _ := json.Marshal(value)
	return runbook.Value{Literal: encoded}
}

func routineRunbookRef(pointer string) *runbook.Value {
	value := runbook.Value{Ref: runbook.JSONPointer(pointer)}
	return &value
}

func newRoutineTestProducer(t *testing.T, definition *runbook.Definition, entrypoint string, calls models.ActionCallReader, opts ...models.ServiceOption) *RunbookStepProducer {
	t.Helper()
	if calls == nil {
		calls = modelsteptest.FixedActionCalls{}
	}
	producer, err := NewRunbookStepProducer(definition, entrypoint, calls, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return producer
}

// routineStepInput is the StepInput of step sequence of run, with binding
// and the Run's session path.
func routineStepInput(run *models.AgentRun, sequence int64, binding *agentrun.TurnRunnerBinding, path *models.AgentSessionPath) agentrun.StepInput {
	if binding == nil {
		binding = &agentrun.TurnRunnerBinding{}
	}
	if path == nil {
		path = &models.AgentSessionPath{}
	}
	return agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: fmt.Sprintf("step-%d", sequence), Sequence: sequence}, Binding: binding, Path: path, Mode: agentrun.StepModeNormal}
}

// produceRoutineStep runs one step and checks it is a valid deterministic
// output.
func produceRoutineStep(t *testing.T, producer *RunbookStepProducer, in agentrun.StepInput) agentrun.StepOutput {
	t.Helper()
	out, err := producer.Produce(t.Context(), in)
	if err != nil {
		t.Fatalf("produce step %s: %v", in.Step.ID, err)
	}
	modelsteptest.RequireValidDeterministicOutput(t, out)
	if out.KernelState == nil {
		t.Fatalf("step %s returned no engine state: %#v", in.Step.ID, out)
	}
	return out
}

// appendRoutineToolResult adds the terminal tool entry of a governed call to
// path, as the inline executor would record it.
func appendRoutineToolResult(path *models.AgentSessionPath, runID, toolCallID string, state models.ToolEntryState, actionCallID string) {
	path.Entries = append(path.Entries, modelsteptest.ProducerToolPath(runID, toolCallID, state, actionCallID).Entries...)
}

func routineWebFetchAction() capability.ModelAction {
	return capability.ModelAction{Name: "web_fetch", SkillID: "web", Version: "1", Action: "fetch", BindingID: "web-binding", BindingRevision: 3}
}

func routineConcurrentDefinition() *runbook.Definition {
	return &runbook.Definition{APIVersion: runbook.APIVersion, ID: "deterministic", Version: "1", Name: "Deterministic", Entrypoints: map[string]string{"manual": "choose"}, Steps: map[string]runbook.Step{
		"choose": {Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{When: runbook.Predicate{Operator: runbook.PredicateTruthy, Left: routineRunbookRef("/input/parallel")}, Next: "fork"}}, Default: "loop"}},
		"fork":   {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, BranchBudgets: map[string]runbook.BudgetAllocation{"a": {MaxTurns: 2}, "b": {MaxActions: 3}}, Join: "join"}},
		"a":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/a": routineRunbookLiteral(true)}, Next: "join"}},
		"b":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/b": routineRunbookLiteral(true)}, Next: "join"}},
		"join":   {Kind: runbook.StepJoin, Join: &runbook.JoinStep{Fork: "fork", Mode: runbook.JoinAll, Next: "done"}},
		"loop":   {Kind: runbook.StepForEach, ForEach: &runbook.ForEachStep{Items: runbook.Value{Ref: "/input/items"}, ItemName: "item", MaxIterations: 3, Body: "copy", Next: "done"}},
		"copy":   {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/last": {Ref: "/loop/item"}}, Next: "again"}},
		"again":  {Kind: runbook.StepLoopReturn, LoopReturn: &runbook.LoopReturnStep{ForEach: "loop"}},
		"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"state": {Ref: "/state"}}}},
	}}
}

// routineRosterTeams serves one Team deployment through both Team catalog
// shapes.
type routineRosterTeams struct {
	team *kernelteam.Deployment
	err  error
}

func (c routineRosterTeams) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.team == nil || c.team.ID != id || c.team.Scope != scope {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return c.team, nil
}

func (c routineRosterTeams) GetDeployment(ctx context.Context, scope skill.ScopeReference, id string) (*kernelteam.Deployment, error) {
	return c.GetTeamDeployment(ctx, scope, id)
}

func routineRosterTeam(scope models.Scope, members ...string) *kernelteam.Deployment {
	team := &kernelteam.Deployment{ID: "crew", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive}
	for _, member := range members {
		team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: member, AgentDeploymentID: member})
	}
	return team
}

// routineRosterPortfolio is a store whose Team catalog serves one Team.
type routineRosterPortfolio struct {
	*sqlite.Store
	team *kernelteam.Deployment
}

func (p routineRosterPortfolio) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if p.team != nil && p.team.ID == id && p.team.Scope == scope {
		return p.team, nil
	}
	return nil, nil
}

func routineAgentDefinition(version string) *kernelagent.AgentDefinition {
	return &kernelagent.AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Operate safely.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
	}
}
