package conversation

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

const (
	MaximumExternalConversationContextMessages = 50
	ExternalConversationContextComplete        = "complete"
	ExternalConversationContextPartial         = "partial"
	ExternalConversationContextUnavailable     = "unavailable"
)

// ExternalConversationContextState describes supplied history, not permission
// to read a different destination or a claim that omitted history is empty.
type ExternalConversationContextState struct {
	Status                 string `json:"status"`
	ImportedMessages       int    `json:"importedMessages"`
	ExternalConversationID string `json:"externalConversationId,omitempty"`
	ExternalThreadID       string `json:"externalThreadId,omitempty"`
	ErrorCode              string `json:"errorCode,omitempty"`
}

func ExternalConversationContextReferencePrefix(endpointID string) string {
	return fmt.Sprintf("external-context:%x:", sha256.Sum256([]byte(endpointID)))
}

func ExternalConversationContextOriginPrefix(endpointID string) string {
	return fmt.Sprintf("external-context-origin:%x:", sha256.Sum256([]byte(endpointID)))
}

func ExternalConversationContextErrorPrefix(endpointID string) string {
	return fmt.Sprintf("external-context-error:%x:", sha256.Sum256([]byte(endpointID)))
}

func NormalizeExternalConversationContextErrorCode(code string) string {
	switch code {
	case "missing_scope", "not_in_channel", "channel_not_found", "thread_not_found", "rate_limited", "unavailable":
		return code
	default:
		return ""
	}
}

func ExternalConversationContextAvailability(message *models.ChannelMessage, endpointID string) *ExternalConversationContextState {
	if message == nil || endpointID == "" {
		return nil
	}
	prefix := ExternalConversationContextReferencePrefix(endpointID)
	var state *ExternalConversationContextState
	for _, reference := range message.References {
		if reference.Kind != models.ConversationReferenceExternalSource || !strings.HasPrefix(reference.ID, prefix) {
			continue
		}
		status := strings.TrimPrefix(reference.ID, prefix)
		if !ValidExternalConversationContextStatus(status) || reference.Version < 1 || reference.Version > MaximumExternalConversationContextMessages+1 {
			continue
		}
		state = &ExternalConversationContextState{Status: status, ImportedMessages: int(reference.Version - 1)}
		break
	}
	if state == nil {
		return nil
	}
	if source := message.ExternalSource; source != nil && models.IsValidExternalConversationReference(source.ChannelID, 1024) &&
		(source.ThreadID == "" || models.IsValidExternalConversationReference(source.ThreadID, 1024)) {
		state.ExternalConversationID, state.ExternalThreadID = source.ChannelID, source.ThreadID
	}
	originPrefix := ExternalConversationContextOriginPrefix(endpointID)
	for _, reference := range message.References {
		// Canonical provider provenance takes precedence; the compact reference
		// is only a fallback for stores that have no structured source metadata.
		if state.ExternalConversationID != "" {
			break
		}
		if reference.Kind != models.ConversationReferenceExternalSource || !strings.HasPrefix(reference.ID, originPrefix) || len(reference.ID) > 256 {
			continue
		}
		encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(reference.ID, originPrefix))
		if err != nil {
			continue
		}
		var ids []string
		if json.Unmarshal(encoded, &ids) != nil || len(ids) != 2 || !models.IsValidExternalConversationReference(ids[0], 1024) || (ids[1] != "" && !models.IsValidExternalConversationReference(ids[1], 1024)) {
			continue
		}
		state.ExternalConversationID, state.ExternalThreadID = ids[0], ids[1]
		break
	}
	errorPrefix := ExternalConversationContextErrorPrefix(endpointID)
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceExternalSource && strings.HasPrefix(reference.ID, errorPrefix) {
			state.ErrorCode = NormalizeExternalConversationContextErrorCode(strings.TrimPrefix(reference.ID, errorPrefix))
			break
		}
	}
	return state
}

func ValidExternalConversationContextStatus(status string) bool {
	return status == ExternalConversationContextComplete || status == ExternalConversationContextPartial || status == ExternalConversationContextUnavailable
}
