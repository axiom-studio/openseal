package runtime

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

const (
	// maximumCardStateCards bounds the newest card references inspected for
	// a turn's card state.
	maximumCardStateCards = 8
	// maximumCardStateLines bounds the card state note.
	maximumCardStateLines = 6
)

// conversationCardState summarises interactions that do not wake the Agent
// (checklist ticks, checked steps) on cards in messages, for the Agent's next
// turn: one line per card component whose shared state changed since the
// Agent's latest message in messages, e.g. "Packing list: 9/14 ticked
// (unticked: passport, charger)". State is the latest write per item across
// everyone the viewer can see. Cards are read newest first and the note is
// at most maximumCardStateLines lines. It is best effort: a card that cannot
// be read is left out rather than failing the turn.
func (r *ConversationRunTurnRunner) conversationCardState(ctx context.Context, conversation *Conversation, viewer ConversationViewer, participantID string, messages []*ChannelMessage) []string {
	if r == nil || r.conversations == nil || r.artifacts == nil || r.config.AttachmentContent == nil || conversation == nil {
		return nil
	}
	ordered := make([]*ChannelMessage, 0, len(messages))
	for _, message := range messages {
		if message != nil {
			ordered = append(ordered, message)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence > ordered[j].Sequence })
	var since time.Time
	for _, message := range ordered {
		if message.Sender.Type == ConversationParticipantAgent && message.Sender.ID == participantID {
			since = message.CreatedAt
			break
		}
	}
	var lines []string
	seen := map[string]bool{}
	for _, message := range ordered {
		for _, reference := range message.References {
			if reference.Kind != ConversationReferenceArtifact || reference.Version < 1 || seen[reference.ID] {
				continue
			}
			if len(seen) == maximumCardStateCards || len(lines) == maximumCardStateLines {
				return lines
			}
			seen[reference.ID] = true
			for _, line := range r.cardStateLines(ctx, conversation, viewer, reference, since) {
				if len(lines) == maximumCardStateLines {
					return lines
				}
				lines = append(lines, line)
			}
		}
	}
	return lines
}

// cardStateLines returns the state lines of one card whose non-waking
// interactions changed at or after since.
func (r *ConversationRunTurnRunner) cardStateLines(ctx context.Context, conversation *Conversation, viewer ConversationViewer, reference ConversationReference, since time.Time) []string {
	latest, err := r.conversations.ListLatestSurfaceInteractions(ctx, LatestSurfaceInteractionFilter{
		Scope: conversation.Scope, ConversationID: conversation.ID, SurfaceID: reference.ID, AcrossActors: true, Viewer: &viewer,
	})
	if err != nil {
		return nil
	}
	changed := map[string]bool{}
	for _, interaction := range latest {
		if interaction.Repeatable && interaction.ItemKey != "" && !interaction.CreatedAt.Before(since) {
			changed[interaction.ComponentID] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	surface := r.loadCardSurface(ctx, conversation.Scope, reference)
	if surface == nil {
		return nil
	}
	var lines []string
	eachCardComponent(surface.Components, func(component presentation.Component) {
		if !changed[component.ID] {
			return
		}
		rule, err := presentation.Interaction(component, "")
		if err != nil || !rule.ItemKeyed || rule.NotifyAgent {
			return
		}
		checked := map[string]bool{}
		for _, interaction := range latest {
			if interaction.ComponentID != component.ID || interaction.ItemKey == "" {
				continue
			}
			var values struct {
				Checked bool `json:"checked"`
			}
			if json.Unmarshal(interaction.Values, &values) == nil {
				checked[interaction.ItemKey] = values.Checked
			}
		}
		if line, ok := presentation.CardStateLine(component, checked); ok {
			lines = append(lines, line)
		}
	})
	return lines
}

func (r *ConversationRunTurnRunner) loadCardSurface(ctx context.Context, scope Scope, reference ConversationReference) *presentation.Surface {
	artifact, err := r.artifacts.GetArtifact(ctx, scope, reference.ID, reference.Version)
	if err != nil || artifact == nil || artifact.Type != presentation.ArtifactType {
		return nil
	}
	reader, err := r.config.AttachmentContent.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		return nil
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, presentation.MaxSurfaceBytes+1))
	if err != nil {
		return nil
	}
	surface, err := presentation.Decode(raw)
	if err != nil {
		return nil
	}
	return surface
}

// eachCardComponent visits components depth first, including tab children.
func eachCardComponent(components []presentation.Component, visit func(presentation.Component)) {
	for _, component := range components {
		visit(component)
		eachCardComponent(component.Children, visit)
		for _, tab := range component.Tabs {
			eachCardComponent(tab.Children, visit)
		}
	}
}
