package models

import (
	"errors"
	"testing"
	"time"
)

func detachedSource(group *RunDependencyGroup) *AgentRun {
	return &AgentRun{ID: group.SourceRunID, Scope: group.Scope, Status: AgentRunStatusRunning, Revision: 7, LeaseOwner: "worker-1"}
}

func TestDetachedGroupResolvesWithoutTouchingItsRunningSource(t *testing.T) {
	group := dependencyFixture(FanInModeAll, 0, DependencyFailureWait, 2)
	group.Detached = true
	edges := []*RunDependency{
		dependencyEdge(group, "s1", RunDependencyStateRunning, true),
		dependencyEdge(group, "s2", RunDependencyStateRunning, true),
	}
	source := detachedSource(group)
	record := RunDependencyResolutionRecord{
		Scope: group.Scope, GroupID: group.ID, DependencyID: "s1", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Actor: ActivityActor{Type: "worker", ID: "w"}, OccurredAt: time.Now().UTC(),
	}
	first, err := ApplyRunDependencyResolution(group, edges, source, record)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.Revision != source.Revision || first.Source.Status != AgentRunStatusRunning || first.Evaluation.Wake ||
		first.Group.Status != RunDependencyGroupWaiting {
		t.Fatalf("first = source %d %s wake %v group %s", first.Source.Revision, first.Source.Status, first.Evaluation.Wake, first.Group.Status)
	}
	record.DependencyID = "s2"
	second, err := ApplyRunDependencyResolution(first.Group, first.Dependencies, source, record)
	if err != nil {
		t.Fatal(err)
	}
	if second.Group.Status != RunDependencyGroupSatisfied || second.Evaluation.Wake || second.Source.Revision != source.Revision ||
		second.Source.WakeCondition != nil {
		t.Fatalf("second = group %s wake %v source %d", second.Group.Status, second.Evaluation.Wake, second.Source.Revision)
	}
	// The same group without Detached refuses a source that is not parked on it.
	group.Detached = false
	if _, err := ApplyRunDependencyResolution(group, edges, source, record); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("attached resolution of a running source = %v", err)
	}
}

func TestDetachedGroupReconcilesWhileItsSourceSteps(t *testing.T) {
	group := dependencyFixture(FanInModeAll, 0, DependencyFailureWait, 1)
	group.Detached = true
	edge := dependencyEdge(group, "s1", RunDependencyStateRunning, true)
	edge.Kind, edge.RequestID, edge.TargetRunID = RunDependencyKindRun, "", "child-1"
	source := detachedSource(group)
	target := &AgentRun{ID: "child-1", Scope: group.Scope, Status: AgentRunStatusFailed, Error: "site down"}
	result, err := ApplyDependencyGroupReconciliation(group, []*RunDependency{edge}, source, map[string]*AgentRun{"child-1": target},
		RunDependencyGroupReconciliationRecord{
			Scope: group.Scope, GroupID: group.ID, ExpectedGroupRevision: group.Revision, ExpectedSourceRevision: source.Revision - 3,
			Actor: ActivityActor{Type: "worker", ID: "w"}, OccurredAt: time.Now().UTC(),
		})
	if err != nil {
		t.Fatal(err)
	}
	if result.Group.Status != RunDependencyGroupFailed || result.Dependencies[0].State != RunDependencyStateFailed || result.Evaluation.Wake ||
		result.Source.Revision != source.Revision {
		t.Fatalf("reconciled = group %s edge %s wake %v", result.Group.Status, result.Dependencies[0].State, result.Evaluation.Wake)
	}
}
