package runtime

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"
)

func conversationStoreKey(scope Scope, id string) string { return scope.key() + ":" + id }

func conversationIdempotencyKey(scope Scope, key string) string { return scope.key() + ":" + key }

func channelMessageStoreKey(scope Scope, conversationID, messageID string) string {
	return scope.key() + ":" + conversationID + ":" + messageID
}

func channelMessageIdempotencyKey(scope Scope, conversationID, key string) string {
	return scope.key() + ":" + conversationID + ":" + key
}

func conversationParticipantStoreKey(scope Scope, conversationID string, participant ConversationParticipant) string {
	return scope.key() + ":" + conversationID + ":" + string(participant.Type) + ":" + participant.ID
}

func (s *MemoryStore) CreateConversation(_ context.Context, conversation *Conversation, idempotencyKey string) (*Conversation, bool, error) {
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, false, ErrInvalidConversation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := conversationIdempotencyKey(conversation.Scope, key)
	if existingID := s.conversationKeys[identityKey]; existingID != "" {
		existing := s.conversations[conversationStoreKey(conversation.Scope, existingID)]
		if existing == nil || existing.Owner != conversation.Owner || existing.Title != conversation.Title ||
			!reflect.DeepEqual(existing.Origin, conversation.Origin) {
			return nil, false, ErrMessageConflict
		}
		return cloneConversation(existing), true, nil
	}
	storeKey := conversationStoreKey(conversation.Scope, conversation.ID)
	if s.conversations[storeKey] != nil {
		return nil, false, ErrMessageConflict
	}
	s.conversations[storeKey] = cloneConversation(conversation)
	s.conversationKeys[identityKey] = conversation.ID
	return cloneConversation(conversation), false, nil
}

func (s *MemoryStore) GetConversation(_ context.Context, scope Scope, conversationID string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversation(s.conversations[conversationStoreKey(scope, strings.TrimSpace(conversationID))]), nil
}

func (s *MemoryStore) FindConversationByIdempotencyKey(_ context.Context, scope Scope, key string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.conversationKeys[conversationIdempotencyKey(scope, strings.TrimSpace(key))]
	return cloneConversation(s.conversations[conversationStoreKey(scope, id)]), nil
}

