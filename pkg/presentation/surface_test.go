package presentation

import (
	"strings"
	"testing"
)

func TestSurfaceRoundTripsSafeComposableContent(t *testing.T) {
	surface := Surface{Schema: Schema, Title: "Launch plan", State: StateReady, Components: []Component{
		{ID: "summary", Type: "text", Text: "A concise **safe** summary."},
		{ID: "trend", Type: "chart", Chart: &Chart{Kind: "line", Series: []ChartSeries{{Name: "Adoption", Points: []ChartPoint{{Label: "Jan", Value: 2}, {Label: "Feb", Value: 5}}}}}},
		{ID: "flow", Type: "diagram", Diagram: &Diagram{Nodes: []DiagramNode{{ID: "a", Label: "Start"}, {ID: "b", Label: "Finish", X: 200}}, Edges: []DiagramEdge{{From: "a", To: "b"}}}},
		{ID: "choice", Type: "form", Form: &Form{Fields: []FormField{{ID: "region", Type: "select", Label: "Region", Options: []FormOption{{Label: "India", Value: "in"}}}}}},
	}}
	raw, err := Encode(surface)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Title != surface.Title || len(decoded.Components) != 4 {
		t.Fatalf("round trip = %#v", decoded)
	}
}

func TestSurfaceFailsClosed(t *testing.T) {
	cases := []Surface{
		{Schema: Schema, Title: "unsafe", State: StateReady, Components: []Component{{ID: "x", Type: "html", Text: "<script>alert(1)</script>"}}},
		{Schema: Schema, Title: "bad edge", State: StateReady, Components: []Component{{ID: "x", Type: "diagram", Diagram: &Diagram{Nodes: []DiagramNode{{ID: "a", Label: "A"}}, Edges: []DiagramEdge{{From: "a", To: "missing"}}}}}},
		{Schema: Schema, Title: "bad color", State: StateReady, Components: []Component{{ID: "x", Type: "canvas", Canvas: &Canvas{Width: 10, Height: 10, Shapes: []CanvasShape{{Kind: "line", Color: "url(javascript:bad)"}}}}}},
	}
	for _, value := range cases {
		if _, err := Encode(value); err == nil {
			t.Fatalf("expected rejection for %#v", value)
		}
	}
}

func TestSkillDefinitionExposesVersionedArtifactAction(t *testing.T) {
	definition := SkillDefinition()
	action := definition.Actions[Publish]
	if definition.ID != SkillID || action.EmittedArtifactTypes[0] != ArtifactType || !definition.Requirements.AlwaysAvailable {
		t.Fatalf("definition = %#v", definition)
	}
	if !strings.Contains(definition.Prompt.Instructions, "plot_chart") || !strings.Contains(definition.Prompt.Instructions, "publish_surface") || len(definition.Prompt.AllowedTools) != 2 {
		t.Fatalf("prompt = %q", definition.Prompt.Instructions)
	}
	plot := definition.Actions[PlotChart]
	if plot.InputSchema["additionalProperties"] != false || plot.InputSchema["properties"].(map[string]interface{})["points"] == nil ||
		plot.Retry.MaxAttempts != 1 || plot.EmittedArtifactTypes[0] != ArtifactType {
		t.Fatalf("bounded chart action = %#v", plot)
	}
	components := action.InputSchema["properties"].(map[string]interface{})["components"].(map[string]interface{})
	component := components["items"].(map[string]interface{})
	properties := component["properties"].(map[string]interface{})
	for _, name := range []string{"chart", "diagram", "canvas", "form", "table", "metric"} {
		payload := properties[name].(map[string]interface{})
		if payload["additionalProperties"] != false || payload["properties"] == nil {
			t.Fatalf("%s payload schema is not closed and explicit: %#v", name, payload)
		}
	}
	chart := properties["chart"].(map[string]interface{})["properties"].(map[string]interface{})
	series := chart["series"].(map[string]interface{})["items"].(map[string]interface{})
	if series["properties"].(map[string]interface{})["points"] == nil {
		t.Fatalf("chart series schema does not explain points: %#v", series)
	}
}
