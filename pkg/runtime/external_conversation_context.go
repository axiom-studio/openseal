package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	MaximumExternalConversationContextMessages = 50
	maximumExternalConversationContextBytes    = 256 * 1024
	ExternalConversationContextComplete        = "complete"
	ExternalConversationContextPartial         = "partial"
	ExternalConversationContextUnavailable     = "unavailable"
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
	Endpoint *ExternalConversationEndpoint       `json:"endpoint"`
	Adapter  *skill.BoundConversationAdapter     `json:"adapter"`
	Event    NormalizedExternalConversationEvent `json:"event"`
}

type ExternalConversationContextMessage struct {
	Source                 *ExternalMessageSource `json:"source,omitempty"`
	ExternalConversationID string                 `json:"externalConversationId"`
	ExternalThreadID       string                 `json:"externalThreadId,omitempty"`
	ExternalMessageID      string                 `json:"externalMessageId"`
	ExternalParticipantID  string                 `json:"externalParticipantId"`
	ParticipantDisplayName string                 `json:"participantDisplayName,omitempty"`
	Text                   string                 `json:"text"`
	OccurredAt             time.Time              `json:"occurredAt"`
}

type ExternalConversationContextResult struct {
	Source    *ExternalMessageSource               `json:"source,omitempty"`
	Status    string                               `json:"status"`
	ErrorCode string                               `json:"errorCode,omitempty"`
	Messages  []ExternalConversationContextMessage `json:"messages,omitempty"`
}

// ExternalConversationContextState describes supplied history, not permission
// to read a different destination or a claim that omitted history is empty.
type ExternalConversationContextState struct {
	Status                 string `json:"status"`
	ImportedMessages       int    `json:"importedMessages"`
	ExternalConversationID string `json:"externalConversationId,omitempty"`
	ExternalThreadID       string `json:"externalThreadId,omitempty"`
	ErrorCode              string `json:"errorCode,omitempty"`
}

func externalConversationContextReference(endpointID string, state *ExternalConversationContextState) ConversationReference {
	return ConversationReference{Kind: ConversationReferenceExternalSource,
		ID: externalConversationContextReferencePrefix(endpointID) + state.Status, Version: int64(state.ImportedMessages) + 1}
}

func externalConversationContextReferencePrefix(endpointID string) string {
	return fmt.Sprintf("external-context:%x:", sha256.Sum256([]byte(endpointID)))
}

func externalConversationContextOriginPrefix(endpointID string) string {
	return fmt.Sprintf("external-context-origin:%x:", sha256.Sum256([]byte(endpointID)))
}

func externalConversationContextErrorPrefix(endpointID string) string {
	return fmt.Sprintf("external-context-error:%x:", sha256.Sum256([]byte(endpointID)))
}

func normalizeExternalConversationContextErrorCode(code string) string {
	switch code {
	case "missing_scope", "not_in_channel", "channel_not_found", "thread_not_found", "rate_limited", "unavailable":
		return code
	default:
		return ""
	}
}

func externalConversationContextOriginReference(endpointID string, state *ExternalConversationContextState) *ConversationReference {
	if state.ExternalConversationID == "" {
		return nil
	}
	encoded, _ := json.Marshal([]string{state.ExternalConversationID, state.ExternalThreadID})
	id := externalConversationContextOriginPrefix(endpointID) + base64.RawURLEncoding.EncodeToString(encoded)
	if len(id) > 256 {
		return nil
	}
	return &ConversationReference{Kind: ConversationReferenceExternalSource, ID: id}
}

func ExternalConversationContextAvailability(message *ChannelMessage, endpointID string) *ExternalConversationContextState {
	if message == nil || endpointID == "" {
		return nil
	}
	prefix := externalConversationContextReferencePrefix(endpointID)
	var state *ExternalConversationContextState
	for _, reference := range message.References {
		if reference.Kind != ConversationReferenceExternalSource || !strings.HasPrefix(reference.ID, prefix) {
			continue
		}
		status := strings.TrimPrefix(reference.ID, prefix)
		if !validExternalConversationContextStatus(status) || reference.Version < 1 || reference.Version > MaximumExternalConversationContextMessages+1 {
			continue
		}
		state = &ExternalConversationContextState{Status: status, ImportedMessages: int(reference.Version - 1)}
		break
	}
	if state == nil {
		return nil
	}
	if source := message.ExternalSource; source != nil && validExternalConversationReference(source.ChannelID, 1024) &&
		(source.ThreadID == "" || validExternalConversationReference(source.ThreadID, 1024)) {
		state.ExternalConversationID, state.ExternalThreadID = source.ChannelID, source.ThreadID
	}
	originPrefix := externalConversationContextOriginPrefix(endpointID)
	for _, reference := range message.References {
		// Canonical provider provenance takes precedence; the compact reference
		// is only a fallback for stores that have no structured source metadata.
		if state.ExternalConversationID != "" {
			break
		}
		if reference.Kind != ConversationReferenceExternalSource || !strings.HasPrefix(reference.ID, originPrefix) || len(reference.ID) > 256 {
			continue
		}
		encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(reference.ID, originPrefix))
		if err != nil {
			continue
		}
		var ids []string
		if json.Unmarshal(encoded, &ids) != nil || len(ids) != 2 || !validExternalConversationReference(ids[0], 1024) || (ids[1] != "" && !validExternalConversationReference(ids[1], 1024)) {
			continue
		}
		state.ExternalConversationID, state.ExternalThreadID = ids[0], ids[1]
		break
	}
	errorPrefix := externalConversationContextErrorPrefix(endpointID)
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceExternalSource && strings.HasPrefix(reference.ID, errorPrefix) {
			state.ErrorCode = normalizeExternalConversationContextErrorCode(strings.TrimPrefix(reference.ID, errorPrefix))
			break
		}
	}
	return state
}

