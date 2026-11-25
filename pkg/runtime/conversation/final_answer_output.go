package conversation

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

// CloneVisual deep-copies a visual so forms, responses and outcomes never
// share component slices.
func CloneVisual(visual *presentation.Visual) *presentation.Visual {
	if visual == nil {
		return nil
	}
	encoded, err := json.Marshal(visual)
	if err != nil {
		copy := *visual
		return &copy
	}
	var cloned presentation.Visual
	if json.Unmarshal(encoded, &cloned) != nil {
		copy := *visual
		return &copy
	}
	return &cloned
}
