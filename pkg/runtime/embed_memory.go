package runtime

import (
	"context"
	"sort"
	"strings"
	"time"
)

func embedInstallationKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
func embedSessionKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
func embedConversationKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func (s *MemoryStore) CreateEmbedInstallation(_ context.Context, value *EmbedInstallation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := embedInstallationKey(value.Scope, value.ID)
	if s.embedInstallations[key] != nil {
		return ErrEmbedRevisionConflict
	}
	if s.embedRoutes[value.PublicRoute] != "" {
		return ErrEmbedRouteConflict
	}
	s.embedInstallations[key] = cloneEmbedInstallation(value)
	s.embedRoutes[value.PublicRoute] = key
	return nil
}

func (s *MemoryStore) GetEmbedInstallation(_ context.Context, scope Scope, id string) (*EmbedInstallation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.embedInstallations[embedInstallationKey(scope, strings.TrimSpace(id))]
	if value == nil {
		return nil, ErrEmbedInstallationNotFound
	}
	return cloneEmbedInstallation(value), nil
}

func (s *MemoryStore) GetEmbedInstallationByRoute(_ context.Context, route string) (*EmbedInstallation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.embedInstallations[s.embedRoutes[strings.TrimSpace(route)]]
	if value == nil {
		return nil, ErrEmbedInstallationNotFound
	}
	return cloneEmbedInstallation(value), nil
}

func (s *MemoryStore) ListEmbedInstallations(_ context.Context, filter EmbedInstallationFilter) ([]*EmbedInstallation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*EmbedInstallation, 0)
	for _, value := range s.embedInstallations {
		if value.Scope == filter.Scope && (filter.DeploymentID == "" || value.DeploymentID == filter.DeploymentID) {
			result = append(result, cloneEmbedInstallation(value))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *MemoryStore) UpdateEmbedInstallation(_ context.Context, value *EmbedInstallation, expected int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := embedInstallationKey(value.Scope, value.ID)
	current := s.embedInstallations[key]
	if current == nil {
		return ErrEmbedInstallationNotFound
	}
	if current.Revision != expected || value.Revision != expected+1 || current.PublicRoute != value.PublicRoute || current.DeploymentID != value.DeploymentID {
		return ErrEmbedRevisionConflict
	}
	s.embedInstallations[key] = cloneEmbedInstallation(value)
	return nil
}

func (s *MemoryStore) CreateEmbedSession(_ context.Context, value *EmbedSession) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := embedSessionKey(value.Scope, value.ID)
	conversationKey := embedConversationKey(value.Scope, value.ConversationID)
	if s.embedSessions[key] != nil || s.embedConversationSessions[conversationKey] != "" {
		return ErrEmbedRevisionConflict
	}
	s.embedSessions[key] = cloneEmbedSession(value)
	s.embedConversationSessions[conversationKey] = key
	return nil
}

func (s *MemoryStore) GetEmbedSession(_ context.Context, scope Scope, id string) (*EmbedSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.embedSessions[embedSessionKey(scope, strings.TrimSpace(id))]
	if value == nil {
		return nil, ErrEmbedSessionNotFound
	}
	return cloneEmbedSession(value), nil
}

func (s *MemoryStore) GetEmbedSessionByConversation(_ context.Context, scope Scope, id string) (*EmbedSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.embedSessions[s.embedConversationSessions[embedConversationKey(scope, strings.TrimSpace(id))]]
	if value == nil {
		return nil, ErrEmbedSessionNotFound
	}
	return cloneEmbedSession(value), nil
}

func (s *MemoryStore) ConsumeEmbedSessionMessage(_ context.Context, scope Scope, id string, maximum int, now time.Time) (*EmbedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := embedSessionKey(scope, strings.TrimSpace(id))
	current := s.embedSessions[key]
	if current == nil {
		return nil, ErrEmbedSessionNotFound
	}
	if current.Status != EmbedSessionActive || !current.ExpiresAt.After(now) {
		return nil, ErrEmbedSessionUnauthorized
	}
	if current.MessageCount >= maximum {
		return nil, ErrEmbedLimitExceeded
	}
	next := cloneEmbedSession(current)
	next.MessageCount++
	next.UpdatedAt = now.UTC()
	s.embedSessions[key] = next
	return cloneEmbedSession(next), nil
}
