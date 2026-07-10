package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ConversationArchiveSource identifies the immutable source record set used
// for a one-way import. OpenSeal stores a source reference on every imported
// message so hosts can remove old execution systems without erasing audit
// provenance.
type ConversationArchiveSource struct {
	System       string `json:"system"`
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceId"`
}

func (s ConversationArchiveSource) Validate() error {
	if !validOpaqueIdentifier(s.System, 80) || !validOpaqueIdentifier(s.ResourceType, 80) ||
		!validOpaqueIdentifier(s.ResourceID, 128) {
		return fmt.Errorf("%w: archive source system, resource type, and resource id are required", ErrInvalidConversation)
	}
	return nil
}

func (s ConversationArchiveSource) reference(recordID string) ConversationReference {
	return ConversationReference{
		Kind: ConversationReferenceExternalSource,
		ID: strings.Join([]string{
			strings.TrimSpace(s.System), strings.TrimSpace(s.ResourceType), strings.TrimSpace(s.ResourceID), strings.TrimSpace(recordID),
		}, ":"),
	}
}

// ConversationArchiveMessage is a user-visible collaboration fact selected
// by the importing host. Hidden reasoning and procedural tool traces must not
// be supplied as channel messages; preserve those through governed artifacts
// or activity records instead.
type ConversationArchiveMessage struct {
	RecordID          string
	Sender            ConversationParticipant
	SenderDisplayName string
	Intent            ConversationMessageIntent
	Content           string
	Audience          ConversationAudience
	Mentions          []ConversationParticipant
	References        []ConversationReference
	ReplyToRecordID   string
	RequiresResponse  bool
	CreatedAt         time.Time
}

type ImportConversationArchiveRequest struct {
	Scope          Scope
	Owner          ObjectiveOwner
	Title          string
	Source         ConversationArchiveSource
	Messages       []ConversationArchiveMessage
	IdempotencyKey string
}

type ConversationArchiveImportResult struct {
	Conversation     *Conversation     `json:"conversation"`
	Messages         []*ChannelMessage `json:"messages"`
	ImportedMessages int               `json:"importedMessages"`
	ReplayedMessages int               `json:"replayedMessages"`
	Replayed         bool              `json:"replayed"`
}

