package builtin

import "github.com/axiom-studio/openseal/pkg/skill"

func documentActions() map[string]skill.Action {
	text := func(max int) map[string]interface{} {
		return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": max}
	}
	// path is absent only for work outside any chat, which has no files.
	rendered := object([]interface{}{"artifactRefs"}, map[string]interface{}{
		"path": map[string]interface{}{"type": "string"}, "artifactRefs": artifactRefs,
		"pageCount": map[string]interface{}{"type": "integer", "minimum": 1},
	})
	pdfBlock := func(kinds ...interface{}) map[string]interface{} {
		return object([]interface{}{"kind"}, map[string]interface{}{
			"kind":    map[string]interface{}{"type": "string", "enum": kinds},
			"text":    map[string]interface{}{"type": "string", "maxLength": 10000},
			"path":    chatPath("For an image block: the image's path, such as images/chart.png."),
			"caption": map[string]interface{}{"type": "string", "maxLength": 500},
		})
	}
	cell := map[string]interface{}{"oneOf": []interface{}{
		map[string]interface{}{"type": "string", "maxLength": 32767}, map[string]interface{}{"type": "number"}, map[string]interface{}{"type": "boolean"}, map[string]interface{}{"type": "null"},
	}}
	sheet := object([]interface{}{"name", "rows"}, map[string]interface{}{
		"name":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 31},
		"rows":   map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 10000, "items": map[string]interface{}{"type": "array", "maxItems": 256, "items": cell}},
		"header": map[string]interface{}{"type": "boolean", "description": "Style and freeze the first row as a header."},
	})
	write := func(name, description string, input map[string]interface{}, artifactType string) skill.Action {
		return skill.Action{Name: name, Description: description, InputSchema: input, OutputSchema: rendered,
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{artifactType}}
	}
	return map[string]skill.Action{
		CreatePDF: write(CreatePDF, "Render a styled Unicode PDF to a path in this chat's files and show it. Supply body for simple text, pages as strings for one text page each, or pages with blocks for headings, paragraphs and images. To change it, call again with the same path and the complete new content. For an exact page count, set expectedPages; the host verifies the rendered count.",
			object([]interface{}{"path"}, map[string]interface{}{
				"path":          chatPath("Where to save it, such as docs/q3-report.pdf."),
				"title":         text(300),
				"body":          text(100000),
				"expectedPages": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 100},
				"blocks":        map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 100, "items": pdfBlock("heading", "paragraph", "image", "page_break")},
				"pages": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 30,
					"items": map[string]interface{}{"anyOf": []interface{}{text(10000), object([]interface{}{"blocks"}, map[string]interface{}{
						"blocks": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 20, "items": pdfBlock("heading", "paragraph", "image")},
					})}},
				},
			}), "report"),
		CreateSheet: write(CreateSheet, "Render an XLSX workbook with named sheets of literal cell values (not formulas) to a path in this chat's files and show it. To change it, call again with the same path and the complete new content.",
			object([]interface{}{"path", "sheets"}, map[string]interface{}{
				"path":   chatPath("Where to save it, such as sheets/pipeline.xlsx."),
				"title":  text(300),
				"sheets": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 20, "items": sheet},
			}), "spreadsheet"),
	}
}