func validExternalConversationContextStatus(status string) bool {
	return status == ExternalConversationContextComplete || status == ExternalConversationContextPartial || status == ExternalConversationContextUnavailable
}

func (w *ExternalConversationInboxWorker) hydrateContext(ctx context.Context, endpoint *ExternalConversationEndpoint,
	conversation *Conversation, mapping *ExternalConversationMapping, item *ExternalConversationInboxItem,
) (*ExternalConversationMapping, *ExternalConversationContextState, error) {
	if w.config.ContextHost == nil || w.config.ContextCatalog == nil {
		return mapping, nil, nil
	}
	key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "message", item.Event.ExternalMessageID)
	if message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, endpoint.Scope, conversation.ID, key); err != nil {
		return mapping, nil, err
	} else if message != nil {
		return mapping, ExternalConversationContextAvailability(message, endpoint.ID), nil
	}
	state := &ExternalConversationContextState{Status: ExternalConversationContextUnavailable,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: item.Event.ExternalThreadID}
	adapter, err := w.resolveContextAdapter(ctx, endpoint, item.Adapter)
	if err != nil {
		return mapping, state, nil
	}
	if !hasConversationFeature(adapter.Adapter.Features, capability.ConversationFeatureContextHistory) {
		return mapping, nil, nil
	}
	// Automatic hydration never broadens an unthreaded mention into the entire
	// channel timeline. Explicit governed reads cover additional channel history.
	if item.Event.ExternalThreadID == "" {
		return mapping, state, nil
	}
	effective := cloneExternalConversationEndpoint(endpoint)
	effective.Address = item.Event.ExternalConversationID
	effective.Adapter = externalConversationResolvedAdapterReference(adapter)
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, readErr := w.config.ContextHost.ReadExternalConversationContext(fetchCtx, ExternalConversationContextHostRequest{
		Endpoint: effective, Adapter: adapter, Event: item.Event,
	})
	cancel()
	if result != nil {
		state.ErrorCode = normalizeExternalConversationContextErrorCode(result.ErrorCode)
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
	if current == nil || current.Status != ExternalConversationEndpointActive || current.Revision != endpoint.Revision {
		return mapping, nil, ErrExternalConversationConflict
	}
	fresh, resolveErr := w.resolveContextAdapter(ctx, current, item.Adapter)
	if resolveErr != nil || fresh.Binding.Revision != adapter.Binding.Revision {
		return mapping, state, nil
	}
	if source := result.Source; source != nil && source.ChannelID == item.Event.ExternalConversationID && source.ParticipantID == item.Event.ExternalParticipantID {
		item.Event.Source = cloneExternalMessageSource(source)
		if name := sourceLabel(source.ParticipantDisplayName, 160); name != "" {
			item.Event.ParticipantDisplayName = name
		}
	}
	if result.Status == ExternalConversationContextUnavailable {
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
			state.Status = ExternalConversationContextPartial
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

func (w *ExternalConversationInboxWorker) resolveContextAdapter(ctx context.Context, endpoint *ExternalConversationEndpoint,
	ref ExternalConversationAdapterReference,
) (*skill.BoundConversationAdapter, error) {
	adapter, err := w.config.ContextCatalog.ResolveConversationAdapterBinding(ctx,
		skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, ref.BindingID, ref.AdapterID)
	if err != nil || adapter == nil || adapter.Binding == nil || adapter.Definition == nil ||
		adapter.Binding.Scope != (skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}) ||
		adapter.Binding.DeploymentID != endpoint.DeploymentID || adapter.Binding.ID != ref.BindingID ||
		adapter.Binding.Revision < ref.BindingRevision || adapter.Binding.SkillID != ref.SkillID ||
		adapter.Definition.ID != ref.SkillID || adapter.AdapterID != ref.AdapterID || adapter.Adapter.Provider != endpoint.Provider {
		return nil, ErrExternalConversationConflict
	}
	return adapter, nil
}

func normalizeExternalConversationContext(result *ExternalConversationContextResult, event NormalizedExternalConversationEvent) ([]ExternalConversationContextMessage, string, bool) {
	if !validExternalConversationContextStatus(result.Status) || len(result.Messages) > MaximumExternalConversationContextMessages {
		return nil, "", false
	}
	status := result.Status
	seen := make(map[string]ExternalConversationContextMessage, len(result.Messages))
	messages := make([]ExternalConversationContextMessage, 0, len(result.Messages))
	bytes := 0
	for _, message := range result.Messages {
		if message.ExternalConversationID != event.ExternalConversationID ||
			(message.ExternalThreadID != event.ExternalThreadID && !(message.ExternalThreadID == "" && message.ExternalMessageID == event.ExternalThreadID)) ||
			!validExternalConversationReference(message.ExternalMessageID, 1024) ||
			!validExternalConversationReference(message.ExternalParticipantID, 1024) ||
			message.OccurredAt.IsZero() || len(message.ParticipantDisplayName) > 160 || strings.ContainsAny(message.ParticipantDisplayName, "\r\n") ||
			strings.TrimSpace(message.Text) == "" || len(message.Text) > 64*1024 {
			return nil, "", false
		}
		if message.ExternalMessageID == event.ExternalMessageID {
			continue
		}
		if !message.OccurredAt.Before(event.OccurredAt) {
			status = ExternalConversationContextPartial
			continue
		}
		message.ExternalThreadID = event.ExternalThreadID
		if previous, duplicate := seen[message.ExternalMessageID]; duplicate {
			if !reflect.DeepEqual(previous, message) {
				return nil, "", false
			}
			continue
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
		status = ExternalConversationContextPartial
	}
	return messages, status, true
}

func (w *ExternalConversationInboxWorker) ensureContextMessage(ctx context.Context, endpoint *ExternalConversationEndpoint,
	conversation *Conversation, thread *ExternalConversationMapping, historical ExternalConversationContextMessage,
) (*ChannelMessage, error) {
	for _, direction := range []ExternalMessageDirection{ExternalMessageInbound, ExternalMessageOutbound} {
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
		participant, err := w.ensureParticipant(ctx, endpoint, NormalizedExternalConversationEvent{
			ExternalParticipantID: historical.ExternalParticipantID, ParticipantDisplayName: historical.ParticipantDisplayName,
		})
		if err != nil {
			return nil, err
		}
		current, err := w.conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		if current.Status != ConversationStatusActive {
			return nil, ErrInvalidConversation
		}
		message := &ChannelMessage{
			ID: stableConversationID(endpoint.Scope, key, "message"), Scope: endpoint.Scope, ConversationID: conversation.ID,
			Sequence: current.LastSequence + 1, Sender: participant.Participant, SenderDisplayName: participant.DisplayName,
			ExternalSource: externalMessageSource(endpoint, NormalizedExternalConversationEvent{ExternalConversationID: historical.ExternalConversationID, ExternalThreadID: historical.ExternalThreadID, ExternalMessageID: historical.ExternalMessageID, ExternalParticipantID: historical.ExternalParticipantID, ParticipantDisplayName: historical.ParticipantDisplayName, OccurredAt: historical.OccurredAt, Source: historical.Source}),
			Intent:         MessageIntentUpdate, Content: strings.TrimSpace(historical.Text), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Historical: true, IdempotencyKey: key, CreatedAt: historical.OccurredAt.UTC(),
			References: []ConversationReference{{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}},
		}
		if thread.ExternalThreadID != "" && thread.ThreadRootMessageID != "" {
			message.ThreadRootID, message.ReplyToMessageID = thread.ThreadRootMessageID, thread.ThreadRootMessageID
		}
		if err := message.Validate(); err != nil {
			return nil, err
		}
		next := cloneConversation(current)
		next.LastSequence, next.Revision, next.UpdatedAt = message.Sequence, current.Revision+1, w.now().UTC()
		result, err := w.store.CommitChannelMessage(ctx, ChannelMessageCommitRecord{
			Conversation: next, ExpectedRevision: current.Revision, Message: message,
		})
		if errors.Is(err, ErrRevisionConflict) {
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
	return nil, ErrRevisionConflict
}

func externalConversationContextMessageFitsThread(message *ChannelMessage, thread *ExternalConversationMapping) bool {
	if message == nil || thread == nil {
		return false
	}
	if thread.ThreadRootMessageID == "" {
		return message.ThreadRootID == ""
	}
	return message.ID == thread.ThreadRootMessageID || message.ThreadRootID == thread.ThreadRootMessageID
}