// ImportArchive imports a historical channel without scheduling conversation
// Runs. It is crash-resumable: conversation and message identities derive from
// the source and every commit is independently idempotent.
func (s *ConversationService) ImportArchive(ctx context.Context, req ImportConversationArchiveRequest) (*ConversationArchiveImportResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, err
	}
	if err := req.Source.Validate(); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	title := strings.TrimSpace(req.Title)
	if key == "" || len(key) > 256 || title == "" || len(title) > 240 || len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: archive title, messages, and idempotency key are required", ErrInvalidConversation)
	}

	messages := append([]ConversationArchiveMessage(nil), req.Messages...)
	sort.SliceStable(messages, func(i, j int) bool {
		if !messages[i].CreatedAt.Equal(messages[j].CreatedAt) {
			return messages[i].CreatedAt.Before(messages[j].CreatedAt)
		}
		return messages[i].RecordID < messages[j].RecordID
	})
	records := make(map[string]string, len(messages))
	for index := range messages {
		message := &messages[index]
		message.RecordID = strings.TrimSpace(message.RecordID)
		if !validOpaqueIdentifier(message.RecordID, 128) || message.CreatedAt.IsZero() {
			return nil, fmt.Errorf("%w: every archive message requires a portable record id and timestamp", ErrInvalidConversation)
		}
		if _, exists := records[message.RecordID]; exists {
			return nil, fmt.Errorf("%w: duplicate archive message record id", ErrInvalidConversation)
		}
		records[message.RecordID] = stableConversationID(req.Scope, key+":"+message.RecordID, "archive-message")
	}

	conversation, replayed, err := s.importArchiveConversation(ctx, req, messages[0].CreatedAt.UTC(), key, title)
	if err != nil {
		return nil, err
	}
	result := &ConversationArchiveImportResult{Conversation: conversation, Replayed: replayed}
	for _, archived := range messages {
		current, err := s.GetConversation(ctx, req.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		replyToID := ""
		if archived.ReplyToRecordID != "" {
			var ok bool
			replyToID, ok = records[strings.TrimSpace(archived.ReplyToRecordID)]
			if !ok {
				return nil, fmt.Errorf("%w: archive reply references an unknown source record", ErrMessageConflict)
			}
		}
		messageKey := key + ":message:" + archived.RecordID
		references := cloneConversationReferences(archived.References)
		references = append(references, req.Source.reference(archived.RecordID))
		message := &ChannelMessage{
			ID: records[archived.RecordID], Scope: req.Scope, ConversationID: conversation.ID,
			Sequence: current.LastSequence + 1, Sender: archived.Sender, Intent: archived.Intent,
			SenderDisplayName: strings.TrimSpace(archived.SenderDisplayName),
			Content:           strings.TrimSpace(archived.Content), Audience: archived.Audience,
			ReplyToMessageID: replyToID, Mentions: cloneParticipants(archived.Mentions), References: references,
			RequiresResponse: archived.RequiresResponse, IdempotencyKey: messageKey, Historical: true, CreatedAt: archived.CreatedAt.UTC(),
		}
		if replyToID != "" {
			reply, getErr := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, replyToID)
			if getErr != nil {
				return nil, getErr
			}
			if reply == nil {
				return nil, fmt.Errorf("%w: archive reply source record has not been imported", ErrMessageConflict)
			}
			message.ThreadRootID = reply.ThreadRootID
			if message.ThreadRootID == "" {
				message.ThreadRootID = reply.ID
			}
		}
		if err := message.Validate(); err != nil {
			return nil, err
		}
		existing, findErr := s.store.FindChannelMessageByIdempotencyKey(ctx, req.Scope, conversation.ID, messageKey)
		if findErr != nil {
			return nil, findErr
		}
		if existing != nil {
			if !sameArchivedChannelMessage(existing, message) {
				return nil, ErrMessageConflict
			}
			result.Messages = append(result.Messages, cloneChannelMessage(existing))
			result.ReplayedMessages++
			continue
		}
		updated := cloneConversation(current)
		updated.LastSequence = message.Sequence
		updated.Revision++
		if message.CreatedAt.After(updated.UpdatedAt) {
			updated.UpdatedAt = message.CreatedAt
		}
		committed, commitErr := s.store.CommitChannelMessage(ctx, ChannelMessageCommitRecord{
			Conversation: updated, ExpectedRevision: current.Revision, Message: message,
		})
		if commitErr != nil {
			return nil, commitErr
		}
		result.Conversation = committed.Conversation
		result.Messages = append(result.Messages, committed.Message)
		result.ImportedMessages++
	}
	result.Replayed = result.Replayed && result.ImportedMessages == 0
	return result, nil
}

func (s *ConversationService) importArchiveConversation(
	ctx context.Context,
	req ImportConversationArchiveRequest,
	createdAt time.Time,
	key string,
	title string,
) (*Conversation, bool, error) {
	if existing, err := s.store.FindConversationByIdempotencyKey(ctx, req.Scope, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.Owner != req.Owner || existing.Title != title || !existing.CreatedAt.Equal(createdAt) {
			return nil, false, ErrMessageConflict
		}
		return existing, true, nil
	}
	conversation := &Conversation{
		ID: stableConversationID(req.Scope, key, "archive-conversation"), Scope: req.Scope, Owner: req.Owner,
		Title: title, Status: ConversationStatusActive, Revision: 1, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.CreateConversation(ctx, conversation, key)
}

func sameArchivedChannelMessage(existing, expected *ChannelMessage) bool {
	if existing == nil || expected == nil {
		return false
	}
	left := cloneChannelMessage(existing)
	right := cloneChannelMessage(expected)
	right.Sequence = left.Sequence
	return reflect.DeepEqual(left, right)
}
