package externalconversation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	maximumExternalConversationContextBytes = 256 * 1024
)

var errExternalConversationContextMappingMismatch = errors.New("provider history does not belong to the canonical thread")

// ExternalConversationContextHost is an optional, read-only adapter boundary.
// It runs after durable ingress, never on the provider acknowledgement path.
// Provider text is untrusted history and cannot authorize work or impersonate
// a canonical Agent, even when the provider author is a bot.
type ExternalConversationContextHost interface {
	ReadExternalConversationContext(context.Context, ExternalConversationContextHostRequest) (*ExternalConversationContextResult, error)
}

type ExternalConversationContextHostRequest struct {
	Endpoint *models.ExternalConversationEndpoint       `json:"endpoint"`
	Adapter  *skill.BoundConversationAdapter            `json:"adapter"`
	Event    models.NormalizedExternalConversationEvent `json:"event"`
}

type ExternalConversationContextMessage struct {
	Source                 *models.ExternalMessageSource           `json:"source,omitempty"`
	ExternalConversationID string                                  `json:"externalConversationId"`
	ExternalThreadID       string                                  `json:"externalThreadId,omitempty"`
	ExternalMessageID      string                                  `json:"externalMessageId"`
	ExternalParticipantID  string                                  `json:"externalParticipantId"`
	ParticipantDisplayName string                                  `json:"participantDisplayName,omitempty"`
	Text                   string                                  `json:"text"`
	Attachments            []models.ExternalConversationAttachment `json:"attachments,omitempty"`
	OccurredAt             time.Time                               `json:"occurredAt"`
}

type ExternalConversationContextResult struct {
	Source    *models.ExternalMessageSource        `json:"source,omitempty"`
	Status    string                               `json:"status"`
	ErrorCode string                               `json:"errorCode,omitempty"`
	Messages  []ExternalConversationContextMessage `json:"messages,omitempty"`
}

func externalConversationContextReference(endpointID string, state *runtimeconversation.ExternalConversationContextState) models.ConversationReference {
	return models.ConversationReference{Kind: models.ConversationReferenceExternalSource,
		ID: runtimeconversation.ExternalConversationContextReferencePrefix(endpointID) + state.Status, Version: int64(state.ImportedMessages) + 1}
}

func externalConversationContextOriginReference(endpointID string, state *runtimeconversation.ExternalConversationContextState) *models.ConversationReference {
	if state.ExternalConversationID == "" {
		return nil
	}
	encoded, _ := json.Marshal([]string{state.ExternalConversationID, state.ExternalThreadID})
	id := runtimeconversation.ExternalConversationContextOriginPrefix(endpointID) + base64.RawURLEncoding.EncodeToString(encoded)
	if len(id) > 256 {
		return nil
	}
	return &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: id}
}

