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
func externalDeliveryVisuals(ctx context.Context, content models.ArtifactContentStore, scope models.Scope, visuals []*models.Artifact) ([]DeliveryVisual, int, error) {
	if len(visuals) == 0 {
		return nil, 0, nil
	}
	if content == nil {
		return nil, 0, errors.New("visual delivery is unavailable")
	}
	result := make([]DeliveryVisual, 0, len(visuals))
	leftOut := 0
	for _, artifact := range visuals {
		raw, err := readDeliveryArtifact(ctx, content, scope, artifact, presentation.MaxSurfaceBytes)
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
		result = append(result, DeliveryVisual{
			ArtifactID: artifact.ID, Version: artifact.Version, Surface: shown,
			FallbackText: presentation.FallbackText(shown),
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
