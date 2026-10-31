package runtime

import (
	"context"
	"time"
)

var _ SpaceStore = (*MemoryStore)(nil)

func (s *MemoryStore) EnsurePersonalSpace(_ context.Context, scope Scope, now time.Time) (*Space, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	space, created := s.ensurePersonalSpaceLocked(scope, now)
	return cloneSpace(space), created, nil
}

func (s *MemoryStore) ensurePersonalSpaceLocked(scope Scope, now time.Time) (*Space, bool) {
	id := PersonalSpaceID(scope)
	if existing := s.spaces[scope][id]; existing != nil {
		return existing, false
	}
	if s.spaces[scope] == nil {
		s.spaces[scope] = make(map[string]*Space)
	}
	space := newPersonalSpace(scope, now)
	s.spaces[scope][id] = space
	return space, true
}

func (s *MemoryStore) CreateSpace(_ context.Context, space *Space) (*Space, error) {
	if err := space.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaces[space.Scope] == nil {
		s.spaces[space.Scope] = make(map[string]*Space)
	}
	if s.spaces[space.Scope][space.ID] != nil {
		return nil, ErrInvalidSpace
	}
	created := cloneSpace(space)
	created.IsPersonal = false
	created.Position = 0
	for _, existing := range s.spaces[space.Scope] {
		if existing.Position >= created.Position {
			created.Position = existing.Position + 1
		}
	}
	s.spaces[space.Scope][space.ID] = created
	return cloneSpace(created), nil
}

func (s *MemoryStore) GetSpace(_ context.Context, scope Scope, spaceID string) (*Space, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	space := s.spaces[scope][spaceID]
	if space == nil {
		return nil, ErrSpaceNotFound
	}
	return cloneSpace(space), nil
}

func (s *MemoryStore) ListSpaces(_ context.Context, scope Scope) ([]*Space, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.orderedSpacesLocked(scope, true), nil
}

func (s *MemoryStore) orderedSpacesLocked(scope Scope, clone bool) []*Space {
	result := make([]*Space, 0, len(s.spaces[scope]))
	for _, space := range s.spaces[scope] {
		if clone {
			space = cloneSpace(space)
		}
		result = append(result, space)
	}
	sortSpaces(result)
	return result
}

func (s *MemoryStore) UpdateSpace(_ context.Context, scope Scope, spaceID string, patch SpacePatch, now time.Time) (*Space, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	space := s.spaces[scope][spaceID]
	if space == nil {
		return nil, ErrSpaceNotFound
	}
	updated := cloneSpace(space)
	applySpacePatch(updated, patch, now)
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	if patch.Position != nil {
		for id, position := range moveSpace(s.orderedSpacesLocked(scope, false), spaceID, *patch.Position) {
			if id == spaceID {
				updated.Position = position
			} else {
				s.spaces[scope][id].Position = position
			}
		}
	}
	s.spaces[scope][spaceID] = updated
	return cloneSpace(updated), nil
}

func (s *MemoryStore) DeleteSpace(_ context.Context, scope Scope, spaceID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	space := s.spaces[scope][spaceID]
	if space == nil {
		return ErrSpaceNotFound
	}
	if space.IsPersonal {
		return ErrPersonalSpaceProtected
	}
	personal, _ := s.ensurePersonalSpaceLocked(scope, now)
	for _, conversation := range s.conversations {
		if conversation.Scope == scope && conversation.SpaceID == spaceID {
			conversation.SpaceID = personal.ID
		}
	}
	kept := make([]*SpaceMember, 0, len(s.spaceMembers[scope]))
	var dropped []*SpaceMember
	for _, member := range s.spaceMembers[scope] {
		if member.SpaceID == spaceID {
			dropped = append(dropped, member)
			continue
		}
		kept = append(kept, member)
	}
	s.spaceMembers[scope] = kept
	delete(s.spaces[scope], spaceID)
	for _, member := range dropped {
		if !s.hasSpaceMembershipLocked(scope, member.Type, member.ID) {
			s.appendSpaceMemberLocked(scope, &SpaceMember{SpaceID: personal.ID, Type: member.Type, ID: member.ID, AddedAt: now})
		}
	}
	return nil
}

