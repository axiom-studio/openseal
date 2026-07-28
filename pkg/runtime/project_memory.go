package runtime

import (
	"context"
	"sort"
)

func projectKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
func (s *MemoryStore) CreateProject(_ context.Context, i *Project) error {
	if err := i.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := projectKey(i.Scope, i.ID)
	if _, ok := s.projects[k]; ok {
		return ErrProjectConflict
	}
	s.projects[k] = cloneProject(i)
	return nil
}
func (s *MemoryStore) CreateProjectWithEvent(_ context.Context, i *Project, e *ActivityEvent) (*ActivityEvent, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := projectKey(i.Scope, i.ID)
	if s.projects[k] != nil {
		return nil, ErrProjectConflict
	}
	if i.IdempotencyKeyHash != "" {
		for _, v := range s.projects {
			if v.Scope == i.Scope && v.IdempotencyKeyHash == i.IdempotencyKeyHash {
				return nil, ErrProjectIdempotency
			}
		}
	}
	s.projects[k] = cloneProject(i)
	p := appendMemoryActivityLocked(s, e)
	return cloneActivityEvent(p), nil
}
func (s *MemoryStore) GetProject(_ context.Context, scope Scope, id string) (*Project, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.projects[projectKey(scope, id)]
	if i == nil {
		return nil, ErrProjectNotFound
	}
	return cloneProject(i), nil
}
func (s *MemoryStore) GetProjectByIdempotency(_ context.Context, scope Scope, key string) (*Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, i := range s.projects {
		if i.Scope == scope && i.IdempotencyKeyHash == key {
			return cloneProject(i), nil
		}
	}
	return nil, ErrProjectNotFound
}
func (s *MemoryStore) ListProjects(_ context.Context, f ProjectFilter) ([]*Project, error) {
	if err := f.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := map[ProjectStatus]bool{}
	for _, v := range f.Statuses {
		status[v] = true
	}
	out := []*Project{}
	for _, i := range s.projects {
		if i.Scope != f.Scope {
			continue
		}
		if f.Owner != nil && i.Owner != *f.Owner {
			continue
		}
		if len(status) > 0 && !status[i.Status] {
			continue
		}
		if f.ObjectiveID != "" && !containsString(i.ObjectiveRefs, f.ObjectiveID) {
			continue
		}
		out = append(out, cloneProject(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt.After(out[b].UpdatedAt) })
	start := f.Offset
	if start > len(out) {
		start = len(out)
	}
	end := len(out)
	if f.Limit > 0 && start+f.Limit < end {
		end = start + f.Limit
	}
	return out[start:end], nil
}
func (s *MemoryStore) UpdateProjectWithEvent(_ context.Context, i *Project, expected int64, e *ActivityEvent) (*ActivityEvent, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := projectKey(i.Scope, i.ID)
	cur := s.projects[k]
	if cur == nil {
		return nil, ErrProjectNotFound
	}
	if cur.Revision != expected || i.Revision != expected+1 {
		return nil, ErrProjectConflict
	}
	s.projects[k] = cloneProject(i)
	p := appendMemoryActivityLocked(s, e)
	return cloneActivityEvent(p), nil
}
func (s *MemoryStore) UpdateProject(_ context.Context, i *Project, expected int64) error {
	if err := i.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := projectKey(i.Scope, i.ID)
	current := s.projects[k]
	if current == nil {
		return ErrProjectNotFound
	}
	if current.Revision != expected || i.Revision != expected+1 {
		return ErrProjectConflict
	}
	s.projects[k] = cloneProject(i)
	return nil
}
func projectContainsString(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}

var _ ProjectStore = (*MemoryStore)(nil)
