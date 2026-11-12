package models

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func ValidateRunDependencyGroupCreateRecord(record RunDependencyGroupCreateRecord) error {
	if record.Group == nil || record.SourceRun == nil || record.Event == nil || len(record.Dependencies) == 0 {
		return errors.New("dependency group, edges, source run, and event are required")
	}
	if err := record.Group.Validate(); err != nil {
		return err
	}
	if err := record.SourceRun.Validate(); err != nil {
		return err
	}
	if err := record.Event.Validate(); err != nil {
		return err
	}
	if record.SourceRun.Scope != record.Group.Scope || record.SourceRun.ID != record.Group.SourceRunID ||
		record.SourceRun.Status != AgentRunStatusWaitingForDependency || record.SourceRun.WakeCondition == nil ||
		record.SourceRun.WakeCondition.Type != "run_dependencies" || record.SourceRun.WakeCondition.Reference != record.Group.ID ||
		record.SourceRun.Revision != record.ExpectedSourceRevision+1 {
		return ErrInvalidRunDependency
	}
	if record.Event.Scope != record.Group.Scope || record.Event.RunID != record.Group.SourceRunID || record.Event.CorrelationID != record.Group.ID {
		return ErrInvalidScope
	}
	if _, err := EvaluateRunDependencies(record.Group, record.Dependencies); err != nil {
		return err
	}
	targets := make(map[string]*AgentRun, len(record.TargetRuns))
	for _, target := range record.TargetRuns {
		if target == nil || target.Validate() != nil || target.Scope != record.Group.Scope || target.ParentRunID != record.Group.SourceRunID || target.RootRunID != record.SourceRun.RootRunID || target.Status != AgentRunStatusQueued {
			return ErrInvalidRunDependency
		}
		if targets[target.ID] != nil {
			return ErrDependencyConflict
		}
		targets[target.ID] = target
	}
	for _, edge := range record.Dependencies {
		if edge.Kind == RunDependencyKindRun && len(targets) > 0 {
			if targets[edge.TargetRunID] == nil {
				return ErrInvalidRunDependency
			}
			delete(targets, edge.TargetRunID)
		}
	}
	if len(targets) != 0 {
		return ErrInvalidRunDependency
	}
	return validateDependencyGroupSessionWrites(record)
}

// validateDependencyGroupSessionWrites checks the session half of a create
// record (ToolTransition, ChildSessions, RequireInterventionCount) before
// any backend opens a transaction.
func validateDependencyGroupSessionWrites(record RunDependencyGroupCreateRecord) error {
	if n := record.RequireInterventionCount; n != nil && *n < 0 {
		return fmt.Errorf("%w: negative intervention count", ErrInvalidRunDependency)
	}
	if transition := record.ToolTransition; transition != nil {
		if transition.Scope != record.Group.Scope || transition.RunID != record.SourceRun.ID {
			return fmt.Errorf("%w: tool transition belongs to another run", ErrInvalidRunDependency)
		}
		if transition.To != ToolEntryAwaitingChild || transition.Wait == nil || transition.Wait.GroupID != record.Group.ID {
			return fmt.Errorf("%w: tool transition must await this group's children", ErrInvalidRunDependency)
		}
		if strings.TrimSpace(transition.Lease.WorkerID) == "" {
			return fmt.Errorf("%w: tool transition needs the source lease", ErrInvalidRunDependency)
		}
		if err := ValidateToolEntryTransitionRequest(DependencyGroupToolTransition(record)); err != nil {
			return err
		}
		if err := ValidateSessionEvents(transition.Scope, transition.RunID, transition.Events, ErrInvalidToolEntryTransition); err != nil {
			return err
		}
	}
	if len(record.ChildSessions) == 0 {
		return nil
	}
	targets := make(map[string]*AgentRun, len(record.TargetRuns))
	for _, target := range record.TargetRuns {
		targets[target.ID] = target
	}
	seen := make(map[string]bool, len(record.ChildSessions))
	for i := range record.ChildSessions {
		child := DependencyGroupChildSession(record, i)
		target := targets[child.OwnerRunID]
		if child.Scope != record.Group.Scope || target == nil || seen[child.OwnerRunID] {
			return fmt.Errorf("%w: child session %q is not for one of the group's new runs", ErrInvalidRunDependency, child.OwnerRunID)
		}
		seen[child.OwnerRunID] = true
		if strings.TrimSpace(child.ParentSessionID) == "" {
			return fmt.Errorf("%w: child session needs the source session as parent", ErrInvalidRunDependency)
		}
		// Only same-Agent children fork the source session (SPEC A3).
		if child.AgentID != target.AssignedAgentID || target.AssignedAgentID != record.SourceRun.AssignedAgentID {
			return fmt.Errorf("%w: child session %q is not a same-Agent child", ErrInvalidRunDependency, child.OwnerRunID)
		}
		if target.SessionID != "" && target.SessionID != RunAgentSessionID(child.Scope, child.OwnerRunID) {
			return fmt.Errorf("%w: child run %q is bound to another session", ErrInvalidRunDependency, child.OwnerRunID)
		}
		if err := ValidateCreateRunAgentSessionTxRequest(child); err != nil {
			return err
		}
	}
	return nil
}