func (s *MemoryStore) hasSpaceMembershipLocked(scope Scope, memberType SpaceMemberType, memberID string) bool {
	for _, member := range s.spaceMembers[scope] {
		if member.Type == memberType && member.ID == memberID {
			return true
		}
	}
	return false
}

func (s *MemoryStore) appendSpaceMemberLocked(scope Scope, member *SpaceMember) *SpaceMember {
	created := cloneSpaceMember(member)
	created.Position = 0
	for _, existing := range s.spaceMembers[scope] {
		if existing.SpaceID == member.SpaceID && existing.Position >= created.Position {
			created.Position = existing.Position + 1
		}
	}
	s.spaceMembers[scope] = append(s.spaceMembers[scope], created)
	return created
}

func (s *MemoryStore) ListSpaceMembers(_ context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.spaces[scope][spaceID] == nil {
		return nil, ErrSpaceNotFound
	}
	result := make([]*SpaceMember, 0)
	for _, member := range s.spaceMembers[scope] {
		if member.SpaceID == spaceID {
			result = append(result, cloneSpaceMember(member))
		}
	}
	sortSpaceMembers(result)
	return result, nil
}

func (s *MemoryStore) PutSpaceMember(_ context.Context, scope Scope, member *SpaceMember) (*SpaceMember, bool, error) {
	if err := member.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaces[scope][member.SpaceID] == nil {
		return nil, false, ErrSpaceNotFound
	}
	for _, existing := range s.spaceMembers[scope] {
		if existing.SpaceID == member.SpaceID && existing.Type == member.Type && existing.ID == member.ID {
			return cloneSpaceMember(existing), false, nil
		}
	}
	return cloneSpaceMember(s.appendSpaceMemberLocked(scope, member)), true, nil
}

func (s *MemoryStore) DeleteSpaceMember(_ context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaces[scope][spaceID] == nil {
		return ErrSpaceNotFound
	}
	kept := s.spaceMembers[scope][:0]
	for _, member := range s.spaceMembers[scope] {
		if member.SpaceID == spaceID && member.Type == memberType && member.ID == memberID {
			continue
		}
		kept = append(kept, member)
	}
	s.spaceMembers[scope] = kept
	return nil
}

// resolveConversationSpaceLocked fills an empty SpaceID with the owner's
// first space, else Personal, and verifies an explicit one exists.
func (s *MemoryStore) resolveConversationSpaceLocked(conversation *Conversation) error {
	scope := conversation.Scope
	if conversation.SpaceID != "" {
		if s.spaces[scope][conversation.SpaceID] == nil {
			return ErrSpaceNotFound
		}
		return nil
	}
	var first *Space
	for _, member := range s.spaceMembers[scope] {
		if string(member.Type) != string(conversation.Owner.Type) || member.ID != conversation.Owner.ID {
			continue
		}
		candidate := s.spaces[scope][member.SpaceID]
		if candidate == nil {
			continue
		}
		if first == nil {
			first = candidate
			continue
		}
		pair := []*Space{first, candidate}
		sortSpaces(pair)
		first = pair[0]
	}
	if first == nil {
		first, _ = s.ensurePersonalSpaceLocked(scope, conversation.CreatedAt)
	}
	conversation.SpaceID = first.ID
	return nil
}

func (s *MemoryStore) EnsureMemberHasSpace(_ context.Context, scope Scope, memberType SpaceMemberType, memberID string, now time.Time) (*SpaceMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasSpaceMembershipLocked(scope, memberType, memberID) {
		return nil, nil
	}
	personal, _ := s.ensurePersonalSpaceLocked(scope, now)
	return cloneSpaceMember(s.appendSpaceMemberLocked(scope, &SpaceMember{SpaceID: personal.ID, Type: memberType, ID: memberID, AddedAt: now})), nil
}