func (w *ExternalConversationInboxWorker) hydrateContext(ctx context.Context, endpoint *models.ExternalConversationEndpoint,
	conversation *models.Conversation, mapping *models.ExternalConversationMapping, item *models.ExternalConversationInboxItem,
) (*models.ExternalConversationMapping, *runtimeconversation.ExternalConversationContextState, error) {
	if w.config.ContextHost == nil || w.config.ContextCatalog == nil {
		return mapping, nil, nil
	}
	key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "message", item.Event.ExternalMessageID)
	if message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, endpoint.Scope, conversation.ID, key); err != nil {
		return mapping, nil, err
	} else if message != nil {
		return mapping, runtimeconversation.ExternalConversationContextAvailability(message, endpoint.ID), nil
	}
	state := &runtimeconversation.ExternalConversationContextState{Status: runtimeconversation.ExternalConversationContextUnavailable,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: item.Event.ExternalThreadID}
	adapter, err := w.resolveContextAdapter(ctx, endpoint, item.Adapter)
	if err != nil {
		return mapping, state, nil
	}
	if !models.HasConversationFeature(adapter.Adapter.Features, capability.ConversationFeatureContextHistory) {
		return mapping, nil, nil
	}
	// Automatic hydration never broadens an unthreaded mention into the entire
	// channel timeline. Explicit governed reads cover additional channel history.
	if item.Event.ExternalThreadID == "" {
		return mapping, state, nil
	}
	effective := models.CloneExternalConversationEndpoint(endpoint)
	effective.Address = item.Event.ExternalConversationID
	effective.Adapter = externalConversationResolvedAdapterReference(adapter)
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, readErr := w.config.ContextHost.ReadExternalConversationContext(fetchCtx, ExternalConversationContextHostRequest{
		Endpoint: effective, Adapter: adapter, Event: item.Event,
	})
	cancel()
	if result != nil {
		state.ErrorCode = runtimeconversation.NormalizeExternalConversationContextErrorCode(result.ErrorCode)
	}
	if readErr != nil || result == nil {
		return mapping, state, nil
	}
	// A configuration change during the provider read must not import context
	// from credentials or an endpoint that is no longer authorized.
	current, getErr := w.store.GetExternalConversationEndpoint(ctx, endpoint.Scope, endpoint.ID)
	if getErr != nil {
		return mapping, nil, getErr
	}
	if current == nil || current.Status != models.ExternalConversationEndpointActive || current.Revision != endpoint.Revision {
		return mapping, nil, models.ErrExternalConversationConflict
	}
	fresh, resolveErr := w.resolveContextAdapter(ctx, current, item.Adapter)
	if resolveErr != nil || fresh.Binding.Revision != adapter.Binding.Revision {
		return mapping, state, nil
	}
	if source := result.Source; source != nil && source.ChannelID == item.Event.ExternalConversationID && source.ParticipantID == item.Event.ExternalParticipantID {
		item.Event.Source = models.CloneExternalMessageSource(source)
		if name := models.ExternalSourceLabel(source.ParticipantDisplayName, 160); name != "" {
			item.Event.ParticipantDisplayName = name
		}
	}
	if result.Status == runtimeconversation.ExternalConversationContextUnavailable {
		return mapping, state, nil
	}
	messages, status, valid := normalizeExternalConversationContext(result, item.Event)
	if !valid {
		return mapping, state, nil
	}
	state.Status = status
	for _, historical := range messages {
		message, importErr := w.ensureContextMessage(ctx, endpoint, conversation, mapping, historical)
		if errors.Is(importErr, errExternalConversationContextMappingMismatch) {
			state.Status = runtimeconversation.ExternalConversationContextPartial
			continue
		}
		if importErr != nil {
			return mapping, nil, importErr
		}
		state.ImportedMessages++
		if item.Event.ExternalThreadID != "" && mapping.ThreadRootMessageID == "" {
			mapping, importErr = w.ensureThreadRoot(ctx, mapping, message.ID)
			if importErr != nil {
				return mapping, nil, importErr
			}
		}
	}
	return mapping, state, nil
}

func (w *ExternalConversationInboxWorker) resolveContextAdapter(ctx context.Context, endpoint *models.ExternalConversationEndpoint,
	ref models.ExternalConversationAdapterReference,
) (*skill.BoundConversationAdapter, error) {
	adapter, err := w.config.ContextCatalog.ResolveConversationAdapterBinding(ctx,
		skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, ref.BindingID, ref.AdapterID)
	if err != nil || adapter == nil || adapter.Binding == nil || adapter.Definition == nil ||
		adapter.Binding.Scope != (skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}) ||
		adapter.Binding.DeploymentID != endpoint.DeploymentID || adapter.Binding.ID != ref.BindingID ||
		adapter.Binding.Revision < ref.BindingRevision || adapter.Binding.SkillID != ref.SkillID ||
		adapter.Definition.ID != ref.SkillID || adapter.AdapterID != ref.AdapterID || adapter.Adapter.Provider != endpoint.Provider {
		return nil, models.ErrExternalConversationConflict
	}
	return adapter, nil
}

