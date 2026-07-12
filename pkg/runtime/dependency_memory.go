package runtime

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
)

func dependencyGroupStoreKey(scope Scope, id string) string { return scope.key() + ":" + id }

func dependencyGroupIdempotencyStoreKey(scope Scope, key string) string {
	return scope.key() + ":" + key
}

func (s *MemoryStore) CreateRunDependencyGroup(_ context.Context, record RunDependencyGroupCreateRecord) (*RunDependencyResult, error) {
	if err := validateRunDependencyGroupCreateRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	groupKey := dependencyGroupStoreKey(record.Group.Scope, record.Group.ID)
	if idempotencyKey := strings.TrimSpace(record.Group.IdempotencyKey); idempotencyKey != "" {
		existingID := s.dependencyGroupKeys[dependencyGroupIdempotencyStoreKey(record.Group.Scope, idempotencyKey)]
		if existingID != "" {
			return s.replayDependencyGroupCreateLocked(record, dependencyGroupStoreKey(record.Group.Scope, existingID))
		}
	}
	if s.dependencyGroups[groupKey] != nil {
		return s.replayDependencyGroupCreateLocked(record, groupKey)
	}
	sourceKey := portfolioKey(record.SourceRun.Scope, record.SourceRun.ID)
	currentSource := s.agentRuns[sourceKey]
	if currentSource == nil {
		return nil, ErrRunNotFound
	}
	if currentSource.Revision != record.ExpectedSourceRevision || record.SourceRun.Revision != currentSource.Revision+1 {
		return nil, ErrRevisionConflict
	}
	for _, target := range record.TargetRuns {
		key := portfolioKey(target.Scope, target.ID)
		if s.agentRuns[key] != nil {
			return nil, ErrRunIdempotency
		}
	}
	s.dependencyGroups[groupKey] = cloneRunDependencyGroup(record.Group)
	s.dependencies[groupKey] = make(map[string]*RunDependency, len(record.Dependencies))
	for _, edge := range record.Dependencies {
		if s.dependencies[groupKey][edge.ID] != nil {
			return nil, ErrDependencyConflict
		}
		s.dependencies[groupKey][edge.ID] = cloneRunDependency(edge)
	}
	if record.Group.IdempotencyKey != "" {
		s.dependencyGroupKeys[dependencyGroupIdempotencyStoreKey(record.Group.Scope, record.Group.IdempotencyKey)] = record.Group.ID
	}
	for _, target := range record.TargetRuns {
		s.agentRuns[portfolioKey(target.Scope, target.ID)] = cloneAgentRun(target)
	}
	s.agentRuns[sourceKey] = cloneAgentRun(record.SourceRun)
	persisted := cloneActivityEvent(appendMemoryActivityLocked(s, record.Event))
	edges := cloneDependencySlice(record.Dependencies)
	evaluation, err := EvaluateRunDependencies(record.Group, edges)
	if err != nil {
		return nil, err
	}
	return &RunDependencyResult{
		Group: cloneRunDependencyGroup(record.Group), Dependencies: edges, Source: cloneAgentRun(record.SourceRun),
		Evaluation: evaluation, Events: []*ActivityEvent{persisted},
	}, nil
}

func (s *MemoryStore) replayDependencyGroupCreateLocked(record RunDependencyGroupCreateRecord, groupKey string) (*RunDependencyResult, error) {
	existing := s.dependencyGroups[groupKey]
	edges := dependencyMapSlice(s.dependencies[groupKey])
	if existing == nil || !sameDependencyGroupRecord(existing, edges, record) {
		return nil, ErrDependencyConflict
	}
	source := s.agentRuns[portfolioKey(existing.Scope, existing.SourceRunID)]
	evaluation, err := EvaluateRunDependencies(existing, edges)
	return &RunDependencyResult{
		Group: cloneRunDependencyGroup(existing), Dependencies: edges, Source: cloneAgentRun(source), Evaluation: evaluation, Replayed: true,
	}, err
}