func (s *MemoryStore) ListConversations(_ context.Context, filter ConversationFilter) ([]*Conversation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Conversation, 0)
	for _, conversation := range s.conversations {
		if conversation.Scope != filter.Scope || (filter.Owner != nil && conversation.Owner != *filter.Owner) ||
			(len(filter.Statuses) > 0 && !containsConversationStatus(filter.Statuses, conversation.Status)) {
			continue
		}
		result = append(result, cloneConversation(conversation))
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageConversations(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) CommitChannelMessage(_ context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if err := validateChannelMessageCommitRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conversationKey := conversationStoreKey(record.Conversation.Scope, record.Conversation.ID)
	current := s.conversations[conversationKey]
	if current == nil {
		return nil, ErrConversationNotFound
	}
	messageKey := channelMessageStoreKey(record.Message.Scope, record.Message.ConversationID, record.Message.ID)
	idempotencyKey := channelMessageIdempotencyKey(record.Message.Scope, record.Message.ConversationID, record.Message.IdempotencyKey)
	if existingID := s.channelMessageKeys[idempotencyKey]; existingID != "" {
		existing := s.channelMessageIDs[channelMessageStoreKey(record.Message.Scope, record.Message.ConversationID, existingID)]
		if !sameChannelMessage(existing, record.Message) {
			return nil, ErrMessageConflict
		}
		return &ChannelMessageCommitResult{Conversation: cloneConversation(current), Message: cloneChannelMessage(existing), Replayed: true}, nil
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 ||
		record.Message.Sequence != current.LastSequence+1 || record.Conversation.LastSequence != record.Message.Sequence {
		return nil, ErrRevisionConflict
	}
	if s.channelMessageIDs[messageKey] != nil {
		return nil, ErrMessageConflict
	}
	s.conversations[conversationKey] = cloneConversation(record.Conversation)
	s.channelMessages[conversationKey] = append(s.channelMessages[conversationKey], cloneChannelMessage(record.Message))
	s.channelMessageIDs[messageKey] = cloneChannelMessage(record.Message)
	s.channelMessageKeys[idempotencyKey] = record.Message.ID
	return &ChannelMessageCommitResult{Conversation: cloneConversation(record.Conversation), Message: cloneChannelMessage(record.Message)}, nil
}

func (s *MemoryStore) GetChannelMessage(_ context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneChannelMessage(s.channelMessageIDs[channelMessageStoreKey(scope, conversationID, messageID)]), nil
}

func (s *MemoryStore) FindChannelMessageByIdempotencyKey(_ context.Context, scope Scope, conversationID, key string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.channelMessageKeys[channelMessageIdempotencyKey(scope, conversationID, strings.TrimSpace(key))]
	return cloneChannelMessage(s.channelMessageIDs[channelMessageStoreKey(scope, conversationID, id)]), nil
}

func (s *MemoryStore) ListChannelMessages(_ context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	conversationKey := conversationStoreKey(filter.Scope, strings.TrimSpace(filter.ConversationID))
	if s.conversations[conversationKey] == nil {
		return nil, ErrConversationNotFound
	}
	result := make([]*ChannelMessage, 0)
	for _, message := range s.channelMessages[conversationKey] {
		if message.Sequence <= filter.AfterSequence || (filter.BeforeSequence > 0 && message.Sequence >= filter.BeforeSequence) ||
			(filter.ThreadRootID != "" && message.ThreadRootID != filter.ThreadRootID && message.ID != filter.ThreadRootID) ||
			(len(filter.Intents) > 0 && !containsConversationIntent(filter.Intents, message.Intent)) {
			continue
		}
		result = append(result, cloneChannelMessage(message))
	}
	sort.Slice(result, func(i, j int) bool {
		if filter.Descending {
			return result[i].Sequence > result[j].Sequence
		}
		return result[i].Sequence < result[j].Sequence
	})
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) CommitParticipationRound(_ context.Context, record ParticipationRoundCommitRecord) (*ParticipationRoundResult, error) {
	if err := validateParticipationRoundCommitRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conversationKey := conversationStoreKey(record.Conversation.Scope, record.Conversation.ID)
	current := s.conversations[conversationKey]
	if current == nil {
		return nil, ErrConversationNotFound
	}
	key := channelMessageIdempotencyKey(record.Round.Scope, record.Round.ConversationID, record.Round.IdempotencyKey)
	if existingID := s.participationKeys[key]; existingID != "" {
		existing := s.participationRounds[channelMessageStoreKey(record.Round.Scope, record.Round.ConversationID, existingID)]
		if existing == nil || !sameParticipationRound(existing.Round, record.Round) {
			return nil, ErrMessageConflict
		}
		replayed := cloneParticipationRoundResult(existing, true)
		replayed.Conversation = cloneConversation(current)
		return replayed, nil
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 {
		return nil, ErrRevisionConflict
	}
	expectedSequence := current.LastSequence
	seenMessages := make(map[string]struct{}, len(record.Messages))
	for _, message := range record.Messages {
		expectedSequence++
		if message.Sequence != expectedSequence || message.ParticipationRoundID != record.Round.ID {
			return nil, ErrInvalidConversation
		}
		storeKey := channelMessageStoreKey(message.Scope, message.ConversationID, message.ID)
		if s.channelMessageIDs[storeKey] != nil {
			return nil, ErrMessageConflict
		}
		if _, exists := seenMessages[message.ID]; exists {
			return nil, ErrMessageConflict
		}
		seenMessages[message.ID] = struct{}{}
	}
	if record.Conversation.LastSequence != expectedSequence {
		return nil, ErrInvalidConversation
	}
	s.conversations[conversationKey] = cloneConversation(record.Conversation)
	for _, message := range record.Messages {
		cloned := cloneChannelMessage(message)
		s.channelMessages[conversationKey] = append(s.channelMessages[conversationKey], cloned)
		s.channelMessageIDs[channelMessageStoreKey(message.Scope, message.ConversationID, message.ID)] = cloneChannelMessage(message)
		s.channelMessageKeys[channelMessageIdempotencyKey(message.Scope, message.ConversationID, message.IdempotencyKey)] = message.ID
	}
	result := &ParticipationRoundResult{
		Conversation: cloneConversation(record.Conversation), Round: cloneParticipationRound(record.Round), Messages: cloneChannelMessages(record.Messages),
	}
	roundKey := channelMessageStoreKey(record.Round.Scope, record.Round.ConversationID, record.Round.ID)
	s.participationRounds[roundKey] = cloneParticipationRoundResult(result, false)
	s.participationKeys[key] = record.Round.ID
	return cloneParticipationRoundResult(result, false), nil
}

func (s *MemoryStore) FindParticipationRoundByIdempotencyKey(_ context.Context, scope Scope, conversationID, key string) (*ParticipationRoundResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.participationKeys[channelMessageIdempotencyKey(scope, conversationID, strings.TrimSpace(key))]
	result := cloneParticipationRoundResult(s.participationRounds[channelMessageStoreKey(scope, conversationID, id)], true)
	if result != nil {
		result.Conversation = cloneConversation(s.conversations[conversationStoreKey(scope, conversationID)])
	}
	return result, nil
}

func (s *MemoryStore) GetParticipationRound(_ context.Context, scope Scope, conversationID, roundID string) (*ParticipationRoundResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := cloneParticipationRoundResult(s.participationRounds[channelMessageStoreKey(scope, conversationID, roundID)], false)
	if result != nil {
		result.Conversation = cloneConversation(s.conversations[conversationStoreKey(scope, conversationID)])
	}
	return result, nil
}

func (s *MemoryStore) ListParticipationRounds(_ context.Context, filter ParticipationRoundFilter) ([]*ParticipationRoundResult, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ParticipationRoundResult, 0)
	for _, round := range s.participationRounds {
		if round.Round.Scope != filter.Scope || round.Round.ConversationID != filter.ConversationID {
			continue
		}
		cloned := cloneParticipationRoundResult(round, false)
		cloned.Conversation = cloneConversation(s.conversations[conversationStoreKey(filter.Scope, filter.ConversationID)])
		result = append(result, cloned)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Round.CommittedAt.Equal(result[j].Round.CommittedAt) {
			return result[i].Round.CommittedAt.After(result[j].Round.CommittedAt)
		}
		return result[i].Round.ID < result[j].Round.ID
	})
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(result) {
		return []*ParticipationRoundResult{}, nil
	}
	result = result[offset:]
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) GetConversationCursor(_ context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationCursor(s.conversationCursors[conversationParticipantStoreKey(scope, conversationID, participant)]), nil
}

func (s *MemoryStore) ListConversationCursors(_ context.Context, scope Scope, conversationID string) ([]*ConversationCursor, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(conversationID, 128) {
		return nil, ErrInvalidConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.conversations[conversationStoreKey(scope, conversationID)] == nil {
		return nil, ErrConversationNotFound
	}
	result := make([]*ConversationCursor, 0)
	for _, cursor := range s.conversationCursors {
		if cursor.Scope == scope && cursor.ConversationID == conversationID {
			result = append(result, cloneConversationCursor(cursor))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Participant.Type != result[j].Participant.Type {
			return result[i].Participant.Type < result[j].Participant.Type
		}
		return result[i].Participant.ID < result[j].Participant.ID
	})
	return result, nil
}

func (s *MemoryStore) PutConversationCursor(_ context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error) {
	if record.Cursor == nil {
		return nil, false, ErrConversationCursorConflict
	}
	if err := record.Cursor.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation := s.conversations[conversationStoreKey(record.Cursor.Scope, record.Cursor.ConversationID)]
	if conversation == nil {
		return nil, false, ErrConversationNotFound
	}
	if record.Cursor.DeliveredSequence > conversation.LastSequence {
		return nil, false, ErrConversationCursorConflict
	}
	key := conversationParticipantStoreKey(record.Cursor.Scope, record.Cursor.ConversationID, record.Cursor.Participant)
	current := s.conversationCursors[key]
	if current != nil && current.DeliveredSequence == record.Cursor.DeliveredSequence && current.ReadSequence == record.Cursor.ReadSequence {
		return cloneConversationCursor(current), true, nil
	}
	if (current == nil && record.ExpectedRevision != 0) || (current != nil && (current.Revision != record.ExpectedRevision || record.Cursor.Revision != current.Revision+1)) {
		return nil, false, ErrConversationCursorConflict
	}
	s.conversationCursors[key] = cloneConversationCursor(record.Cursor)
	return cloneConversationCursor(record.Cursor), false, nil
}

func (s *MemoryStore) GetConversationPresence(_ context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationPresence(s.conversationPresence[conversationParticipantStoreKey(scope, conversationID, participant)]), nil
}

func (s *MemoryStore) PutConversationPresence(_ context.Context, record ConversationPresenceRecord) (*ConversationPresence, error) {
	if record.Presence == nil {
		return nil, ErrConversationPresenceConflict
	}
	if err := record.Presence.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conversations[conversationStoreKey(record.Presence.Scope, record.Presence.ConversationID)] == nil {
		return nil, ErrConversationNotFound
	}
	key := conversationParticipantStoreKey(record.Presence.Scope, record.Presence.ConversationID, record.Presence.Participant)
	current := s.conversationPresence[key]
	if current != nil && current.ExpiresAt.After(record.Presence.UpdatedAt) {
		if current.Revision != record.ExpectedRevision || current.LeaseID != record.Presence.LeaseID || record.Presence.Revision != current.Revision+1 {
			return nil, ErrConversationPresenceConflict
		}
	} else if record.ExpectedRevision != 0 || record.Presence.Revision != 1 {
		return nil, ErrConversationPresenceConflict
	}
	s.conversationPresence[key] = cloneConversationPresence(record.Presence)
	return cloneConversationPresence(record.Presence), nil
}

func (s *MemoryStore) ReleaseConversationPresence(_ context.Context, request ReleaseConversationPresenceRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := request.Participant.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationParticipantStoreKey(request.Scope, request.ConversationID, request.Participant)
	current := s.conversationPresence[key]
	if current == nil {
		return nil
	}
	if strings.TrimSpace(request.LeaseID) == "" || request.LeaseID != current.LeaseID {
		return ErrConversationPresenceConflict
	}
	delete(s.conversationPresence, key)
	return nil
}

func (s *MemoryStore) ListConversationPresence(_ context.Context, scope Scope, conversationID string, activeAt time.Time) ([]*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ConversationPresence, 0)
	for _, presence := range s.conversationPresence {
		if presence.Scope == scope && presence.ConversationID == conversationID && presence.ExpiresAt.After(activeAt) {
			result = append(result, cloneConversationPresence(presence))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left := string(result[i].Participant.Type) + ":" + result[i].Participant.ID
		right := string(result[j].Participant.Type) + ":" + result[j].Participant.ID
		return left < right
	})
	return result, nil
}

func validateChannelMessageCommitRecord(record ChannelMessageCommitRecord) error {
	if record.Conversation == nil || record.Message == nil {
		return ErrInvalidConversation
	}
	if err := record.Conversation.Validate(); err != nil {
		return err
	}
	if err := record.Message.Validate(); err != nil {
		return err
	}
	if record.Conversation.Scope != record.Message.Scope || record.Conversation.ID != record.Message.ConversationID || record.ExpectedRevision <= 0 {
		return ErrInvalidConversation
	}
	return nil
}

func validateParticipationRoundCommitRecord(record ParticipationRoundCommitRecord) error {
	if record.Conversation == nil || record.Round == nil || record.ExpectedRevision <= 0 {
		return ErrInvalidConversation
	}
	if err := record.Conversation.Validate(); err != nil {
		return err
	}
	if err := record.Round.Validate(); err != nil {
		return err
	}
	if record.Conversation.Scope != record.Round.Scope || record.Conversation.ID != record.Round.ConversationID {
		return ErrInvalidConversation
	}
	for _, message := range record.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
		if message.Scope != record.Round.Scope || message.ConversationID != record.Round.ConversationID {
			return ErrInvalidConversation
		}
	}
	return nil
}

