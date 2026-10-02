package builtin

import "github.com/axiom-studio/openseal/pkg/skill"

func siteActions() map[string]skill.Action {
	artifactRefs := map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"id", "version"},
		"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "integer", "minimum": 1}},
	}}
	output := func(public bool) map[string]interface{} {
		required := []interface{}{"artifactRefs"}
		properties := map[string]interface{}{"artifactRefs": artifactRefs}
		if public {
			required = append(required, "publicPath")
			properties["publicPath"] = map[string]interface{}{"type": "string"}
		} else {
			required = append(required, "previewPath")
			properties["previewPath"] = map[string]interface{}{"type": "string"}
		}
		return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	siteID := map[string]interface{}{"type": "string", "pattern": `^site-[a-f0-9]{64}$`}
	source := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 500000}
	title := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300}
	version := map[string]interface{}{"type": "integer", "minimum": 1}
	return map[string]skill.Action{
		CreateSite: {Name: CreateSite, Description: "Create a private hot-reload site preview from React JSX and CSS. Export a default App component from reactSource; react is the supported import. HTML is retained for older drafts. Use publish_site to make it public.",
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"required": []interface{}{"title"}, "oneOf": []interface{}{map[string]interface{}{"required": []interface{}{"reactSource"}}, map[string]interface{}{"required": []interface{}{"html"}}}, "properties": map[string]interface{}{"title": title, "html": source, "reactSource": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 60000}, "css": map[string]interface{}{"type": "string", "maxLength": 60000}}},
			OutputSchema: output(false), SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{"site_draft"}},
		UpdateSite: {Name: UpdateSite, Description: "Edit the existing React site using its siteId and expectedLatestVersion. The running canvas updates automatically; retain React source and CSS rather than creating a new HTML file.",
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"required": []interface{}{"siteId", "expectedLatestVersion", "title"},
				"oneOf":    []interface{}{map[string]interface{}{"required": []interface{}{"reactSource"}}, map[string]interface{}{"required": []interface{}{"html"}}}, "properties": map[string]interface{}{"siteId": siteID, "expectedLatestVersion": version, "title": title, "html": source, "reactSource": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 60000}, "css": map[string]interface{}{"type": "string", "maxLength": 60000}}},
			OutputSchema: output(false), SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{"site_draft"}},
		PublishSite: {Name: PublishSite, Description: "Publish the reviewed current draft as a public snapshot at a stable internet URL. Future draft edits stay private until published again.",
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"required":   []interface{}{"siteId", "expectedLatestVersion"},
				"properties": map[string]interface{}{"siteId": siteID, "expectedLatestVersion": version}},
			OutputSchema: output(true), SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{"published_site"}},
	}
}
