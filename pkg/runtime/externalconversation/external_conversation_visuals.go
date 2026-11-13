package externalconversation

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
)

// DeliveryVisual is an answer's visual as an adapter that declares
// capability.ConversationFeatureVisuals receives it: the surface to draw,
// without quiz answer keys, and its plain-markdown reading for notifications
// and for components the provider cannot draw.
type DeliveryVisual struct {
	ArtifactID   string               `json:"artifactId"`
	Version      int64                `json:"version"`
	Surface      presentation.Surface `json:"surface"`
	FallbackText string               `json:"fallbackText"`
	// Answers count what people in the conversation answered on the
	// components a channel can answer (externalCardComponents), so a card
	// refreshed after an answer shows them.
	Answers []DeliveryVisualAnswers `json:"answers,omitempty"`
}

// DeliveryVisualAnswers counts each person's latest answer to one component:
// People who answered, and how many chose each chip, action or option.
// Forms only count people. A poll counts votes on any version, as its tally
// does; other components count answers to this version.
type DeliveryVisualAnswers struct {
	ComponentID string                 `json:"componentId"`
	People      int                    `json:"people"`
	Chosen      []DeliveryVisualChoice `json:"chosen,omitempty"`
}

// DeliveryVisualChoice is how many people chose one id. It is a list entry,
// not a map key, because ids are the agent's and must not become envelope
// keys.
type DeliveryVisualChoice struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// Delivery summaries that record a visual a delivered send left out, so the
// omission is on record without reaching the channel.
const (
	visualsLeftOutSummary = "Visual left out: this connection does not show visuals."
	// A delivery envelope must not carry credential-like keys, and a
	// visual's table columns and comparison values are keyed by the agent.
	visualsUnsafeSummary = "Visual left out: its fields look like credentials."
)

// externalDeliveryVisuals reads visuals (externalDeliveryFiles) for an
// adapter that shows them. It returns how many it left out because their
// content cannot travel in a delivery envelope.
func externalDeliveryVisuals(ctx context.Context, content models.ArtifactContentStore, interactions models.ConversationStore, message *models.ChannelMessage, visuals []*models.Artifact) ([]DeliveryVisual, int, error) {
	if len(visuals) == 0 {
		return nil, 0, nil
	}
	if content == nil {
		return nil, 0, errors.New("visual delivery is unavailable")
	}
	result := make([]DeliveryVisual, 0, len(visuals))
	leftOut := 0
	for _, artifact := range visuals {
		raw, err := readDeliveryArtifact(ctx, content, message.Scope, artifact, presentation.MaxSurfaceBytes)
		if err != nil {
			return nil, 0, err
		}
		surface, err := presentation.Decode(raw)
		if err != nil {
			return nil, 0, err
		}
		shown := presentation.StripAnswerKeys(*surface)
		if !credentialFreeSurface(shown) {
			leftOut++
			continue
		}
		latest, err := interactions.ListLatestSurfaceInteractions(ctx, models.LatestSurfaceInteractionFilter{
			Scope: message.Scope, ConversationID: message.ConversationID, SurfaceID: artifact.ID,
		})
		if err != nil {
			return nil, 0, err
		}
		result = append(result, DeliveryVisual{
			ArtifactID: artifact.ID, Version: artifact.Version, Surface: shown,
			FallbackText: presentation.FallbackText(shown),
			Answers:      visualAnswers(shown, artifact.Version, latest),
		})
	}
	return result, leftOut, nil
}

func credentialFreeSurface(surface presentation.Surface) bool {
	encoded, err := json.Marshal(surface)
	if err != nil {
		return false
	}
	var document map[string]interface{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return false
	}
	return models.ValidateCredentialFreeContext(document) == nil
}

// visualAnswers counts the latest answers (one per person and component)
// on the components of surface a channel can answer.
func visualAnswers(surface presentation.Surface, version int64, latest []*models.SurfaceInteraction) []DeliveryVisualAnswers {
	var result []DeliveryVisualAnswers
	for _, component := range surface.Components {
		result = append(result, componentAnswers(component, version, latest)...)
	}
	return result
}

func componentAnswers(component presentation.Component, version int64, latest []*models.SurfaceInteraction) []DeliveryVisualAnswers {
	var nested []DeliveryVisualAnswers
	for _, child := range component.Children {
		nested = append(nested, componentAnswers(child, version, latest)...)
	}
	if !externalCardComponents[component.Type] {
		return nested
	}
	answers := DeliveryVisualAnswers{ComponentID: component.ID}
	counts := map[string]int{}
	var order []string
	for _, interaction := range latest {
		if interaction == nil || interaction.ComponentID != component.ID ||
			(component.Type != "poll" && interaction.SurfaceVersion != version) {
			continue
		}
		answers.People++
		for _, id := range chosenIDs(component.Type, interaction) {
			if counts[id] == 0 {
				order = append(order, id)
			}
			counts[id]++
		}
	}
	if answers.People == 0 {
		return nested
	}
	for _, id := range order {
		answers.Chosen = append(answers.Chosen, DeliveryVisualChoice{ID: id, Count: counts[id]})
	}
	return append(nested, answers)
}

// chosenIDs is what one answer picked: the chip of a choice, the button of
// actions, the options of a quiz answer or poll vote. Forms pick nothing.
func chosenIDs(componentType string, interaction *models.SurfaceInteraction) []string {
	var values struct {
		ChipID    string   `json:"chipId"`
		Selected  []string `json:"selected"`
		OptionIDs []string `json:"optionIds"`
	}
	_ = json.Unmarshal(interaction.Values, &values)
	switch componentType {
	case "choices":
		if values.ChipID != "" {
			return []string{values.ChipID}
		}
	case "actions":
		return []string{interaction.ActionID}
	case "quiz":
		return values.Selected
	case "poll":
		return values.OptionIDs
	}
	return nil
}