func sameChannelMessage(left, right *ChannelMessage) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftCopy, rightCopy := *left, *right
	leftCopy.Sequence, rightCopy.Sequence = 0, 0
	leftCopy.CreatedAt, rightCopy.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func sameParticipationRound(left, right *ParticipationRound) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.ID == right.ID && left.Scope == right.Scope && left.ConversationID == right.ConversationID &&
		left.TriggerMessageID == right.TriggerMessageID && sameConversationArbitrationPolicy(left.Policy, right.Policy) && left.IdempotencyKey == right.IdempotencyKey &&
		reflect.DeepEqual(left.Proposals, right.Proposals)
}

func cloneParticipationRoundResult(in *ParticipationRoundResult, replayed bool) *ParticipationRoundResult {
	if in == nil {
		return nil
	}
	return &ParticipationRoundResult{
		Conversation: cloneConversation(in.Conversation), Round: cloneParticipationRound(in.Round),
		Messages: cloneChannelMessages(in.Messages), Replayed: replayed,
	}
}

func containsConversationStatus(values []ConversationStatus, wanted ConversationStatus) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsConversationIntent(values []ConversationMessageIntent, wanted ConversationMessageIntent) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func pageConversations(values []*Conversation, offset, limit int) []*Conversation {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*Conversation{}
	}
	values = values[offset:]
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}

var _ ConversationStore = (*MemoryStore)(nil)