// DependencyGroupToolTransition is the record's ToolTransition with its
// clock defaulted to the group's creation time.
func DependencyGroupToolTransition(record RunDependencyGroupCreateRecord) ToolEntryTransitionRequest {
	transition := *record.ToolTransition
	if transition.Now.IsZero() {
		transition.Now = record.Group.CreatedAt
	}
	transition.Now = transition.Now.UTC()
	if transition.Lease.Now.IsZero() {
		transition.Lease.Now = transition.Now
	}
	return transition
}

// DependencyGroupChildSession is ChildSessions[i] with its clock defaulted
// to the group's creation time.
func DependencyGroupChildSession(record RunDependencyGroupCreateRecord, i int) CreateRunAgentSessionTxRequest {
	child := record.ChildSessions[i]
	if child.Now.IsZero() {
		child.Now = record.Group.CreatedAt
	}
	child.Now = child.Now.UTC()
	return child
}

// CheckDependencyGroupCreateSource fences a create against the stored
// source Run: the tool transition's lease, the steering count, the revision
// CAS and the session binding, in that order (a steering refusal wins over
// the revision conflict that adding an intervention also causes).
func CheckDependencyGroupCreateSource(current *AgentRun, record RunDependencyGroupCreateRecord) error {
	if transition := record.ToolTransition; transition != nil {
		lease := DependencyGroupToolTransition(record).Lease
		if current.LeaseOwner != lease.WorkerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(lease.Now) {
			return ErrLeaseLost
		}
	}
	if n := record.RequireInterventionCount; n != nil && len(current.PendingInterventions) != *n {
		return ErrSteeringArrived
	}
	if current.Revision != record.ExpectedSourceRevision || record.SourceRun.Revision != current.Revision+1 {
		return ErrRevisionConflict
	}
	if record.SourceRun.SessionID != current.SessionID {
		return ErrSessionMoved
	}
	for _, child := range record.ChildSessions {
		if current.SessionID == "" || child.ParentSessionID != current.SessionID {
			return ErrSessionMoved
		}
	}
	return nil
}

func ValidateRunDependencyResolutionRecord(record RunDependencyResolutionRecord) error {
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(record.GroupID, 128) || !IsValidOpaqueIdentifier(record.DependencyID, 128) ||
		record.ExpectedDependencyRevision <= 0 || record.OccurredAt.IsZero() {
		return ErrInvalidRunDependency
	}
	if record.State != RunDependencyStateSatisfied && record.State != RunDependencyStateFailed && record.State != RunDependencyStateCanceled {
		return ErrInvalidRunDependency
	}
	if err := ValidateCredentialFreeContext(record.Result); err != nil {
		return err
	}
	return ValidateArtifactReferences(nil, record.Artifacts)
}

func SameDependencyGroupRecord(group *RunDependencyGroup, edges []*RunDependency, record RunDependencyGroupCreateRecord) bool {
	if group.Scope != record.Group.Scope || group.SourceRunID != record.Group.SourceRunID || group.Policy != record.Group.Policy ||
		group.IdempotencyKey != record.Group.IdempotencyKey || len(edges) != len(record.Dependencies) {
		return false
	}
	return reflect.DeepEqual(dependencyIntents(edges), dependencyIntents(record.Dependencies))
}

func dependencyIntents(edges []*RunDependency) []string {
	values := make([]string, 0, len(edges))
	for _, edge := range edges {
		values = append(values, strings.Join([]string{string(edge.Kind), edge.TargetRunID, edge.RequestID, string(runeBool(edge.Required))}, "\x00"))
	}
	sort.Strings(values)
	return values
}

func runeBool(value bool) rune {
	if value {
		return '1'
	}
	return '0'
}

func CloneRunDependencies(values []*RunDependency) []*RunDependency {
	result := make([]*RunDependency, len(values))
	for index, edge := range values {
		result[index] = CloneRunDependency(edge)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
