package runtime

import "context"

var _ RunDependencyReconciliationStore = (*MemoryStore)(nil)

func (s *MemoryStore) saveMemoryDependencyGroupLocked(key string, group *RunDependencyGroup) {
	s.dependencyGroups[key] = cloneRunDependencyGroup(group)
	if s.dependencyWaitingGroups == nil {
		s.dependencyWaitingGroups = make(map[Scope]*memoryOrderedIndexNode[string])
	}
	less := func(left, right string) bool { return left < right }
	if group.Status == RunDependencyGroupWaiting {
		s.dependencyWaitingGroups[group.Scope] = memoryOrderedIndexInsert(s.dependencyWaitingGroups[group.Scope], group.ID, less)
	} else {
		s.dependencyWaitingGroups[group.Scope] = memoryOrderedIndexDelete(s.dependencyWaitingGroups[group.Scope], group.ID, less)
	}
}

func (s *MemoryStore) ListWaitingRunDependencyGroups(_ context.Context, scope Scope, afterID string, limit int) ([]RunDependencyGroupWork, error) {
	if err := validateWaitingDependencyGroupPage(scope, afterID, limit); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := memoryOrderedIndexAfter(s.dependencyWaitingGroups[scope], &afterID, nil, limit, func(left, right string) bool { return left < right })
	result := make([]RunDependencyGroupWork, 0, len(ids))
	for _, id := range ids {
		group := s.dependencyGroups[dependencyGroupStoreKey(scope, id)]
		source := s.agentRuns[portfolioKey(scope, group.SourceRunID)]
		if source == nil {
			continue
		}
		ready := isTerminalAgentRunStatus(source.Status)
		if !ready {
			for _, edge := range s.dependencies[dependencyGroupStoreKey(scope, id)] {
				if edge.Kind != RunDependencyKindRun || edge.State != RunDependencyStatePending && edge.State != RunDependencyStateRunning {
					continue
				}
				if target := s.agentRuns[portfolioKey(scope, edge.TargetRunID)]; target != nil && isTerminalAgentRunStatus(target.Status) {
					ready = true
					break
				}
			}
		}
		result = append(result, RunDependencyGroupWork{Scope: scope, GroupID: group.ID, GroupRevision: group.Revision, SourceRunID: source.ID, SourceRevision: source.Revision, Ready: ready})
	}
	return result, nil
}

func (s *MemoryStore) ReconcileRunDependencyGroup(_ context.Context, record RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error) {
	if err := validateDependencyGroupReconciliationRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := dependencyGroupStoreKey(record.Scope, record.GroupID)
	group := s.dependencyGroups[key]
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	edges := dependencyMapSlice(s.dependencies[key])
	sourceKey := portfolioKey(record.Scope, group.SourceRunID)
	source := s.agentRuns[sourceKey]
	targets := make(map[string]*AgentRun)
	if source != nil && !isTerminalAgentRunStatus(source.Status) && !isTerminalDependencyGroup(group) {
		for _, edge := range edges {
			if edge.Kind != RunDependencyKindRun || edge.State != RunDependencyStatePending && edge.State != RunDependencyStateRunning {
				continue
			}
			if target := s.agentRuns[portfolioKey(record.Scope, edge.TargetRunID)]; target != nil && isTerminalAgentRunStatus(target.Status) {
				targets[target.ID] = cloneAgentRun(target)
			}
		}
	}
	result, err := applyDependencyGroupReconciliation(group, edges, source, targets, record)
	if err != nil || result.Replayed {
		return result, err
	}
	if result.Source.Revision != source.Revision {
		if err := s.validateMemoryRunSkillDependenciesLocked(result.Source); err != nil {
			return nil, err
		}
	}
	s.saveMemoryDependencyGroupLocked(key, result.Group)
	for _, edge := range result.Dependencies {
		if edge.Revision != s.dependencies[key][edge.ID].Revision {
			s.dependencies[key][edge.ID] = cloneRunDependency(edge)
		}
	}
	if result.Source.Revision != source.Revision {
		s.saveMemoryAgentRunLocked(sourceKey, result.Source)
	}
	for index, event := range result.Events {
		result.Events[index] = cloneActivityEvent(appendMemoryActivityLocked(s, event))
	}
	return cloneDependencyResult(result), nil
}
