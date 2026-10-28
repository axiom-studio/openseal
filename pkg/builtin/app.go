package builtin

import "github.com/axiom-studio/openseal/pkg/skill"

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
	appPath := chatPath("The app's directory, such as apps/tracker.")
	return map[string]skill.Action{
		SaveApp: {Name: SaveApp,
			Description: "Build the app in a directory of this chat's files (its src/App.tsx exports the default App component) and show it live. Each save is a new version the person can view, undo, download and publish. Build errors come back so you can fix the files and save again.",
			InputSchema: object([]interface{}{"path"}, map[string]interface{}{
				"path":  appPath,
				"title": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300, "description": "The app's name; kept from the last save when omitted."},
			}),
			OutputSchema: object([]interface{}{"artifactRefs", "appId", "path", "previewPath"}, map[string]interface{}{
				"artifactRefs": artifactRefs, "appId": map[string]interface{}{"type": "string", "pattern": AppIDPattern},
				"path": map[string]interface{}{"type": "string"}, "previewPath": map[string]interface{}{"type": "string"},
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{AppArtifactType}},
		SaveAppData: {Name: SaveAppData,
			Description: "Save one named dataset for an app, such as pipeline_by_stage. The app reads the latest snapshot with useData or getData and shows when it was updated. Each save keeps the previous snapshots as history. value is any JSON up to 1 MB.",
			InputSchema: object([]interface{}{"path", "name", "value"}, map[string]interface{}{
				"path":  appPath,
				"name":  map[string]interface{}{"type": "string", "pattern": AppDataNamePattern},
				"value": map[string]interface{}{"description": "JSON value: object, array, string, number, boolean or null."},
			}),
			OutputSchema: object([]interface{}{"artifactRefs", "name", "updatedAt"}, map[string]interface{}{
				"artifactRefs": artifactRefs, "name": map[string]interface{}{"type": "string"}, "updatedAt": map[string]interface{}{"type": "string"},
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{AppDataArtifactType}},
		ReadAppData: {Name: ReadAppData,
			Description: "Read a dataset you saved for one of your apps in this chat.",
			InputSchema: object([]interface{}{"app", "name"}, map[string]interface{}{
				"app":  map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024, "description": "The app's directory, such as apps/tracker, or its appId."},
				"name": map[string]interface{}{"type": "string", "pattern": AppDataNamePattern},
			}),
			OutputSchema: object([]interface{}{"name", "value", "version", "updatedAt"}, map[string]interface{}{
				"name": map[string]interface{}{"type": "string"}, "value": map[string]interface{}{"description": "The saved JSON value."},
				"version": map[string]interface{}{"type": "integer", "minimum": 1}, "updatedAt": map[string]interface{}{"type": "string"},
			}),
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}},
		PublishApp: {Name: PublishApp,
			Description:  "Publish the last saved version of the app in a directory at a stable public internet address. Later saves stay private until it is published again. Only when the user asks for a public link.",
			InputSchema:  object([]interface{}{"path"}, map[string]interface{}{"path": appPath}),
			OutputSchema: object([]interface{}{"artifactRefs", "publicPath"}, map[string]interface{}{"artifactRefs": artifactRefs, "publicPath": map[string]interface{}{"type": "string"}}),
			SideEffect:   skill.SideEffectExternal, Risk: skill.RiskLevelExternal, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{PublishedAppArtifactType}},
	}
}
