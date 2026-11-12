package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type RecordSurfaceInteractionRequest struct {
	Scope             Scope
	ConversationID    string
	SurfaceArtifactID string
	SurfaceID         string
	SurfaceVersion    int64
	// MessageID is the message whose references carry the surface artifact.
	MessageID      string
	ComponentID    string
	ItemKey        string
	ActionID       string
	Actor          ConversationParticipant
	Repeatable     bool
	Values         json.RawMessage
	Outcome        json.RawMessage
	IdempotencyKey string
}

func decodeSurfaceInteraction(payload string) (*SurfaceInteraction, error) {
	var value SurfaceInteraction
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode surface interaction: %w", err)
	}
	return &value, nil
}

// RecordSurfaceInteraction stores one interaction for an existing
// conversation. It returns the stored interaction and whether it replayed an
// earlier one with the same idempotency key. MessageID must name a message of
// the conversation that references the surface artifact. Hosts validate the
// surface, version, component and value shape before calling it.
func (s *ConversationService) RecordSurfaceInteraction(ctx context.Context, req RecordSurfaceInteractionRequest) (*SurfaceInteraction, bool, error) {
	if s == nil || s.store == nil {
		return nil, false, errors.New("conversation store is not configured")
	}
	if _, err := s.GetConversation(ctx, req.Scope, req.ConversationID); err != nil {
		return nil, false, err
	}
	interaction := &SurfaceInteraction{
		ID: s.newID(), Scope: req.Scope, ConversationID: strings.TrimSpace(req.ConversationID),
		SurfaceArtifactID: strings.TrimSpace(req.SurfaceArtifactID), SurfaceID: strings.TrimSpace(req.SurfaceID),
		SurfaceVersion: req.SurfaceVersion, MessageID: strings.TrimSpace(req.MessageID), ComponentID: strings.TrimSpace(req.ComponentID),
		ItemKey: strings.TrimSpace(req.ItemKey), ActionID: strings.TrimSpace(req.ActionID), ActorType: req.Actor.Type, ActorID: strings.TrimSpace(req.Actor.ID),
		Repeatable: req.Repeatable, Values: bytes.Clone(req.Values), Outcome: bytes.Clone(req.Outcome),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), CreatedAt: s.now().UTC(),
	}
	if err := interaction.Validate(); err != nil {
		return nil, false, err
	}
	message, err := s.GetChannelMessage(ctx, interaction.Scope, interaction.ConversationID, interaction.MessageID)
	if err != nil {
		return nil, false, err
	}
	if !channelMessageReferencesArtifact(message, interaction.SurfaceArtifactID) {
		return nil, false, fmt.Errorf("%w: message %q does not reference surface artifact %q", ErrInvalidSurfaceInteraction, interaction.MessageID, interaction.SurfaceArtifactID)
	}
	return s.store.CreateSurfaceInteraction(ctx, interaction)
}

func channelMessageReferencesArtifact(message *ChannelMessage, artifactID string) bool {
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceArtifact && reference.ID == artifactID {
			return true
		}
	}
	return false
}

// filterVisibleSurfaceInteractions keeps the interactions viewer may read:
// those whose card message the viewer can see.
func (s *ConversationService) filterVisibleSurfaceInteractions(ctx context.Context, scope Scope, conversationID string, interactions []*SurfaceInteraction, viewer ConversationViewer) ([]*SurfaceInteraction, error) {
	if err := viewer.Validate(); err != nil {
		return nil, err
	}
	candidates := make([]*SurfaceInteraction, 0, len(interactions))
	messageIDs := make([]string, 0)
	requested := make(map[string]bool)
	for _, interaction := range interactions {
		candidates = append(candidates, interaction)
		if !requested[interaction.MessageID] {
			requested[interaction.MessageID] = true
			messageIDs = append(messageIDs, interaction.MessageID)
		}
	}
	if len(candidates) == 0 {
		return candidates, nil
	}
	messages := make([]*ChannelMessage, 0, len(messageIDs))
	if batch, ok := s.store.(ChannelMessageBatchStore); ok {
		loaded, err := loadChannelMessageBatches(ctx, batch, scope, conversationID, messageIDs)
		if err != nil {
			return nil, err
		}
		for _, id := range messageIDs {
			if message := loaded[id]; message != nil {
				messages = append(messages, message)
			}
		}
	} else {
		for _, id := range messageIDs {
			message, err := s.store.GetChannelMessage(ctx, scope, conversationID, id)
			if err != nil {
				return nil, err
			}
			if message != nil {
				messages = append(messages, message)
			}
		}
	}
	visibleMessages, err := s.filterVisibleChannelMessages(ctx, scope, conversationID, messages, viewer)
	if err != nil {
		return nil, err
	}
	visibleMessageIDs := make(map[string]bool, len(visibleMessages))
	for _, message := range visibleMessages {
		visibleMessageIDs[message.ID] = true
	}
	visible := make([]*SurfaceInteraction, 0, len(candidates))
	for _, interaction := range candidates {
		if visibleMessageIDs[interaction.MessageID] {
			visible = append(visible, interaction)
		}
	}
	return visible, nil
}

// ListSurfaceInteractions pages interactions by Sequence. With a Viewer it
// scans past hidden interactions so a page is not cut short by them.
func (s *ConversationService) ListSurfaceInteractions(ctx context.Context, filter SurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if filter.Viewer == nil {
		return s.store.ListSurfaceInteractions(ctx, filter)
	}
	viewer := *filter.Viewer
	scan := filter
	scan.Viewer = nil
	desired := scan.EffectiveLimit()
	result := make([]*SurfaceInteraction, 0)
	for len(result) < desired {
		raw, err := s.store.ListSurfaceInteractions(ctx, scan)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		visible, err := s.filterVisibleSurfaceInteractions(ctx, filter.Scope, filter.ConversationID, raw, viewer)
		if err != nil {
			return nil, err
		}
		result = append(result, visible...)
		scan.AfterSequence = raw[len(raw)-1].Sequence
		if len(raw) < scan.EffectiveLimit() {
			break
		}
	}
	if len(result) > desired {
		result = result[:desired]
	}
	return result, nil
}

// ListLatestSurfaceInteractions resolves last-write-wins state of a surface.
// With a Viewer it reduces only the interactions that viewer may read.
func (s *ConversationService) ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if filter.Viewer == nil {
		return s.store.ListLatestSurfaceInteractions(ctx, filter)
	}
	viewer := *filter.Viewer
	scan := filter
	scan.Viewer = nil
	if err := scan.Validate(); err != nil {
		return nil, err
	}
	page := SurfaceInteractionFilter{Scope: filter.Scope, ConversationID: filter.ConversationID, SurfaceID: filter.SurfaceID, Limit: maximumSurfaceInteractionLimit}
	visible := make([]*SurfaceInteraction, 0)
	for {
		raw, err := s.store.ListSurfaceInteractions(ctx, page)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		filtered, err := s.filterVisibleSurfaceInteractions(ctx, filter.Scope, filter.ConversationID, raw, viewer)
		if err != nil {
			return nil, err
		}
		visible = append(visible, filtered...)
		page.AfterSequence = raw[len(raw)-1].Sequence
		if len(raw) < page.Limit {
			break
		}
	}
	return latestSurfaceInteractions(visible, filter.AcrossActors), nil
}
