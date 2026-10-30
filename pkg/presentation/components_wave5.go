package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Map pins real places from find_place. Read-only.
type Map struct {
	Pins []MapPin `json:"pins"`
	// Connect draws a dashed line through the pins in order: the order of
	// visits, never a route.
	Connect bool `json:"connect,omitempty"`
}

// MapPin is one pinned place. The host fills in Label (when left out) and
// the output-only Address, Lat and Lon with StampPlaces.
type MapPin struct {
	ID     string `json:"id"`
	Place  string `json:"place"`
	Label  string `json:"label,omitempty"`
	Detail string `json:"detail,omitempty"`

	Address string   `json:"address,omitempty"`
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
}

// Map limits.
const (
	MaxMapPins          = 20
	MaxMapPinLabelRunes = 60
	MaxMapPinDetail     = 300
	// maxStampedAddressRunes bounds an address the host fills in, as for a
	// place card.
	maxStampedAddressRunes = 160
	maxPlaceNameRunes      = 80
	maxPlaceCategoryRunes  = 30
	maxItineraryPlaceRunes = 80
)

func validateMap(v *validator, c Component, _ string) error {
	pins := c.Map
	if pins == nil {
		return payloadRequired("map")
	}
	if err := bounded("map.pins", len(pins.Pins), 1, MaxMapPins); err != nil {
		return err
	}
	ids := map[string]bool{}
	places := map[string]int{}
	for index, pin := range pins.Pins {
		at := fmt.Sprintf("map.pins[%d]", index)
		if err := firstError(itemIDs(ids, at, pin.ID), optional(at+".label", pin.Label, MaxMapPinLabelRunes), optional(at+".detail", pin.Detail, MaxMapPinDetail)); err != nil {
			return err
		}
		if pin.Place == "" {
			return fmt.Errorf("%s.place is required: a place id returned by find_place", at)
		}
		if earlier, pinned := places[pin.Place]; pinned {
			return fmt.Errorf("%s.place %s is already pinned by map.pins[%d]; pin each place once", at, pin.Place, earlier)
		}
		places[pin.Place] = index
		if err := firstError(v.placeRef(at+".place", pin.Place), v.stampedText(at+".address", pin.Address, maxStampedAddressRunes),
			v.stampedPoint(at, pin.Place, pin.Lat, pin.Lon)); err != nil {
			return err
		}
	}
	return nil
}

// location checks an optional location (a place id from find_place) and
// its output-only coordinates at field.
func (v *validator) location(field, placeID string, lat, lon *float64) error {
	if placeID != "" {
		if err := v.placeRef(field+".location", placeID); err != nil {
			return err
		}
	}
	return v.stampedPoint(field, placeID, lat, lon)
}

// PlaceIDs lists every place id a surface refers to (map pins, place and
// itinerary item locations), each once, in the order they appear.
func PlaceIDs(surface Surface) []string {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	walk(surface.Components, func(c *Component, _ string, _ int) bool {
		if c.Map != nil {
			for _, pin := range c.Map.Pins {
				add(pin.Place)
			}
		}
		if c.Place != nil {
			add(c.Place.Location)
		}
		if c.Itinerary != nil {
			for _, day := range c.Itinerary.Days {
				for _, item := range day.Items {
					add(item.Location)
				}
			}
		}
		return true
	})
	return ids
}

// ResolvePlaces looks up every place id of surface with the host's
// PlaceProvenance. It fails on an id find_place never returned.
func ResolvePlaces(ctx context.Context, surface Surface, lookup PlaceProvenance) (map[string]PlaceFix, error) {
	ids := PlaceIDs(surface)
	fixes := make(map[string]PlaceFix, len(ids))
	if len(ids) == 0 {
		return fixes, nil
	}
	if lookup == nil {
		return nil, fmt.Errorf("places cannot be looked up here")
	}
	for _, id := range ids {
		fix, found, err := lookup(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("look up place %s: %w", id, err)
		}
		if !found {
			return nil, fmt.Errorf("place %s was not found by find_place in this chat", id)
		}
		fixes[id] = fix
	}
	return fixes, nil
}

// StampPlaces returns a copy of surface with every place filled in from
// fixes (place id → what find_place returned): lat and lon on map pins,
// place cards and itinerary items with a location; a map pin's label and
// address, a place card's name, address and category, and an itinerary
// item's place when they are empty. Hosts call it after Validate with
// their PlaceProvenance accepted the surface and store the result; the
// input is not modified. It fails when a place id has no fix.
func StampPlaces(surface Surface, fixes map[string]PlaceFix) (Surface, error) {
	encoded, err := json.Marshal(surface)
	if err != nil {
		return Surface{}, err
	}
	var stamped Surface
	if err := json.Unmarshal(encoded, &stamped); err != nil {
		return Surface{}, err
	}
	var failure error
	fixFor := func(path, id string) (PlaceFix, bool) {
		if id == "" || failure != nil {
			return PlaceFix{}, false
		}
		fix, ok := fixes[id]
		if !ok {
			failure = fmt.Errorf("%s: no fix for place %s", path, id)
			return PlaceFix{}, false
		}
		if err := validPlaceFix(fix); err != nil {
			failure = fmt.Errorf("%s: place %s: %w", path, id, err)
			return PlaceFix{}, false
		}
		return fix, true
	}
	walk(stamped.Components, func(c *Component, path string, _ int) bool {
		if c.Map != nil {
			for index := range c.Map.Pins {
				pin := &c.Map.Pins[index]
				fix, ok := fixFor(fmt.Sprintf("%s.map.pins[%d]", path, index), pin.Place)
				if !ok {
					continue
				}
				pin.Lat, pin.Lon = point(fix)
				fill(&pin.Label, fix.Name, MaxMapPinLabelRunes)
				fill(&pin.Address, fix.Address, maxStampedAddressRunes)
			}
		}
		if place := c.Place; place != nil {
			if fix, ok := fixFor(path+".place", place.Location); ok {
				place.Lat, place.Lon = point(fix)
				fill(&place.Name, fix.Name, maxPlaceNameRunes)
				fill(&place.Address, fix.Address, maxStampedAddressRunes)
				fill(&place.Category, fix.Category, maxPlaceCategoryRunes)
			}
		}
		if c.Itinerary != nil {
			for day := range c.Itinerary.Days {
				for index := range c.Itinerary.Days[day].Items {
					item := &c.Itinerary.Days[day].Items[index]
					fix, ok := fixFor(fmt.Sprintf("%s.itinerary.days[%d].items[%d]", path, day, index), item.Location)
					if !ok {
						continue
					}
					item.Lat, item.Lon = point(fix)
					fill(&item.Place, fix.Name, maxItineraryPlaceRunes)
				}
			}
		}
		return failure == nil
	})
	if failure != nil {
		return Surface{}, failure
	}
	return stamped, nil
}

func point(fix PlaceFix) (*float64, *float64) {
	lat, lon := fix.Lat, fix.Lon
	return &lat, &lon
}

// fill sets an empty field to value, cut to maximum characters with an
// ellipsis.
func fill(field *string, value string, maximum int) {
	if strings.TrimSpace(*field) != "" {
		return
	}
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > maximum {
		value = strings.TrimSpace(string(runes[:maximum-1])) + "…"
	}
	*field = value
}
