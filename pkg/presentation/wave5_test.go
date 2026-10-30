package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// knownPlaces is what find_place returned in the test chat, including the
// places of the worked examples.
var knownPlaces = map[string]PlaceFix{
	"place:5e1c7a09b3d2": {Name: "Brighton Palace Pier", Lat: 50.8164, Lon: -0.1365, Address: "Madeira Drive, Brighton, England, United Kingdom", Category: "pier"},
	"place:a84f20c6e917": {Name: "The Lanes", Lat: 50.8222, Lon: -0.1406, Address: "Brighton, England, United Kingdom", Category: "neighbourhood"},
	"place:0d3b9e5f7c21": {Name: "Rottingdean", Lat: 50.8094, Lon: -0.0583, Address: "Brighton and Hove, England, United Kingdom", Category: "village"},
	"place:c72e4a1b8f06": {Name: "Brighton", Lat: 50.8291, Lon: -0.1413, Address: "Queen's Road, Brighton, England, United Kingdom", Category: "station"},
	"place:000000000001": {Name: "Lalbagh Botanical Garden", Lat: 12.9507, Lon: 77.5848, Address: "Mavalli, Bengaluru, Karnataka, India", Category: "garden"},
	"place:000000000002": {Name: strings.Repeat("Very long name ", 10), Lat: 12.97, Lon: 77.59},
}

func examplePlaces(_ context.Context, id string) (PlaceFix, bool, error) {
	fix, ok := knownPlaces[id]
	return fix, ok, nil
}

func places() ValidateOptions { return ValidateOptions{PlaceProvenance: examplePlaces} }

func mapOf(pins ...MapPin) Component {
	return Component{ID: "where", Type: "map", Summary: "Where the places are.", Map: &Map{Pins: pins}}
}

func validateOne(component Component, opts ValidateOptions) error {
	_, err := Validate(context.Background(), Surface{Schema: Schema, Title: "t", State: StateReady, Components: []Component{component}}, opts)
	return err
}

