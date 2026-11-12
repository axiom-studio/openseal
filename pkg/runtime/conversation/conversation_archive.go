package conversation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ArchiveSource identifies the immutable source record set used
// for a one-way import. OpenSeal stores a source reference on every imported
// message so hosts can remove old execution systems without erasing audit
// provenance.
type ArchiveSource struct {
	System       string `json:"system"`
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceId"`
}

func (s ArchiveSource) Validate() error {
	if !models.IsValidOpaqueIdentifier(s.System, 80) || !models.IsValidOpaqueIdentifier(s.ResourceType, 80) ||
		!models.IsValidOpaqueIdentifier(s.ResourceID, 128) {
		return fmt.Errorf("%w: archive source system, resource type, and resource id are required", models.ErrInvalidConversation)
	}
	return nil
}

func (s ArchiveSource) reference(recordID string) models.ConversationReference {
	parts := []string{strings.TrimSpace(s.System), strings.TrimSpace(s.ResourceType), strings.TrimSpace(s.ResourceID)}
	if strings.TrimSpace(recordID) != "" {
		parts = append(parts, strings.TrimSpace(recordID))
	}
	return models.ConversationReference{
		Kind: models.ConversationReferenceExternalSource,
		ID:   strings.Join(parts, ":"),
	}
}

// ArchiveMessage is a user-visible collaboration fact selected
// by the importing host. Hidden reasoning and procedural tool traces must not
// be supplied as channel messages; preserve those through governed artifacts
// or activity records instead.
type ArchiveMessage struct {
	RecordID           string
	Sender             models.ConversationParticipant
	SenderDisplayName  string
	Intent             models.ConversationMessageIntent
	Content            string
	Audience           models.ConversationAudience
	Mentions           []models.ConversationParticipant
	References         []models.ConversationReference
	ReplyToRecordID    string
	BroadcastToChannel bool
	RequiresResponse   bool
	CreatedAt          time.Time
}

type ImportConversationArchiveRequest struct {
	Scope          models.Scope
	Owner          models.ObjectiveOwner
	Title          string
	Source         ArchiveSource
	Messages       []ArchiveMessage
	IdempotencyKey string
}

type ArchiveImportResult struct {
	Conversation     *models.Conversation     `json:"conversation"`
	Messages         []*models.ChannelMessage `json:"messages"`
	ImportedMessages int                      `json:"importedMessages"`
	ReplayedMessages int                      `json:"replayedMessages"`
	Replayed         bool                     `json:"replayed"`
}

// ImportArchive imports a historical channel without scheduling conversation
// Runs. It is crash-resumable: conversation and message identities derive from
// the source and every commit is independently idempotent.
func (s *Service) ImportArchive(ctx context.Context, req ImportConversationArchiveRequest) (*ArchiveImportResult, error) {
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
		return nil, fmt.Errorf("%w: archive title, messages, and idempotency key are required", models.ErrInvalidConversation)
	}

	messages := append([]ArchiveMessage(nil), req.Messages...)
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
		if !models.IsValidOpaqueIdentifier(message.RecordID, 128) || message.CreatedAt.IsZero() {
			return nil, fmt.Errorf("%w: every archive message requires a portable record id and timestamp", models.ErrInvalidConversation)
		}
		if _, exists := records[message.RecordID]; exists {
			return nil, fmt.Errorf("%w: duplicate archive message record id", models.ErrInvalidConversation)
		}
		records[message.RecordID] = models.StableConversationID(req.Scope, key+":"+message.RecordID, "archive-message")
	}

	conversation, replayed, err := s.importArchiveConversation(ctx, req, messages[0].CreatedAt.UTC(), key, title)
	if err != nil {
		return nil, err
	}
	result := &ArchiveImportResult{Conversation: conversation, Replayed: replayed}
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
				return nil, fmt.Errorf("%w: archive reply references an unknown source record", models.ErrMessageConflict)
			}
		}
		messageKey := key + ":message:" + archived.RecordID
		references := models.CloneConversationReferences(archived.References)
		references = append(references, req.Source.reference(archived.RecordID))
		message := &models.ChannelMessage{
			ID: records[archived.RecordID], Scope: req.Scope, ConversationID: conversation.ID,
			Sequence: current.LastSequence + 1, Sender: archived.Sender, Intent: archived.Intent,
			SenderDisplayName: strings.TrimSpace(archived.SenderDisplayName),
			Content:           strings.TrimSpace(archived.Content), Audience: archived.Audience,
			ReplyToMessageID: replyToID, BroadcastToChannel: archived.BroadcastToChannel,
			Mentions: models.CloneConversationParticipants(archived.Mentions), References: references,
			RequiresResponse: archived.RequiresResponse, IdempotencyKey: messageKey, Historical: true, CreatedAt: archived.CreatedAt.UTC(),
		}
		if replyToID != "" {
			reply, getErr := s.store.GetChannelMessage(ctx, req.Scope, conversation.ID, replyToID)
			if getErr != nil {
				return nil, getErr
			}
			if reply == nil {
				return nil, fmt.Errorf("%w: archive reply source record has not been imported", models.ErrMessageConflict)
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
				return nil, models.ErrMessageConflict
			}
			result.Messages = append(result.Messages, models.CloneChannelMessage(existing))
			result.ReplayedMessages++
			continue
		}
		updated := models.CloneConversation(current)
		updated.LastSequence = message.Sequence
		updated.Revision++
		if message.CreatedAt.After(updated.UpdatedAt) {
			updated.UpdatedAt = message.CreatedAt
		}
		committed, commitErr := s.store.CommitChannelMessage(ctx, models.ChannelMessageCommitRecord{
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

func (s *Service) importArchiveConversation(
	ctx context.Context,
	req ImportConversationArchiveRequest,
	createdAt time.Time,
	key string,
	title string,
) (*models.Conversation, bool, error) {
	if existing, err := s.store.FindConversationByIdempotencyKey(ctx, req.Scope, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.Owner != req.Owner || existing.Title != title || !existing.CreatedAt.Equal(createdAt) ||
			!reflect.DeepEqual(existing.Origin, archiveConversationOrigin(req.Source)) {
			return nil, false, models.ErrMessageConflict
		}
		return existing, true, nil
	}
	conversation := &models.Conversation{
		ID: models.StableConversationID(req.Scope, key, "archive-conversation"), Scope: req.Scope, Owner: req.Owner,
		Title: title, Origin: archiveConversationOrigin(req.Source), Status: models.ConversationStatusActive,
		ApprovalMode: models.DefaultConversationApprovalMode, Revision: 1, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	return s.store.CreateConversation(ctx, conversation, key)
}

func archiveConversationOrigin(source ArchiveSource) *models.ConversationReference {
	reference := source.reference("")
	return &reference
}

func sameArchivedChannelMessage(existing, expected *models.ChannelMessage) bool {
	if existing == nil || expected == nil {
		return false
	}
	left := models.CloneChannelMessage(existing)
	right := models.CloneChannelMessage(expected)
	right.Sequence = left.Sequence
	return reflect.DeepEqual(left, right)
}
