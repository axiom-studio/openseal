package builtin

import "github.com/axiom-studio/openseal/pkg/skill"

func creativeDocumentActions() map[string]skill.Action {
	stringField := func(max int) map[string]interface{} {
		return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": max}
	}
	fileField := func(extension string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9 ._-]{0,119}\.` + extension + `$`}
	}
	artifactOutput := func(preview bool) map[string]interface{} {
		properties := map[string]interface{}{"pageCount": map[string]interface{}{"type": "integer", "minimum": 1}, "artifactRefs": map[string]interface{}{
			"type": "array", "minItems": 1, "maxItems": 1,
			"items": map[string]interface{}{"type": "object", "additionalProperties": false,
				"required":   []interface{}{"id", "version"},
				"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "integer", "minimum": 1}},
			},
		}}
		required := []interface{}{"artifactRefs"}
		if preview {
			properties["previewPath"] = map[string]interface{}{"type": "string", "minLength": 1}
			required = append(required, "previewPath")
		}
		return map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": required,
			"properties": properties,
		}
	}
	makeAction := func(name, description string, input map[string]interface{}, artifactType string) skill.Action {
		return skill.Action{Name: name, Description: description, InputSchema: input, OutputSchema: artifactOutput(name == CreateSlides),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{artifactType}}
	}
	pdfBlock := func(kinds ...string) map[string]interface{} {
		enums := make([]interface{}, 0, len(kinds))
		for _, kind := range kinds {
			enums = append(enums, kind)
		}
		return map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"kind"},
			"properties": map[string]interface{}{
				"kind":       map[string]interface{}{"type": "string", "enum": enums},
				"text":       map[string]interface{}{"type": "string", "maxLength": 10000},
				"artifactId": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
				"version":    map[string]interface{}{"type": "integer", "minimum": 1},
				"caption":    map[string]interface{}{"type": "string", "maxLength": 500},
			},
		}
	}
	return map[string]skill.Action{
		CreatePDF: makeAction(CreatePDF, "Create a styled Unicode PDF. Supply body for simple text, pages as strings for one text page each, or pages with blocks for headings and generated images. The host supplies a title and filename when omitted. For exact page counts, set expectedPages; the host verifies the rendered count. Reuse documentId with expectedLatestVersion to revise: revisionMode append (default) adds a short section, while replace renders the supplied complete document as the new version. Report only the final revision.",
			map[string]interface{}{"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"title": stringField(300), "body": stringField(100000), "filename": fileField("pdf"),
					"documentId":            map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`},
					"revisionMode":          map[string]interface{}{"type": "string", "enum": []interface{}{"append", "replace"}},
					"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0},
					"expectedPages":         map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 100},
					"blocks":                map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 100, "items": pdfBlock("heading", "paragraph", "image", "page_break")},
					"pages": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 30,
						"items": map[string]interface{}{"anyOf": []interface{}{stringField(10000), map[string]interface{}{"type": "object", "additionalProperties": false,
							"required": []interface{}{"blocks"}, "properties": map[string]interface{}{
								"blocks": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 20, "items": pdfBlock("heading", "paragraph", "image")},
							}}}},
					},
				}}, "report"),
		CreateSlides: makeAction(CreateSlides, "Create a self-contained HTML slide deck for the full-screen presentation viewer.",
			map[string]interface{}{"type": "object", "additionalProperties": false,
				"required": []interface{}{"deckId", "expectedLatestVersion", "title", "slides", "filename"},
				"properties": map[string]interface{}{
					"title": stringField(300), "filename": fileField("html"),
					"deckId":                map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`},
					"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0},
					"slides": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 40,
						"items": map[string]interface{}{"type": "object", "additionalProperties": false,
							"required": []interface{}{"title", "bullets"},
							"properties": map[string]interface{}{"title": stringField(200),
								"bullets": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 8, "items": stringField(500)},
								"notes":   map[string]interface{}{"type": "string", "maxLength": 3000}},
						}},
				}}, "slide_deck"),
	}
}
