package runtime

import (
	"context"
	"sort"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *MemoryStore) memorySkillRuntimeMaintenanceActiveLocked(scope Scope, skillID string) *SkillRuntimeMaintenance {
	g := s.skillRuntimeMaintenance[skillRuntimeMaintenanceLockKey(scope, skillID)]
	if g == nil || !g.Active {
		return nil
	}
	return cloneSkillRuntimeMaintenance(g)
}

func (s *MemoryStore) AcquireSkillRuntimeMaintenance(_ context.Context, r SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := skillRuntimeMaintenanceLockKey(r.Scope, r.SkillID)
	next, initial, err := nextSkillRuntimeMaintenance(s.skillRuntimeMaintenance[key], r)
	if err != nil {
		return nil, err
	}
	if initial && s.skillRuntimeUsage[memorySkillRuntimeUsageKey{Scope: r.Scope, SkillID: r.SkillID}] > 0 {
		return nil, ErrSkillReferenceUpgradeBusy
	}
	if initial {
		var revisions []SkillRuntimeMaintenanceBindingRevision
		expected := make(map[string]int64, len(r.ExpectedBindings))
		for _, b := range r.ExpectedBindings {
			expected[b.DeploymentID+"\x00"+b.BindingID] = b.Revision
		}
		for _, b := range s.skillBindings {
			if b.Scope.Kind == r.Scope.Kind && b.Scope.ID == r.Scope.ID && b.SkillID == r.SkillID && !b.Disabled {
				key := b.DeploymentID + "\x00" + b.ID
				if b.SourceIdentity != r.SourceIdentity {
					return nil, ErrSkillRuntimeMaintenanceConflict
				}
				if r.ExpectedBindingDigest != "" {
					revisions = append(revisions, SkillRuntimeMaintenanceBindingRevision{b.DeploymentID, b.ID, b.Revision})
				} else {
					if expected[key] != b.Revision {
						return nil, ErrSkillRuntimeMaintenanceConflict
					}
					delete(expected, key)
				}
			}
		}
		if len(expected) != 0 {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
		if r.ExpectedBindingDigest != "" {
			sort.Slice(revisions, func(i, j int) bool {
				return revisions[i].DeploymentID < revisions[j].DeploymentID || revisions[i].DeploymentID == revisions[j].DeploymentID && revisions[i].BindingID < revisions[j].BindingID
			})
			if skillRuntimeMaintenanceBindingDigest(revisions) != r.ExpectedBindingDigest {
				return nil, ErrSkillRuntimeMaintenanceConflict
			}
		}
	}
	if s.skillRuntimeMaintenance == nil {
		s.skillRuntimeMaintenance = make(map[string]*SkillRuntimeMaintenance)
	}
	s.skillRuntimeMaintenance[key] = cloneSkillRuntimeMaintenance(next)
	return cloneSkillRuntimeMaintenance(next), nil
}

func (s *MemoryStore) GetSkillRuntimeMaintenance(_ context.Context, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.skillRuntimeMaintenance[skillRuntimeMaintenanceLockKey(scope, skillID)]
	if g == nil {
		return nil, ErrSkillRuntimeMaintenanceNotFound
	}
	return cloneSkillRuntimeMaintenance(g), nil
}

func (s *MemoryStore) ListActiveSkillRuntimeMaintenances(_ context.Context, scope Scope) ([]*SkillRuntimeMaintenance, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*SkillRuntimeMaintenance, 0)
	for _, g := range s.skillRuntimeMaintenance {
		if g.Scope == scope && g.Active {
			result = append(result, cloneSkillRuntimeMaintenance(g))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SkillID < result[j].SkillID })
	return result, nil
}

func (s *MemoryStore) CompleteSkillRuntimeMaintenance(_ context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := skillRuntimeMaintenanceLockKey(r.Scope, r.SkillID)
	g := s.skillRuntimeMaintenance[key]
	if err := validateMaintenanceCompletion(g, r); err != nil {
		return nil, err
	}
	if r.VerifiedVersion != g.DesiredVersion {
		return nil, ErrSkillRuntimeMaintenanceConflict
	}
	for _, b := range s.skillBindings {
		if b.Scope.Kind == r.Scope.Kind && b.Scope.ID == r.Scope.ID && b.SkillID == r.SkillID && !b.Disabled && (b.SkillVersion != r.VerifiedVersion || b.SourceIdentity != g.SourceIdentity) {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
	}
	if s.skillRuntimeUsage[memorySkillRuntimeUsageKey{Scope: r.Scope, SkillID: r.SkillID}] > 0 {
		return nil, ErrSkillReferenceUpgradeBusy
	}
	next := completeMaintenanceRecord(g, r)
	s.skillRuntimeMaintenance[key] = next
	return cloneSkillRuntimeMaintenance(next), nil
}

func (s *MemoryStore) BeginSkillRuntimeMaintenanceRollback(_ context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := skillRuntimeMaintenanceLockKey(r.Scope, r.SkillID)
	g := s.skillRuntimeMaintenance[key]
	if err := validateMaintenanceCompletion(g, r); err != nil {
		return nil, err
	}
	next := beginMaintenanceRollbackRecord(g, r)
	s.skillRuntimeMaintenance[key] = next
	return cloneSkillRuntimeMaintenance(next), nil
}

func (s *MemoryStore) ListSkillRuntimeMaintenanceBindings(_ context.Context, scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	if err := validateMaintenanceBindingPage(scope, skillID, afterDeploymentID, afterBindingID, limit); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*skill.Binding, 0, limit)
	for _, b := range s.skillBindings {
		if b.Scope.Kind != scope.Kind || b.Scope.ID != scope.ID || b.SkillID != skillID || b.Disabled || !(b.DeploymentID > afterDeploymentID || b.DeploymentID == afterDeploymentID && b.ID > afterBindingID) {
			continue
		}
		at := sort.Search(len(result), func(i int) bool {
			return result[i].DeploymentID > b.DeploymentID || result[i].DeploymentID == b.DeploymentID && result[i].ID >= b.ID
		})
		if at >= limit {
			continue
		}
		result = append(result, nil)
		copy(result[at+1:], result[at:])
		result[at] = b
		if len(result) > limit {
			result = result[:limit]
		}
	}
	for i, b := range result {
		result[i] = cloneMemorySkillBinding(b)
	}
	return result, nil
}
