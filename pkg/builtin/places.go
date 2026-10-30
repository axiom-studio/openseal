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
			"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumPlaceQueryRunes, "description": "Such as Lalbagh Botanical Garden or 95131, San Jose, CA, USA; add the region and country when known. To search around a place, find it first and pass its id as near rather than writing near it here."},
			"near": map[string]interface{}{"type": "string", "pattern": presentation.PlaceIDPattern,
				"description": "A place id found in this chat (a city, postcode, address or venue); results within about 50 km of it, closest first."},
			"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumPlaceResults, "description": "At most this many places; 3 when omitted."},
		}),
		OutputSchema: object([]interface{}{"places"}, map[string]interface{}{
			"places": map[string]interface{}{"type": "array", "maxItems": MaximumPlaceResults, "items": place},
		}),
		SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}

// route limits and travel modes.
const (
	MaximumRouteStops = 10
	MaximumRouteSteps = 20
)

// RouteModes are how route travels; drive when omitted.
var RouteModes = []string{"drive", "walk", "bicycle"}

// RouteUnits are route's distance units; km when omitted.
var RouteUnits = []string{"km", "mi"}

// RouteAction gives directions between places find_place found in the chat,
// optionally in the shortest visiting order.
func RouteAction() skill.Action {
	placeID := map[string]interface{}{"type": "string", "pattern": presentation.PlaceIDPattern}
	text := map[string]interface{}{"type": "string"}
	enum := func(values []string) map[string]interface{} {
		items := make([]interface{}, len(values))
		for i, value := range values {
			items[i] = value
		}
		return map[string]interface{}{"type": "string", "enum": items}
	}
	distance := map[string]interface{}{"type": "number", "minimum": 0}
	minutes := map[string]interface{}{"type": "integer", "minimum": 0}
	stop := object([]interface{}{"place", "name"}, map[string]interface{}{"place": placeID, "name": text})
	leg := object([]interface{}{"from", "to", "distance", "minutes", "steps"}, map[string]interface{}{
		"from": placeID, "to": placeID, "distance": distance, "minutes": minutes,
		"steps": map[string]interface{}{"type": "array", "maxItems": MaximumRouteSteps, "items": text},
	})
	return skill.Action{
		Name:        Route,
		Description: "Directions between places found in this chat: each leg's distance, time and turn-by-turn steps, and optionally the shortest order to visit them.",
		InputSchema: object([]interface{}{"stops"}, map[string]interface{}{
			"stops": map[string]interface{}{"type": "array", "minItems": 2, "maxItems": MaximumRouteStops, "uniqueItems": true, "items": placeID,
				"description": "Place ids found in this chat, in visiting order; the first is the start."},
			"end": map[string]interface{}{"type": "string", "pattern": presentation.PlaceIDPattern,
				"description": "Where to finish after every stop, such as the first stop for a round trip."},
			"optimize": map[string]interface{}{"type": "boolean", "description": "Visit the stops after the first in the shortest order."},
			"mode":     enum(RouteModes),
			"units":    enum(RouteUnits),
		}),
		OutputSchema: object([]interface{}{"mode", "units", "stops", "legs", "distance", "minutes"}, map[string]interface{}{
			"mode": enum(RouteModes), "units": enum(RouteUnits),
			"stops":    map[string]interface{}{"type": "array", "minItems": 2, "maxItems": MaximumRouteStops + 1, "items": stop},
			"legs":     map[string]interface{}{"type": "array", "minItems": 1, "maxItems": MaximumRouteStops, "items": leg},
			"distance": distance, "minutes": minutes,
		}),
		SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}
