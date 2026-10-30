package builtin

import (
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// find_place limits.
const (
	MaximumPlaceQueryRunes = 200
	MaximumPlaceResults    = 5
	DefaultPlaceResults    = 3
)

// FindPlaceAction looks real places up for maps and place cards. The host
// records every result per conversation so visuals can refer to it by its
// place id and the host stamps the coordinates in.
func FindPlaceAction() skill.Action {
	placeID := map[string]interface{}{"type": "string", "pattern": presentation.PlaceIDPattern}
	text := map[string]interface{}{"type": "string"}
	coordinate := func(limit float64) map[string]interface{} {
		return map[string]interface{}{"type": "number", "minimum": -limit, "maximum": limit}
	}
	place := object([]interface{}{"place", "name", "lat", "lon"}, map[string]interface{}{
		"place": placeID, "name": text, "category": text, "address": text, "lat": coordinate(90), "lon": coordinate(180),
	})
	return skill.Action{
		Name:        FindPlace,
		Description: "Look up real places (venues, landmarks, streets, towns) to show on a map or a place card. Refer to a result by its place id; never type coordinates.",
		InputSchema: object([]interface{}{"query"}, map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumPlaceQueryRunes, "description": "Such as Lalbagh Botanical Garden or cafes in Indiranagar."},
			"near": map[string]interface{}{"type": "string", "pattern": presentation.PlaceIDPattern,
				"description": "A place id from an earlier find_place result in this chat; results near it come first."},
			"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumPlaceResults, "description": "At most this many places; 3 when omitted."},
		}),
		OutputSchema: object([]interface{}{"places"}, map[string]interface{}{
			"places": map[string]interface{}{"type": "array", "maxItems": MaximumPlaceResults, "items": place},
		}),
		SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}
