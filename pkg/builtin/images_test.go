package builtin

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestFindImageInputTakesAQueryAndLimit(t *testing.T) {
	for _, tc := range []struct {
		arguments map[string]interface{}
		query     string
		limit     int
	}{
		{map[string]interface{}{"query": "  Tokyo   skyline "}, "Tokyo skyline", DefaultImageResults},
		{map[string]interface{}{"query": "Eiffel Tower", "limit": 5}, "Eiffel Tower", 5},
		{map[string]interface{}{"query": "Eiffel Tower", "limit": 1}, "Eiffel Tower", 1},
	} {
		input, err := ParseFindImageInput(tc.arguments)
		if err != nil || input.Query != tc.query || input.Limit != tc.limit {
			t.Fatalf("%v: input = %+v, %v", tc.arguments, input, err)
		}
	}
	for name, arguments := range map[string]map[string]interface{}{
		"no query":       {},
		"only limit":     {"limit": 2},
		"blank query":    {"query": "   "},
		"long query":     {"query": strings.Repeat("a", MaximumImageQueryRunes+1)},
		"query not text": {"query": 42},
		"limit zero":     {"query": "Tokyo", "limit": 0},
		"limit six":      {"query": "Tokyo", "limit": 6},
		"page url":       {"query": "Tokyo", "pageUrl": "https://example.com/"},
		"unknown field":  {"query": "Tokyo", "url": "https://example.com/"},
	} {
		if _, err := ParseFindImageInput(arguments); err == nil {
			t.Fatalf("%s accepted: %v", name, arguments)
		}
	}
}

func TestFindImageContractReturnsSavedImages(t *testing.T) {
	catalog, d := builtinContractCatalog(t)
	a := d.Actions[FindImage]
	if a.InputSchema["additionalProperties"] != false || a.Retry.MaxAttempts != 1 || string(a.SideEffect) != "write" {
		t.Fatalf("find_image = %#v", a)
	}
	if !strings.Contains(d.Prompt.Instructions, "Find photos with find_image") {
		t.Fatal("the prompt must say when to use find_image")
	}
	for _, input := range []map[string]interface{}{{"query": "Tokyo"}, {"query": "Tokyo", "limit": 1}} {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, skill.DefinitionSourceIdentity(d), FindImage, input); err != nil {
			t.Fatalf("valid find_image input rejected: %v, %v", input, err)
		}
	}
	for _, input := range []map[string]interface{}{{"limit": 2}, {"query": "Tokyo", "pageUrl": "https://example.com/a"}, {"query": "Tokyo", "limit": 6}, {"query": "Tokyo", "near": "x"}} {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, skill.DefinitionSourceIdentity(d), FindImage, input); err == nil {
			t.Fatalf("invalid find_image input accepted: %v", input)
		}
	}
	bound := &skill.BoundAction{Definition: d, Binding: &skill.Binding{}, Action: a}
	image := map[string]interface{}{"image": map[string]interface{}{"artifactId": "file-1", "version": 1}, "title": "Tokyo Tower", "credit": "Jane Doe",
		"license": "CC BY-SA 4.0", "sourceUrl": "https://commons.wikimedia.org/wiki/File:Tokyo_Tower.jpg", "width": 800, "height": 600}
	for _, output := range []map[string]interface{}{{"images": []interface{}{image}}, {"images": []interface{}{}}} {
		if err := catalog.ValidateOutput(t.Context(), bound, output); err != nil {
			t.Fatalf("valid find_image result rejected: %v", err)
		}
	}
	for _, output := range []map[string]interface{}{
		{"images": []interface{}{map[string]interface{}{"title": "x", "credit": "x", "license": "", "sourceUrl": "https://x"}}},
		{"images": []interface{}{map[string]interface{}{"image": map[string]interface{}{"artifactId": "file-1", "version": 0}, "title": "x", "credit": "x", "license": "", "sourceUrl": "https://x"}}},
		{"images": []interface{}{image, image, image, image, image, image}},
	} {
		if err := catalog.ValidateOutput(t.Context(), bound, output); err == nil {
			t.Fatalf("invalid find_image result accepted: %v", output)
		}
	}
}