func TestValidateWave5(t *testing.T) {
	lat, lon := 12.9, 77.5
	pins := func(count int) []MapPin {
		list := make([]MapPin, count)
		for index := range list {
			list[index] = MapPin{ID: fmt.Sprintf("p%d", index), Place: fmt.Sprintf("place:%012x", index+1)}
		}
		return list
	}
	cases := []struct {
		name      string
		component Component
		opts      ValidateOptions
		err       string
	}{
		{"map", mapOf(MapPin{ID: "garden", Place: "place:000000000001", Label: "Lalbagh", Detail: "Glass house and lake."}, MapPin{ID: "pier", Place: "place:5e1c7a09b3d2"}), places(), ""},
		{"map connected", Component{ID: "m", Type: "map", Summary: "s", Map: &Map{Pins: pins(2), Connect: true}}, places(), ""},
		{"unknown place", mapOf(MapPin{ID: "a", Place: "place:000000000001"}, MapPin{ID: "b", Place: "place:000000000002"}, MapPin{ID: "c", Place: "place:4b2f9c0e1a77"}), places(),
			"map.pins[2].place place:4b2f9c0e1a77 was not found by find_place in this chat"},
		{"not a place id", mapOf(MapPin{ID: "a", Place: "Lalbagh"}), places(), `map.pins[0].place "Lalbagh" must be a place id returned by find_place`},
		{"missing place", mapOf(MapPin{ID: "a"}), places(), "map.pins[0].place is required"},
		{"place pinned twice", mapOf(MapPin{ID: "a", Place: "place:000000000001"}, MapPin{ID: "b", Place: "place:000000000001"}), places(), "already pinned by map.pins[0]"},
		{"too many pins", Component{ID: "m", Type: "map", Summary: "s", Map: &Map{Pins: pins(21)}}, ValidateOptions{SkipHostChecks: true}, "map.pins needs 1-20"},
		{"no pins", Component{ID: "m", Type: "map", Summary: "s", Map: &Map{}}, places(), "map.pins needs 1-20"},
		{"long label", mapOf(MapPin{ID: "a", Place: "place:000000000001", Label: strings.Repeat("x", 61)}), places(), "map.pins[0].label"},
		{"long detail", mapOf(MapPin{ID: "a", Place: "place:000000000001", Detail: strings.Repeat("x", 301)}), places(), "map.pins[0].detail"},
		{"no summary", Component{ID: "m", Type: "map", Map: &Map{Pins: pins(1)}}, places(), "summary is required"},
		{"typed coordinates", mapOf(MapPin{ID: "a", Place: "place:000000000001", Lat: &lat, Lon: &lon}), places(), "map.pins[0].lat and .lon are filled in from find_place; leave them out"},
		{"typed address", mapOf(MapPin{ID: "a", Place: "place:000000000001", Address: "Bengaluru"}), places(), "map.pins[0].address is filled in from find_place"},
		{"no place checks", mapOf(MapPin{ID: "a", Place: "place:000000000001"}), ValidateOptions{}, "map.pins[0].place place:000000000001 cannot be checked here"},
		{"lookup fails", mapOf(MapPin{ID: "a", Place: "place:000000000001"}), ValidateOptions{PlaceProvenance: func(context.Context, string) (PlaceFix, bool, error) {
			return PlaceFix{}, false, errors.New("store down")
		}}, "check map.pins[0].place place:000000000001: store down"},
		{"skip host checks", mapOf(MapPin{ID: "a", Place: "place:999999999999", Address: "Somewhere", Lat: &lat, Lon: &lon}), ValidateOptions{SkipHostChecks: true}, ""},
		{"stored half point", mapOf(MapPin{ID: "a", Place: "place:999999999999", Lat: &lat}), ValidateOptions{SkipHostChecks: true}, "come as a pair"},
		{"place with location", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Location: "place:000000000001"}}, places(), ""},
		{"place with name and location", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Name: "Lalbagh", Location: "place:000000000001"}}, places(), ""},
		{"place without name or location", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Category: "garden"}}, places(), "place.name"},
		{"place unknown location", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Location: "place:000000000007"}}, places(), "place.location place:000000000007 was not found by find_place in this chat"},
		{"place typed coordinates", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Location: "place:000000000001", Lat: &lat, Lon: &lon}}, places(), "place.lat and .lon are filled in"},
		{"stored coordinates without location", Component{ID: "p", Type: "place", Summary: "s", Place: &Place{Name: "x", Lat: &lat, Lon: &lon}}, ValidateOptions{SkipHostChecks: true}, "come as a pair with a place id"},
		{"itinerary locations", Component{ID: "i", Type: "itinerary", Summary: "s", Itinerary: &Itinerary{Days: []ItineraryDay{{ID: "d", Label: "Day", Items: []ItineraryItem{
			{ID: "a", Title: "Garden", Location: "place:000000000001"}, {ID: "b", Title: "Pier", Location: "place:5e1c7a09b3d2"},
		}}}}}, places(), ""},
		{"itinerary unknown location", Component{ID: "i", Type: "itinerary", Summary: "s", Itinerary: &Itinerary{Days: []ItineraryDay{{ID: "d", Label: "Day", Items: []ItineraryItem{
			{ID: "a", Title: "Garden", Location: "place:000000000001"}, {ID: "b", Title: "Pier", Location: "place:000000000005"},
		}}}}}, places(), "itinerary.days[0].items[1].location place:000000000005 was not found by find_place in this chat"},
		{"itinerary typed coordinates", Component{ID: "i", Type: "itinerary", Summary: "s", Itinerary: &Itinerary{Days: []ItineraryDay{{ID: "d", Label: "Day", Items: []ItineraryItem{
			{ID: "a", Title: "Garden", Location: "place:000000000001", Lat: &lat, Lon: &lon},
		}}}}}, places(), "itinerary.days[0].items[0].lat and .lon are filled in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOne(tc.component, tc.opts)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("Validate = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestStampPlaces(t *testing.T) {
	surface := Surface{Schema: Schema, Title: "Day out", State: StateReady, Components: []Component{
		mapOf(MapPin{ID: "garden", Place: "place:000000000001"}, MapPin{ID: "pier", Place: "place:5e1c7a09b3d2", Label: "The pier"}, MapPin{ID: "long", Place: "place:000000000002"}),
		{ID: "card", Type: "place", Summary: "s", Place: &Place{Location: "place:000000000001", Address: "Main gate"}},
		{ID: "plan", Type: "itinerary", Summary: "s", Itinerary: &Itinerary{Days: []ItineraryDay{{ID: "d", Label: "Day", Items: []ItineraryItem{
			{ID: "a", Title: "Garden", Location: "place:000000000001"}, {ID: "b", Title: "Pier", Place: "On the front", Location: "place:5e1c7a09b3d2"}, {ID: "c", Title: "Home"},
		}}}}},
	}}
	if _, err := Validate(context.Background(), surface, places()); err != nil {
		t.Fatal(err)
	}
	if ids := PlaceIDs(surface); strings.Join(ids, ",") != "place:000000000001,place:5e1c7a09b3d2,place:000000000002" {
		t.Fatalf("PlaceIDs = %v", ids)
	}
	before, _ := json.Marshal(surface)
	fixes, err := ResolvePlaces(context.Background(), surface, examplePlaces)
	if err != nil || len(fixes) != 3 {
		t.Fatalf("ResolvePlaces = %v, %v", fixes, err)
	}
	stamped, err := StampPlaces(surface, fixes)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := json.Marshal(surface); string(after) != string(before) {
		t.Fatal("StampPlaces changed its input")
	}
	pins := stamped.Components[0].Map.Pins
	if pins[0].Label != "Lalbagh Botanical Garden" || pins[0].Address != "Mavalli, Bengaluru, Karnataka, India" || *pins[0].Lat != 12.9507 || *pins[0].Lon != 77.5848 {
		t.Fatalf("pin = %+v", pins[0])
	}
	if pins[1].Label != "The pier" || *pins[1].Lat != 50.8164 {
		t.Fatalf("labelled pin = %+v", pins[1])
	}
	if label := []rune(pins[2].Label); len(label) != MaxMapPinLabelRunes || label[len(label)-1] != '…' || pins[2].Address != "" {
		t.Fatalf("long pin = %+v", pins[2])
	}
	card := stamped.Components[1].Place
	if card.Name != "Lalbagh Botanical Garden" || card.Address != "Main gate" || card.Category != "garden" || *card.Lat != 12.9507 {
		t.Fatalf("place = %+v", card)
	}
	items := stamped.Components[2].Itinerary.Days[0].Items
	if items[0].Place != "Lalbagh Botanical Garden" || *items[0].Lon != 77.5848 || items[1].Place != "On the front" || *items[1].Lat != 50.8164 || items[2].Lat != nil || items[2].Place != "" {
		t.Fatalf("items = %+v", items)
	}
	// Stamped visuals are stored: they encode and decode, but the model
	// may not send them back with coordinates.
	encoded, err := Encode(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(context.Background(), stamped, places()); err == nil || !strings.Contains(err.Error(), "filled in from find_place") {
		t.Fatalf("stamped visual from the model = %v", err)
	}
	if _, err := StampPlaces(surface, map[string]PlaceFix{"place:000000000001": knownPlaces["place:000000000001"]}); err == nil || !strings.Contains(err.Error(), "no fix for place place:5e1c7a09b3d2") {
		t.Fatalf("missing fix = %v", err)
	}
	if _, err := StampPlaces(surface, map[string]PlaceFix{"place:000000000001": {Name: "x", Lat: 91}, "place:5e1c7a09b3d2": knownPlaces["place:5e1c7a09b3d2"], "place:000000000002": knownPlaces["place:000000000002"]}); err == nil {
		t.Fatal("out of range fix stamped")
	}
	if _, err := ResolvePlaces(context.Background(), surface, func(context.Context, string) (PlaceFix, bool, error) { return PlaceFix{}, false, nil }); err == nil {
		t.Fatal("unknown place resolved")
	}
	if fixes, err := ResolvePlaces(context.Background(), Surface{Components: []Component{text("t", "x")}}, nil); err != nil || len(fixes) != 0 {
		t.Fatalf("no places = %v, %v", fixes, err)
	}
}

func TestLintWave5(t *testing.T) {
	located := func(locations ...string) Component {
		items := make([]ItineraryItem, len(locations))
		for index, location := range locations {
			items[index] = ItineraryItem{ID: fmt.Sprintf("i%d", index), Title: "Stop", Location: location}
		}
		return Component{ID: "plan", Type: "itinerary", Summary: "s", Itinerary: &Itinerary{Days: []ItineraryDay{{ID: "d", Label: "Day", Items: items}}}}
	}
	cases := []struct {
		component Component
		want      string
	}{
		{mapOf(MapPin{ID: "a", Place: "place:000000000001"}), LintMapOnePin},
		{mapOf(MapPin{ID: "a", Place: "place:000000000001"}, MapPin{ID: "b", Place: "place:000000000002"}), ""},
		{located("place:000000000001", ""), LintItineraryOneLocation},
		{located("place:000000000001", "place:000000000002"), ""},
		{located("", ""), ""},
	}
	for _, tc := range cases {
		var codes []string
		for _, warning := range Lint(Surface{Components: []Component{tc.component}}) {
			codes = append(codes, warning.Code)
		}
		if got := strings.Join(codes, ","); got != tc.want {
			t.Fatalf("Lint(%s) = %q, want %q", tc.component.Type, got, tc.want)
		}
	}
}

// The model-facing format never offers the fields the host stamps in.
func TestStampedFieldsAreNotModelFacing(t *testing.T) {
	properties := componentJSONSchema()["properties"].(map[string]interface{})
	pin := properties["map"].(map[string]interface{})["properties"].(map[string]interface{})["pins"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	place := properties["place"].(map[string]interface{})["properties"].(map[string]interface{})
	item := properties["itinerary"].(map[string]interface{})["properties"].(map[string]interface{})["days"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})["items"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	for name, fields := range map[string]map[string]interface{}{"pin": pin, "place": place, "item": item} {
		for _, key := range []string{"lat", "lon"} {
			if fields[key] != nil {
				t.Fatalf("%s offers %s", name, key)
			}
		}
	}
	if pin["address"] != nil || pin["place"] == nil || place["location"] == nil || item["location"] == nil {
		t.Fatalf("pin %v, place %v, item %v", pin, place, item)
	}
	format := VisualFormat()
	for _, want := range []string{"placeId = str/" + PlaceIDPattern + "/ (from find_place)", "\nmap*: Pins for 1-20 real places from find_place; tap a pin for its detail.", "location?:placeId"} {
		if !strings.Contains(format, want) {
			t.Fatalf("format lacks %q", want)
		}
	}
	for _, word := range []string{"lat:", "lon:", "lat?:", "lon?:"} {
		if strings.Contains(format, word) {
			t.Fatalf("format offers %s", word)
		}
	}
}