func (s *MemoryStore) GetRunDependencyGroup(_ context.Context, scope Scope, groupID string) (*RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRunDependencyGroup(s.dependencyGroups[dependencyGroupStoreKey(scope, groupID)]), nil
}

func (s *MemoryStore) FindRunDependencyGroupByIdempotencyKey(_ context.Context, scope Scope, key string) (*RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.dependencyGroupKeys[dependencyGroupIdempotencyStoreKey(scope, strings.TrimSpace(key))]
	return cloneRunDependencyGroup(s.dependencyGroups[dependencyGroupStoreKey(scope, id)]), nil
}

func (s *MemoryStore) ListRunDependencies(_ context.Context, scope Scope, groupID string) ([]*RunDependency, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := dependencyGroupStoreKey(scope, groupID)
	if s.dependencyGroups[key] == nil {
		return nil, ErrDependencyGroupNotFound
	}
	return dependencyMapSlice(s.dependencies[key]), nil
}

func (s *MemoryStore) ResolveRunDependency(_ context.Context, record RunDependencyResolutionRecord) (*RunDependencyResult, error) {
	if err := validateRunDependencyResolutionRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	groupKey := dependencyGroupStoreKey(record.Scope, record.GroupID)
	group := s.dependencyGroups[groupKey]
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	edges := dependencyMapSlice(s.dependencies[groupKey])
	sourceKey := portfolioKey(group.Scope, group.SourceRunID)
	result, err := applyRunDependencyResolution(group, edges, s.agentRuns[sourceKey], record)
	if err != nil || result.Replayed {
		return result, err
	}
	s.dependencyGroups[groupKey] = cloneRunDependencyGroup(result.Group)
	for _, edge := range result.Dependencies {
		s.dependencies[groupKey][edge.ID] = cloneRunDependency(edge)
	}
	s.agentRuns[sourceKey] = cloneAgentRun(result.Source)
	persisted := make([]*ActivityEvent, 0, len(result.Events))
	for _, event := range result.Events {
		persisted = append(persisted, cloneActivityEvent(appendMemoryActivityLocked(s, event)))
	}
	result.Events = persisted
	return cloneDependencyResult(result), nil
}

func validateRunDependencyGroupCreateRecord(record RunDependencyGroupCreateRecord) error {
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
	return nil
}

func validateRunDependencyResolutionRecord(record RunDependencyResolutionRecord) error {
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(record.GroupID, 128) || !validOpaqueIdentifier(record.DependencyID, 128) ||
		record.ExpectedDependencyRevision <= 0 || record.OccurredAt.IsZero() {
		return ErrInvalidRunDependency
	}
	if record.State != RunDependencyStateSatisfied && record.State != RunDependencyStateFailed && record.State != RunDependencyStateCanceled {
		return ErrInvalidRunDependency
	}
	if err := validateCredentialFreeContext(record.Result); err != nil {
		return err
	}
	return validateArtifactReferences(nil, record.Artifacts)
}

func sameDependencyGroupRecord(group *RunDependencyGroup, edges []*RunDependency, record RunDependencyGroupCreateRecord) bool {
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

func dependencyMapSlice(values map[string]*RunDependency) []*RunDependency {
	result := make([]*RunDependency, 0, len(values))
	for _, edge := range values {
		result = append(result, cloneRunDependency(edge))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneDependencySlice(values []*RunDependency) []*RunDependency {
	result := make([]*RunDependency, len(values))
	for index, edge := range values {
		result[index] = cloneRunDependency(edge)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneDependencyResult(in *RunDependencyResult) *RunDependencyResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Group = cloneRunDependencyGroup(in.Group)
	out.Dependency = cloneRunDependency(in.Dependency)
	out.Dependencies = cloneDependencySlice(in.Dependencies)
	out.Source = cloneAgentRun(in.Source)
	out.Events = make([]*ActivityEvent, len(in.Events))
	for index, event := range in.Events {
		out.Events[index] = cloneActivityEvent(event)
	}
	return &out
}
