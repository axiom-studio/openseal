package runtime

import (
	"context"
	"sort"
)

func cloneSkillSetupRequest(r *SkillSetupRequest) *SkillSetupRequest {
	if r == nil {
		return nil
	}
	c := *r
	c.RequiredActions = append([]string(nil), r.RequiredActions...)
	return &c
}
func (s *MemoryStore) GetSkillSetupRequest(_ context.Context, scope Scope, id string) (*SkillSetupRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidSkillSetup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSkillSetupRequest(s.skillSetupRequests[portfolioKey(scope, id)]), nil
}
func (s *MemoryStore) ListSkillSetupRequests(_ context.Context, scope Scope, deploymentID, conversationID string) ([]*SkillSetupRequest, error) {
	if scope.Validate() != nil || deploymentID == "" || conversationID == "" {
		return nil, ErrInvalidSkillSetup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []*SkillSetupRequest{}
	for _, r := range s.skillSetupRequests {
		if r.Scope == scope && r.DeploymentID == deploymentID && r.ConversationID == conversationID {
			result = append(result, cloneSkillSetupRequest(r))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}
func (s *MemoryStore) SaveSkillSetupRequest(_ context.Context, r *SkillSetupRequest, expected int64) error {
	if r.Validate() != nil || r.Revision != expected+1 {
		return ErrInvalidSkillSetup
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(r.Scope, r.ID)
	current := s.skillSetupRequests[key]
	if current == nil && expected != 0 || current != nil && (current.Revision != expected || current.Status != "pending") {
		return ErrSkillSetupConflict
	}
	s.skillSetupRequests[key] = cloneSkillSetupRequest(r)
	return nil
}
