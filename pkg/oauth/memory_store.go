package oauth

import (
	"context"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type MemoryStore struct {
	mu          sync.RWMutex
	sessions    map[string]*AuthorizationSession
	connections map[string]*Connection
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:    make(map[string]*AuthorizationSession),
		connections: make(map[string]*Connection),
	}
}

func (s *MemoryStore) CreateAuthorizationSession(_ context.Context, session *AuthorizationSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := oauthKey(session.Scope, session.ID)
	if s.sessions[key] != nil {
		return ErrRevisionConflict
	}
	s.sessions[key] = cloneSession(session)
	return nil
}

func (s *MemoryStore) GetAuthorizationSession(_ context.Context, scope capability.ScopeReference, id string) (*AuthorizationSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session := s.sessions[oauthKey(scope, id)]
	if session == nil {
		return nil, ErrSessionNotFound
	}
	return cloneSession(session), nil
}

func (s *MemoryStore) UpdateAuthorizationSession(_ context.Context, session *AuthorizationSession, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := oauthKey(session.Scope, session.ID)
	current := s.sessions[key]
	if current == nil {
		return ErrSessionNotFound
	}
	if current.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	s.sessions[key] = cloneSession(session)
	return nil
}

func (s *MemoryStore) GetConnection(_ context.Context, scope capability.ScopeReference, id string) (*Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	connection := s.connections[oauthKey(scope, id)]
	if connection == nil {
		return nil, ErrConnectionNotFound
	}
	return cloneConnection(connection), nil
}

func (s *MemoryStore) UpsertConnection(_ context.Context, connection *Connection, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := oauthKey(connection.Scope, connection.ID)
	current := s.connections[key]
	if current == nil && expectedRevision != 0 || current != nil && current.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	s.connections[key] = cloneConnection(connection)
	return nil
}

func oauthKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + id
}

func cloneSession(value *AuthorizationSession) *AuthorizationSession {
	if value == nil {
		return nil
	}
	result := *value
	result.Requirement.Scopes = append([]string(nil), value.Requirement.Scopes...)
	return &result
}