func normalizeExternalConversationContext(result *ExternalConversationContextResult, event models.NormalizedExternalConversationEvent) ([]ExternalConversationContextMessage, string, bool) {
	if !runtimeconversation.ValidExternalConversationContextStatus(result.Status) || len(result.Messages) > runtimeconversation.MaximumExternalConversationContextMessages {
		return nil, "", false
	}
	status := result.Status
	seen := make(map[string]ExternalConversationContextMessage, len(result.Messages))
	messages := make([]ExternalConversationContextMessage, 0, len(result.Messages))
	bytes, attachmentCount := 0, 0
	for _, message := range result.Messages {
		if message.ExternalConversationID != event.ExternalConversationID ||
			(message.ExternalThreadID != event.ExternalThreadID && !(message.ExternalThreadID == "" && message.ExternalMessageID == event.ExternalThreadID)) ||
			!models.IsValidExternalConversationReference(message.ExternalMessageID, 1024) ||
			!models.IsValidExternalConversationReference(message.ExternalParticipantID, 1024) ||
			message.OccurredAt.IsZero() || len(message.ParticipantDisplayName) > 160 || strings.ContainsAny(message.ParticipantDisplayName, "\r\n") ||
			(strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0) || len(message.Text) > 64*1024 || models.ValidateExternalAttachments(message.Attachments) != nil {
			return nil, "", false
		}
		if message.ExternalMessageID == event.ExternalMessageID {
			continue
		}
		if message.OccurredAt.After(event.OccurredAt) || (message.OccurredAt.Equal(event.OccurredAt) && message.ExternalMessageID != event.ReplyToExternalMessageID) {
			status = runtimeconversation.ExternalConversationContextPartial
			continue
		}
		message.ExternalThreadID = event.ExternalThreadID
		if previous, duplicate := seen[message.ExternalMessageID]; duplicate {
			if !reflect.DeepEqual(previous, message) {
				return nil, "", false
			}
			continue
		}
		attachmentCount += len(message.Attachments)
		if attachmentCount > 8 {
			return nil, "", false
		}
		bytes += len(message.Text)
		if bytes > maximumExternalConversationContextBytes {
			return nil, "", false
		}
		seen[message.ExternalMessageID] = message
		messages = append(messages, message)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].ExternalMessageID == event.ExternalThreadID {
			return true
		}
		if messages[j].ExternalMessageID == event.ExternalThreadID {
			return false
		}
		if !messages[i].OccurredAt.Equal(messages[j].OccurredAt) {
			return messages[i].OccurredAt.Before(messages[j].OccurredAt)
		}
		return messages[i].ExternalMessageID < messages[j].ExternalMessageID
	})
	if event.ExternalThreadID != "" && event.ExternalThreadID != event.ExternalMessageID &&
		(len(messages) == 0 || messages[0].ExternalMessageID != event.ExternalThreadID) {
		status = runtimeconversation.ExternalConversationContextPartial
	}
	return messages, status, true
}

