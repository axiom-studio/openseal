package runtime

import (
	"errors"
	"testing"
	"time"
)

func dependencyFixture(mode FanInMode, quorum int, failure DependencyFailureMode, expected int) *RunDependencyGroup {
	now := time.Now().UTC()
	sealed := now
	return &RunDependencyGroup{
		ID: "join-1", Scope: Scope{Kind: "tenant", ID: "one"}, SourceRunID: "source-1",
		Policy: RunDependencyPolicy{Mode: mode, Quorum: quorum, FailureMode: failure}, ExpectedCount: expected,
		Status: RunDependencyGroupWaiting, Revision: 2, CreatedAt: now, UpdatedAt: now, SealedAt: &sealed,
	}
}

func dependencyEdge(group *RunDependencyGroup, id string, state RunDependencyState, required bool) *RunDependency {
	now := group.CreatedAt
	edge := &RunDependency{
		ID: id, Scope: group.Scope, GroupID: group.ID, SourceRunID: group.SourceRunID, RequestID: "request-" + id,
		Kind: RunDependencyKindAgentRequest, State: state, Required: required, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if state == RunDependencyStateSatisfied || state == RunDependencyStateFailed || state == RunDependencyStateCanceled {
		edge.ResolvedAt = &now
	}
	return edge
}

func TestEvaluateRunDependenciesRequiresSealAndCompleteMembership(t *testing.T) {
	group := dependencyFixture(FanInModeAll, 0, DependencyFailureFailFast, 2)
	group.Status = RunDependencyGroupOpen
	group.SealedAt = nil
	edges := []*RunDependency{dependencyEdge(group, "one", RunDependencyStateSatisfied, true)}
	evaluation, err := EvaluateRunDependencies(group, edges)
	if err != nil || evaluation.Wake || evaluation.Status != RunDependencyGroupOpen {
		t.Fatalf("open evaluation = %#v, err = %v", evaluation, err)
	}

	group.Status = RunDependencyGroupWaiting
	group.SealedAt = &group.UpdatedAt
	if _, err := EvaluateRunDependencies(group, edges); !errors.Is(err, ErrInvalidRunDependency) {
		t.Fatalf("sealed incomplete group error = %v", err)
	}
}

func TestEvaluateRunDependenciesAllHonorsRequiredAndOptionalEdges(t *testing.T) {
	group := dependencyFixture(FanInModeAll, 0, DependencyFailureFailFast, 3)
	edges := []*RunDependency{
		dependencyEdge(group, "required-b", RunDependencyStateSatisfied, true),
		dependencyEdge(group, "optional", RunDependencyStateRunning, false),
		dependencyEdge(group, "required-a", RunDependencyStateSatisfied, true),
	}
	evaluation, err := EvaluateRunDependencies(group, edges)
	if err != nil || !evaluation.Wake || evaluation.Status != RunDependencyGroupSatisfied || evaluation.Required != 2 {
		t.Fatalf("all evaluation = %#v, err = %v", evaluation, err)
	}
	if got := evaluation.SatisfiedDependencies; len(got) != 2 || got[0] != "required-a" || got[1] != "required-b" {
		t.Fatalf("sorted satisfied dependencies = %#v", got)
	}

	edges[0] = dependencyEdge(group, "required-b", RunDependencyStateFailed, true)
	evaluation, err = EvaluateRunDependencies(group, edges)
	if err != nil || !evaluation.Wake || evaluation.Status != RunDependencyGroupFailed {
		t.Fatalf("fail-fast all evaluation = %#v, err = %v", evaluation, err)
	}
	group.Policy.FailureMode = DependencyFailureWait
	evaluation, err = EvaluateRunDependencies(group, edges)
	if err != nil || evaluation.Wake || evaluation.Status != RunDependencyGroupWaiting {
		t.Fatalf("wait all evaluation = %#v, err = %v", evaluation, err)
	}
}

func TestEvaluateRunDependenciesAnyIsOrderIndependent(t *testing.T) {
	group := dependencyFixture(FanInModeAny, 0, DependencyFailureWait, 3)
	edges := []*RunDependency{
		dependencyEdge(group, "z", RunDependencyStateFailed, true),
		dependencyEdge(group, "a", RunDependencyStateSatisfied, false),
		dependencyEdge(group, "m", RunDependencyStatePending, true),
	}
	first, err := EvaluateRunDependencies(group, edges)
	if err != nil || !first.Wake || first.Status != RunDependencyGroupSatisfied {
		t.Fatalf("any evaluation = %#v, err = %v", first, err)
	}
	second, err := EvaluateRunDependencies(group, []*RunDependency{edges[2], edges[0], edges[1]})
	if err != nil || second.Status != first.Status || second.SatisfiedDependencies[0] != first.SatisfiedDependencies[0] {
		t.Fatalf("reordered evaluation = %#v, err = %v", second, err)
	}
}

func TestEvaluateRunDependenciesQuorumSucceedsOrBecomesImpossible(t *testing.T) {
	group := dependencyFixture(FanInModeQuorum, 2, DependencyFailureFailFast, 3)
	edges := []*RunDependency{
		dependencyEdge(group, "one", RunDependencyStateSatisfied, true),
		dependencyEdge(group, "two", RunDependencyStateSatisfied, true),
		dependencyEdge(group, "three", RunDependencyStateRunning, false),
	}
	evaluation, err := EvaluateRunDependencies(group, edges)
	if err != nil || !evaluation.Wake || evaluation.Status != RunDependencyGroupSatisfied {
		t.Fatalf("quorum success = %#v, err = %v", evaluation, err)
	}
	edges[1] = dependencyEdge(group, "two", RunDependencyStateFailed, true)
	edges[2] = dependencyEdge(group, "three", RunDependencyStateCanceled, false)
	evaluation, err = EvaluateRunDependencies(group, edges)
	if err != nil || !evaluation.Wake || evaluation.Status != RunDependencyGroupFailed {
		t.Fatalf("quorum failure = %#v, err = %v", evaluation, err)
	}
}

func TestEvaluateRunDependenciesTerminalGroupDoesNotWakeTwice(t *testing.T) {
	group := dependencyFixture(FanInModeAny, 0, DependencyFailureFailFast, 1)
	now := group.UpdatedAt
	group.Status = RunDependencyGroupSatisfied
	group.ResolvedAt = &now
	group.WakeSignalID = "dependency-group:join-1:2"
	evaluation, err := EvaluateRunDependencies(group, []*RunDependency{dependencyEdge(group, "one", RunDependencyStateSatisfied, true)})
	if err != nil || evaluation.Wake || evaluation.Status != RunDependencyGroupSatisfied {
		t.Fatalf("terminal evaluation = %#v, err = %v", evaluation, err)
	}
}

func TestRunDependencyValidationRejectsUnsafeResultAndDuplicateEdges(t *testing.T) {
	group := dependencyFixture(FanInModeAll, 0, DependencyFailureFailFast, 2)
	unsafe := dependencyEdge(group, "unsafe", RunDependencyStateSatisfied, true)
	unsafe.Result = map[string]interface{}{"apiKey": "should-never-be-model-visible"}
	if err := unsafe.Validate(); !errors.Is(err, ErrInvalidRunDependency) {
		t.Fatalf("unsafe dependency error = %v", err)
	}
	edge := dependencyEdge(group, "same", RunDependencyStateSatisfied, true)
	if _, err := EvaluateRunDependencies(group, []*RunDependency{edge, cloneRunDependency(edge)}); !errors.Is(err, ErrInvalidRunDependency) {
		t.Fatalf("duplicate dependency error = %v", err)
	}
}
