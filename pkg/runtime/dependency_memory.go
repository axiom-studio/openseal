package runtime

import (
	"context"
	"errors"
	"fmt"
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
	if err := checkDependencyGroupCreateSource(currentSource, record); err != nil {
		return nil, err
	}
	for _, target := range record.TargetRuns {
		key := portfolioKey(target.Scope, target.ID)
		if s.agentRuns[key] != nil {
			return nil, ErrRunIdempotency
		}
		if err := s.validateMemoryRunSkillDependenciesLocked(target); err != nil {
			return nil, err
		}
	}
	if err := s.validateMemoryRunSkillDependenciesLocked(record.SourceRun); err != nil {
		return nil, err
	}
	// The memory store has no rollback, so everything that can fail is
	// checked before the first write.
	edgeIDs := make(map[string]bool, len(record.Dependencies))
	for _, edge := range record.Dependencies {
		if edgeIDs[edge.ID] {
			return nil, ErrDependencyConflict
		}
		edgeIDs[edge.ID] = true
	}
	var toolEntry *SessionEntry
	if transition := record.ToolTransition; transition != nil {
		current := s.memorySessionEntryLocked(transition.Scope, transition.EntryID)
		if current == nil {
			return nil, ErrSessionEntryNotFound
		}
		if current.SessionID != currentSource.SessionID {
			return nil, ErrSessionMoved
		}
		next, err := applyToolEntryTransition(current, dependencyGroupToolTransition(record))
		if err != nil {
			return nil, err
		}
		toolEntry = next
	}
	for i := range record.ChildSessions {
		if err := s.checkMemoryChildSessionLocked(dependencyGroupChildSession(record, i)); err != nil {
			return nil, err
		}
	}
	s.saveMemoryDependencyGroupLocked(groupKey, record.Group)
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
		s.saveMemoryAgentRunLocked(portfolioKey(target.Scope, target.ID), target)
	}
	s.saveMemoryAgentRunLocked(sourceKey, record.SourceRun)
	events := []*ActivityEvent{cloneActivityEvent(appendMemoryActivityLocked(s, record.Event))}
	sessions := make([]*AgentSession, 0, len(record.ChildSessions))
	for i := range record.ChildSessions {
		created, err := s.createRunAgentSessionLocked(dependencyGroupChildSession(record, i))
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, created.Session)
	}
	if transition := record.ToolTransition; transition != nil {
		ref := s.agentSessionEntryIndex[portfolioKey(transition.Scope, transition.EntryID)]
		s.agentSessionEntries[ref.sessionKey][ref.index] = toolEntry
		events = append(events, s.appendSessionEventsLocked(transition.Events, dependencyGroupToolTransition(record).Now)...)
		toolEntry = cloneSessionEntry(toolEntry)
	}
	edges := cloneDependencySlice(record.Dependencies)
	evaluation, err := EvaluateRunDependencies(record.Group, edges)
	if err != nil {
		return nil, err
	}
	return &RunDependencyResult{
		Group: cloneRunDependencyGroup(record.Group), Dependencies: edges, Source: cloneAgentRun(record.SourceRun),
		Evaluation: evaluation, Events: events, ToolEntry: toolEntry, ChildSessions: sessions,
	}, nil
}

// checkMemoryChildSessionLocked is the part of createRunAgentSessionLocked
// that can fail, run before the dependency group writes anything.
func (s *MemoryStore) checkMemoryChildSessionLocked(req CreateRunAgentSessionTxRequest) error {
	id := RunAgentSessionID(req.Scope, req.OwnerRunID)
	if s.agentSessions[portfolioKey(req.Scope, id)] != nil {
		return ErrAgentSessionConflict
	}
	if other, ok := s.agentSessionOwners[portfolioKey(req.Scope, req.OwnerRunID)]; ok && other != id {
		return ErrAgentSessionConflict
	}
	parent := s.agentSessions[portfolioKey(req.Scope, req.ParentSessionID)]
	if parent == nil {
		return ErrAgentSessionNotFound
	}
	if req.ForkSequence > parent.NextSequence-1 {
		return fmt.Errorf("%w: fork sequence %d is past the parent leaf %d", ErrInvalidAgentSession, req.ForkSequence, parent.NextSequence-1)
	}
	if parent.Depth+1 > MaxAgentSessionDepth {
		return ErrAgentSessionForkTooDeep
	}
	return nil
}

