package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Earlier canonical answer projections did not persist the source's spoken
// delivery mode. An upgrade must keep those committed receipts unchanged.
// Only answer projection calls this after a conflict; the general message
// admission boundary continues to compare every request field strictly.
func (s *Service) ReplayLegacySpokenResponse(ctx context.Context, request PostChannelMessageRequest) (*models.ChannelMessage, error) {
	if request.Intent != models.MessageIntentAnswer || request.ResponseMode != "spoken" {
		return nil, nil
	}
	existing, err := s.store.FindChannelMessageByIdempotencyKey(ctx, request.Scope, request.ConversationID, request.IdempotencyKey)
	if err != nil || existing == nil || existing.ResponseMode != "" {
		return nil, err
	}
	legacy := request
	legacy.ResponseMode = ""
	if !sameChannelMessageRequest(existing, legacy) {
		return nil, nil
	}
	return existing, nil
}
