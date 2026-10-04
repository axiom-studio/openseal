// Package creation supplies host-rendered page and workbook contracts and content.
package creation

import (
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	Page              = "page"
	Workbook          = "spreadsheet"
	WorkbookMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

func PageAction() skill.Action {
	return action("create_page", "Page", Page, "html", map[string]interface{}{
		"html": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 500000, "description": "Complete self-contained HTML document with embedded styling. No external build step."},
	})
}

func SheetAction() skill.Action {
	cell := map[string]interface{}{"oneOf": []interface{}{
		map[string]interface{}{"type": "string", "maxLength": 32767}, map[string]interface{}{"type": "number"}, map[string]interface{}{"type": "boolean"}, map[string]interface{}{"type": "null"},
	}}
	sheet := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"name", "rows"}, "properties": map[string]interface{}{
		"name":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 31},
		"rows":   map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 10000, "items": map[string]interface{}{"type": "array", "maxItems": 256, "items": cell}},
		"header": map[string]interface{}{"type": "boolean", "description": "Style and freeze the first row as a header."},
	}}
	return action("create_sheet", "Sheet", Workbook, "xlsx", map[string]interface{}{
		"sheets": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 20, "items": sheet},
	})
}

func action(action, name, artifactType, extension string, content map[string]interface{}) skill.Action {
	properties := map[string]interface{}{
		"artifactId":            map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`},
		"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 1000000, "description": "0 creates an artifact; use the last returned version to revise the same artifact."},
		"title":                 map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300},
		"filename":              map[string]interface{}{"type": "string", "pattern": `^[^/\\]{1,240}\.` + extension + `$`},
		"requirementName":       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128},
	}
	required := []interface{}{"artifactId", "expectedLatestVersion", "title", "filename", "requirementName"}
	for key, schema := range content {
		properties[key] = schema
		required = append(required, key)
	}
	refs := map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"id", "version", "requirementName"}, "properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "integer", "minimum": 1}, "requirementName": map[string]interface{}{"type": "string"},
		},
	}}
	return skill.Action{Name: action, Description: "Create or revise a " + name + " artifact in the host. Retain artifactId and pass the current expectedLatestVersion for revisions, supplying complete content after reading the current artifact. Preserve unrelated content. Workbook cells are literal values, not formulas.", InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"artifactRefs"}, "properties": map[string]interface{}{"artifactRefs": refs}},
		SideEffect:   skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{artifactType}}
}
