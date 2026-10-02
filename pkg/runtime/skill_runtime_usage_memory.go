package runtime

import (
	"context"
	"sort"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type memorySkillRuntimeUsageKey struct {
	Scope        Scope
	SkillID      string
	SkillVersion string
	DeploymentID string
	BindingID    string
}

type memorySkillRuntimeUsageEntry struct {
	Keys     [2]memorySkillRuntimeUsageKey
	RunKey   string
	Status   ActionCallStatus
	Blocking bool
}

func memorySkillRuntimeUsageKeyFor(filter SkillRuntimeUsageFilter) memorySkillRuntimeUsageKey {
	return memorySkillRuntimeUsageKey{Scope: filter.Scope, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion, DeploymentID: filter.DeploymentID, BindingID: filter.BindingID}
}

// Canonical action writes keep both runtime-wide and exact-grant counters in
// the same MemoryStore lock as the durable action transition. Queries never
// traverse unrelated actions or copy action output payloads.
func (s *MemoryStore) saveMemoryActionCallLocked(key string, call *ActionCall) {
	previous, exists := s.skillRuntimeUsageCalls[key]
	if exists {
		if previous.Blocking {
			s.adjustMemorySkillRuntimeUsageLocked(previous.Keys, -1)
		}
		if receipts := s.skillRuntimeReceiptRuns[previous.RunKey]; receipts != nil {
			delete(receipts, key)
			if len(receipts) == 0 {
				delete(s.skillRuntimeReceiptRuns, previous.RunKey)
			}
		}
	}
	s.actions[key] = cloneActionCall(call)
	entry := memorySkillRuntimeUsageEntry{
		Keys: [2]memorySkillRuntimeUsageKey{
			{Scope: call.Scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion},
			{Scope: call.Scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, DeploymentID: call.DeploymentID, BindingID: call.BindingID},
		},
		RunKey: portfolioKey(call.Scope, call.RunID), Status: call.Status,
	}
	entry.Blocking = unfinishedSkillActionStatus(call.Status) || (call.Status == ActionCallStatusSucceeded && s.memorySkillRunActiveLocked(entry.RunKey))
	if s.skillRuntimeUsageCalls == nil {
		s.skillRuntimeUsageCalls = make(map[string]memorySkillRuntimeUsageEntry)
	}
	s.skillRuntimeUsageCalls[key] = entry
	if entry.Blocking {
		s.adjustMemorySkillRuntimeUsageLocked(entry.Keys, 1)
	}
	if call.Status == ActionCallStatusSucceeded {
		if s.skillRuntimeReceiptRuns == nil {
			s.skillRuntimeReceiptRuns = make(map[string]map[string]struct{})
		}
		if s.skillRuntimeReceiptRuns[entry.RunKey] == nil {
			s.skillRuntimeReceiptRuns[entry.RunKey] = make(map[string]struct{})
		}
		s.skillRuntimeReceiptRuns[entry.RunKey][key] = struct{}{}
	}
}

func unfinishedSkillActionStatus(status ActionCallStatus) bool {
	return status == ActionCallStatusReady || status == ActionCallStatusWaitingApproval || status == ActionCallStatusRunning || status == ActionCallStatusCompensating
}

func (s *MemoryStore) memorySkillRunActiveLocked(runKey string) bool {
	run := s.agentRuns[runKey]
	return run != nil && !isTerminalAgentRunStatus(run.Status)
}

func (s *MemoryStore) adjustMemorySkillRuntimeUsageLocked(keys [2]memorySkillRuntimeUsageKey, delta int) {
	if s.skillRuntimeUsage == nil {
		s.skillRuntimeUsage = make(map[memorySkillRuntimeUsageKey]int)
	}
	for i, key := range keys {
		if i > 0 && key == keys[0] {
			continue
		}
		count := s.skillRuntimeUsage[key] + delta
		if count > 0 {
			s.skillRuntimeUsage[key] = count
		} else {
			delete(s.skillRuntimeUsage, key)
		}
	}
	logical := keys[0]
	logical.SkillVersion = ""
	count := s.skillRuntimeUsage[logical] + delta
	if count > 0 {
		s.skillRuntimeUsage[logical] = count
	} else {
		delete(s.skillRuntimeUsage, logical)
	}
}

func (s *MemoryStore) refreshMemorySkillRuntimeReceiptsLocked(runKey string) {
	active := s.memorySkillRunActiveLocked(runKey)
	for key := range s.skillRuntimeReceiptRuns[runKey] {
		entry := s.skillRuntimeUsageCalls[key]
		if entry.Blocking == active {
			continue
		}
		delta := -1
		if active {
			delta = 1
		}
		s.adjustMemorySkillRuntimeUsageLocked(entry.Keys, delta)
		entry.Blocking = active
		s.skillRuntimeUsageCalls[key] = entry
	}
}

func (s *MemoryStore) HasSkillRuntimeUsage(_ context.Context, filter SkillRuntimeUsageFilter) (bool, error) {
	if err := filter.Validate(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasMemorySkillRuntimeUsageLocked(filter), nil
}

func (s *MemoryStore) hasMemorySkillRuntimeUsageLocked(filter SkillRuntimeUsageFilter) bool {
	if s.skillRuntimeUsage[memorySkillRuntimeUsageKeyFor(filter)] > 0 {
		return true
	}
	if filter.DeploymentID != "" {
		key := memorySkillRuntimeUsageKeyFor(filter)
		key.BindingID = ""
		if s.skillRuntimeUsage[key] > 0 {
			return true
		}
		key.DeploymentID = ""
		// Unqualified typed Run pins conservatively retain the runtime, but
		// cannot prove authority for any particular account.
		return s.skillRuntimeUsage[key] > 0 && s.memoryUnqualifiedRunSkillDependencyLocked(key)
	}
	return false
}

func (s *MemoryStore) ListReferencedSkillRuntimeVersions(_ context.Context, scope Scope) ([]SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	references := make(map[SkillRuntimeReference]struct{})
	for _, binding := range s.skillBindings {
		if !binding.Disabled && binding.Scope.Kind == scope.Kind && binding.Scope.ID == scope.ID {
			references[SkillRuntimeReference{SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity}] = struct{}{}
		}
	}
	for key := range s.skillRuntimeUsage {
		if key.Scope != scope || key.DeploymentID == "" || key.SkillVersion == "" {
			continue
		}
		identity := ""
		binding := s.skillBindings[memorySkillBindingKey(skill.ScopeReference(scope), key.DeploymentID, key.BindingID)]
		if binding != nil && binding.SkillID == key.SkillID && binding.SkillVersion == key.SkillVersion {
			identity = binding.SourceIdentity
		}
		references[SkillRuntimeReference{SkillID: key.SkillID, SkillVersion: key.SkillVersion, SourceIdentity: identity}] = struct{}{}
	}
	for _, dependencies := range s.skillRuntimeRunDependencies {
		for key := range dependencies {
			if key.Scope == scope {
				references[SkillRuntimeReference{SkillID: key.SkillID, SkillVersion: key.SkillVersion}] = struct{}{}
			}
		}
	}
	result := make([]SkillRuntimeReference, 0, len(references))
	for reference := range references {
		result = append(result, reference)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SkillID != result[j].SkillID {
			return result[i].SkillID < result[j].SkillID
		}
		if result[i].SkillVersion != result[j].SkillVersion {
			return result[i].SkillVersion < result[j].SkillVersion
		}
		return result[i].SourceIdentity < result[j].SourceIdentity
	})
	return result, nil
}
