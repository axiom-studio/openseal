package builtin

import (
	"github.com/axiom-studio/openseal/pkg/appproject"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Artifact types an app emits: the private, editable project and the public
// snapshot made by publish_app.
const (
	AppArtifactType          = "app"
	PublishedAppArtifactType = "published_app"
	// AppIDPattern matches host-assigned app identifiers.
	AppIDPattern = `^app-[a-f0-9]{64}$`
	// AppDataArtifactType is one named dataset an app reads; each save is a
	// new version, so its history is kept.
	AppDataArtifactType = "app_data"
	// AppDataNamePattern matches dataset names an app reads with getData.
	AppDataNamePattern = `^[A-Za-z0-9_.-]{1,64}$`
	// MaximumAppDataBytes bounds one dataset snapshot.
	MaximumAppDataBytes = 1024 * 1024
)

func appActions() map[string]skill.Action {
	object := func(required []interface{}, properties map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	appID := map[string]interface{}{"type": "string", "pattern": AppIDPattern}
	version := map[string]interface{}{"type": "integer", "minimum": 1}
	title := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300}
	filePath := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": appproject.MaximumPathBytes,
		"description": "Relative path such as src/App.tsx or src/components/Chart.tsx."}
	content := map[string]interface{}{"type": "string", "maxLength": appproject.MaximumFileBytes}
	file := object([]interface{}{"path", "content"}, map[string]interface{}{"path": filePath, "content": content})
	files := func(minimum int) map[string]interface{} {
		return map[string]interface{}{"type": "array", "minItems": minimum, "maxItems": appproject.MaximumEdits, "items": file}
	}
	replace := map[string]interface{}{"type": "array", "maxItems": appproject.MaximumEdits, "items": object([]interface{}{"path", "find", "replace"}, map[string]interface{}{
		"path": filePath, "find": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": appproject.MaximumFileBytes, "description": "Exact text that occurs once in the file."},
		"replace": content,
	})}
	refs := map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": object([]interface{}{"id", "version"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string"}, "version": version,
	})}
	saved := object([]interface{}{"artifactRefs", "appId", "previewPath"}, map[string]interface{}{
		"artifactRefs": refs, "appId": appID, "previewPath": map[string]interface{}{"type": "string"},
	})
	return map[string]skill.Action{
		CreateApp: {Name: CreateApp,
			Description:  "Create an app: a TypeScript project shown live in the chat's canvas. src/App.tsx must export the default App component. Import react and @openseal/app; other packages are not available. Use it for websites, dashboards, forms and tools.",
			InputSchema:  object([]interface{}{"title", "files"}, map[string]interface{}{"title": title, "files": files(1)}),
			OutputSchema: saved, SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{AppArtifactType}},
		UpdateApp: {Name: UpdateApp,
			Description: "Change an existing app by its appId and expectedLatestVersion. write replaces whole files, replace swaps one exact unique snippet, delete removes files. The canvas updates as soon as it saves; build errors come back so you can fix them.",
			InputSchema: object([]interface{}{"appId", "expectedLatestVersion"}, map[string]interface{}{
				"appId": appID, "expectedLatestVersion": version, "title": title, "write": files(0), "replace": replace,
				"delete": map[string]interface{}{"type": "array", "maxItems": appproject.MaximumEdits, "items": filePath},
			}),
			OutputSchema: saved, SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{AppArtifactType}},
		ReadApp: {Name: ReadApp,
			Description: "List an app's files and read the ones you name. Read before changing a file you have not seen in this conversation.",
			InputSchema: object([]interface{}{"appId"}, map[string]interface{}{
				"appId": appID, "version": version,
				"paths": map[string]interface{}{"type": "array", "maxItems": 50, "items": filePath},
			}),
			OutputSchema: object([]interface{}{"appId", "version", "title", "files"}, map[string]interface{}{
				"appId": appID, "version": version, "title": map[string]interface{}{"type": "string"},
				"files": map[string]interface{}{"type": "array", "items": object([]interface{}{"path", "sizeBytes"}, map[string]interface{}{
					"path": map[string]interface{}{"type": "string"}, "sizeBytes": map[string]interface{}{"type": "integer", "minimum": 0},
				})},
				"contents": map[string]interface{}{"type": "array", "items": file},
			}),
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}},
		SaveAppData: {Name: SaveAppData,
			Description: "Save one named dataset for an app, such as pipeline_by_stage. The app reads the latest snapshot with useData or getData and shows when it was updated. Each save keeps the previous snapshots as history. value is any JSON up to 1 MB.",
			InputSchema: object([]interface{}{"appId", "name", "value"}, map[string]interface{}{
				"appId": appID,
				"name":  map[string]interface{}{"type": "string", "pattern": AppDataNamePattern},
				"value": map[string]interface{}{"description": "JSON value: object, array, string, number, boolean or null."},
			}),
			OutputSchema: object([]interface{}{"artifactRefs", "name", "updatedAt"}, map[string]interface{}{
				"artifactRefs": refs, "name": map[string]interface{}{"type": "string"}, "updatedAt": map[string]interface{}{"type": "string"},
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{AppDataArtifactType}},
		PublishApp: {Name: PublishApp,
			Description:  "Publish the current version of an app at a stable public internet address. Later changes stay private until it is published again. Only when the user asks for a public link.",
			InputSchema:  object([]interface{}{"appId", "expectedLatestVersion"}, map[string]interface{}{"appId": appID, "expectedLatestVersion": version}),
			OutputSchema: object([]interface{}{"artifactRefs", "publicPath"}, map[string]interface{}{"artifactRefs": refs, "publicPath": map[string]interface{}{"type": "string"}}),
			SideEffect:   skill.SideEffectExternal, Risk: skill.RiskLevelExternal, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{PublishedAppArtifactType}},
	}
}