func (w *ExternalConversationInboxWorker) ensureContextMessage(ctx context.Context, endpoint *models.ExternalConversationEndpoint,
	conversation *models.Conversation, thread *models.ExternalConversationMapping, historical ExternalConversationContextMessage,
) (*models.ChannelMessage, error) {
	for _, direction := range []models.ExternalMessageDirection{models.ExternalMessageInbound, models.ExternalMessageOutbound} {
		mapping, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, direction, historical.ExternalMessageID)
		if err != nil {
			return nil, err
		}
		if mapping != nil {
			if mapping.ConversationID != conversation.ID {
				return nil, errExternalConversationContextMappingMismatch
			}
			message, getErr := w.conversations.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, mapping.ChannelMessageID)
			if getErr != nil {
				return nil, getErr
			}
			if !externalConversationContextMessageFitsThread(message, thread) {
				return nil, errExternalConversationContextMappingMismatch
			}
			return message, nil
		}
	}
	// Quoted files belong to the historical message, not the user's new reply.
	attachmentRefs, notices, err := w.importAttachments(ctx, endpoint, conversation, models.NormalizedExternalConversationEvent{
		ID:   stableExternalConversationID(endpoint.Scope, endpoint.ID, "context-attachment", historical.ExternalMessageID),
		Type: capability.ConversationEventMessageReceived, OrderingKey: historical.ExternalMessageID, Text: historical.Text,
		ExternalConversationID: historical.ExternalConversationID, ExternalThreadID: historical.ExternalThreadID,
		ExternalMessageID: historical.ExternalMessageID, ExternalParticipantID: historical.ExternalParticipantID,
		Attachments: historical.Attachments, OccurredAt: historical.OccurredAt,
	})
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(historical.Text)
	if text == "" && len(historical.Attachments) > 0 {
		text = "Shared attached files."
	}
	if len(notices) > 0 {
		text += "\n\nAttachment access: " + strings.Join(notices, "; ")
	}
	key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "message", historical.ExternalMessageID)
	for attempts := 0; attempts < 32; attempts++ {
		if existing, err := w.store.FindChannelMessageByIdempotencyKey(ctx, endpoint.Scope, conversation.ID, key); err != nil {
			return nil, err
		} else if existing != nil {
			if !externalConversationContextMessageFitsThread(existing, thread) {
				return nil, errExternalConversationContextMappingMismatch
			}
			if err := w.ensureInboundMessageMapping(ctx, endpoint, historical.ExternalMessageID, existing); err != nil {
				return nil, err
			}
			return existing, nil
		}
		participant, err := w.ensureParticipant(ctx, endpoint, models.NormalizedExternalConversationEvent{
			ExternalParticipantID: historical.ExternalParticipantID, ParticipantDisplayName: historical.ParticipantDisplayName,
		})
		if err != nil {
			return nil, err
		}
		current, err := w.conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		if current.Status != models.ConversationStatusActive {
			return nil, models.ErrInvalidConversation
		}
		message := &models.ChannelMessage{
			ID: models.StableConversationID(endpoint.Scope, key, "message"), Scope: endpoint.Scope, ConversationID: conversation.ID,
			Sequence: current.LastSequence + 1, Sender: participant.Participant, SenderDisplayName: participant.DisplayName,
			ExternalSource: activity.NewExternalMessageSource(endpoint, models.NormalizedExternalConversationEvent{ExternalConversationID: historical.ExternalConversationID, ExternalThreadID: historical.ExternalThreadID, ExternalMessageID: historical.ExternalMessageID, ExternalParticipantID: historical.ExternalParticipantID, ParticipantDisplayName: historical.ParticipantDisplayName, OccurredAt: historical.OccurredAt, Source: historical.Source}),
			Intent:         models.MessageIntentUpdate, Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			Historical: true, IdempotencyKey: key, CreatedAt: historical.OccurredAt.UTC(),
			References: append([]models.ConversationReference{{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}}, attachmentRefs...),
		}
		if thread.ExternalThreadID != "" && thread.ThreadRootMessageID != "" {
			message.ThreadRootID, message.ReplyToMessageID = thread.ThreadRootMessageID, thread.ThreadRootMessageID
		}
		if err := message.Validate(); err != nil {
			return nil, err
		}
		next := models.CloneConversation(current)
		next.LastSequence, next.Revision, next.UpdatedAt = message.Sequence, current.Revision+1, w.now().UTC()
		result, err := w.store.CommitChannelMessage(ctx, models.ChannelMessageCommitRecord{
			Conversation: next, ExpectedRevision: current.Revision, Message: message,
		})
		if errors.Is(err, models.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := w.ensureInboundMessageMapping(ctx, endpoint, historical.ExternalMessageID, result.Message); err != nil {
			return nil, err
		}
		return result.Message, nil
	}
	return nil, models.ErrRevisionConflict
}

func externalConversationContextMessageFitsThread(message *models.ChannelMessage, thread *models.ExternalConversationMapping) bool {
	if message == nil || thread == nil {
		return false
	}
	if thread.ThreadRootMessageID == "" {
		return message.ThreadRootID == ""
	}
	return message.ID == thread.ThreadRootMessageID || message.ThreadRootID == thread.ThreadRootMessageID
}