func (s *MemoryStore) replayDependencyGroupCreateLocked(record RunDependencyGroupCreateRecord, groupKey string) (*RunDependencyResult, error) {
	existing := s.dependencyGroups[groupKey]
	edges := dependencyMapSlice(s.dependencies[groupKey])
	if existing == nil || !sameDependencyGroupRecord(existing, edges, record) {
		return nil, ErrDependencyConflict
	}
	source := s.agentRuns[portfolioKey(existing.Scope, existing.SourceRunID)]
	evaluation, err := EvaluateRunDependencies(existing, edges)
	result := &RunDependencyResult{
		Group: cloneRunDependencyGroup(existing), Dependencies: edges, Source: cloneAgentRun(source), Evaluation: evaluation, Replayed: true,
	}
	// A replay writes nothing: the original create already moved the tool
	// entry and created the child sessions; it reports their current state.
	if transition := record.ToolTransition; transition != nil {
		result.ToolEntry = cloneSessionEntry(s.memorySessionEntryLocked(transition.Scope, transition.EntryID))
	}
	for _, child := range record.ChildSessions {
		if session := s.agentSessions[portfolioKey(child.Scope, RunAgentSessionID(child.Scope, child.OwnerRunID))]; session != nil {
			result.ChildSessions = append(result.ChildSessions, cloneAgentSession(session))
		}
	}
	return result, err
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
	if result.Source.Revision != s.agentRuns[sourceKey].Revision {
		if err := s.validateMemoryRunSkillDependenciesLocked(result.Source); err != nil {
			return nil, err
		}
	}
	s.saveMemoryDependencyGroupLocked(groupKey, result.Group)
	for _, edge := range result.Dependencies {
		s.dependencies[groupKey][edge.ID] = cloneRunDependency(edge)
	}
	if result.Source.Revision != s.agentRuns[sourceKey].Revision {
		s.saveMemoryAgentRunLocked(sourceKey, result.Source)
	}
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
		if err := validateToolEntryTransitionRequest(dependencyGroupToolTransition(record)); err != nil {
			return err
		}
		if err := validateSessionEvents(transition.Scope, transition.RunID, transition.Events, ErrInvalidToolEntryTransition); err != nil {
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
		child := dependencyGroupChildSession(record, i)
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
		if err := validateCreateRunAgentSessionTxRequest(child); err != nil {
			return err
		}
	}
	return nil
}

// dependencyGroupToolTransition is the record's ToolTransition with its
// clock defaulted to the group's creation time.
func dependencyGroupToolTransition(record RunDependencyGroupCreateRecord) ToolEntryTransitionRequest {
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

// dependencyGroupChildSession is ChildSessions[i] with its clock defaulted
// to the group's creation time.
func dependencyGroupChildSession(record RunDependencyGroupCreateRecord, i int) CreateRunAgentSessionTxRequest {
	child := record.ChildSessions[i]
	if child.Now.IsZero() {
		child.Now = record.Group.CreatedAt
	}
	child.Now = child.Now.UTC()
	return child
}

// checkDependencyGroupCreateSource fences a create against the stored
// source Run: the tool transition's lease, the steering count, the revision
// CAS and the session binding, in that order (a steering refusal wins over
// the revision conflict that adding an intervention also causes).
func checkDependencyGroupCreateSource(current *AgentRun, record RunDependencyGroupCreateRecord) error {
	if transition := record.ToolTransition; transition != nil {
		lease := dependencyGroupToolTransition(record).Lease
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
