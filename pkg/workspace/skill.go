package workspace

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID            = "openseal.workspace"
	SkillVersion       = "1.0.0"
	WorkspaceConfigKey = "workspaceId"

	ListDirectory = "list_directory"
	ReadFile      = "read_file"
	SearchFiles   = "search_files"
	WriteFile     = "write_file"
	ApplyPatch    = "apply_patch"
	RunCommand    = "run_command"
)

const (
	TransportList   = "workspace-list"
	TransportRead   = "workspace-read"
	TransportSearch = "workspace-search"
	TransportWrite  = "workspace-write"
	TransportPatch  = "workspace-patch"
	TransportRun    = "workspace-run"
)

func SkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Workspace",
		Description: "Inspect and modify files and run bounded tools inside the Agent's isolated Workspace.",
		Transport:   skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		BindingConfigSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{WorkspaceConfigKey},
			"properties": map[string]interface{}{WorkspaceConfigKey: map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128}},
		},
		Actions: map[string]skill.Action{
			ListDirectory: readAction(ListDirectory, "List files and directories in a Workspace path.", TransportList, listInput(), listOutput(), "workspace:files:read"),
			ReadFile:      readAction(ReadFile, "Read a bounded UTF-8 file from the Workspace.", TransportRead, pathInput(), fileOutput(), "workspace:files:read"),
			SearchFiles:   readAction(SearchFiles, "Search Workspace files for a text or regular-expression pattern.", TransportSearch, searchInput(), searchOutput(), "workspace:files:read"),
			WriteFile:     writeAction(WriteFile, "Create or replace a UTF-8 file in the Workspace.", TransportWrite, writeInput(), writeOutput(), skill.RiskLevelWrite, "workspace:files:write"),
			ApplyPatch:    writeAction(ApplyPatch, "Apply a unified diff inside the Workspace.", TransportPatch, patchInput(), writeOutput(), skill.RiskLevelWrite, "workspace:files:write"),
			RunCommand:    writeAction(RunCommand, "Run one bounded executable with explicit arguments inside the Workspace.", TransportRun, commandInput(), commandOutput(), skill.RiskLevelWrite, "workspace:process:execute"),
		},
		Prompt: &capability.PromptModule{
			Instructions:  "Use Workspace paths relative to the bound Workspace root. Inspect files before changing them, use apply_patch for focused edits, and run the narrowest relevant checks after a change. Never search for or print credentials.",
			UserInvocable: true,
			AllowedTools:  []string{ListDirectory, ReadFile, SearchFiles, WriteFile, ApplyPatch, RunCommand},
		},
	}
}

func readAction(name, description, endpoint string, input, output map[string]interface{}, permission string) skill.Action {
	return skill.Action{Name: name, Description: description, InputSchema: input, OutputSchema: output,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Permissions: []string{permission},
		Timeout: capability.Duration(30 * time.Second), Retry: skill.ActionRetryPolicy{MaxAttempts: 2, InitialBackoff: capability.Duration(time.Second)},
		Idempotency: skill.IdempotencySupported, Transport: &skill.TransportReference{Kind: "tool", Endpoint: endpoint}}
}

func writeAction(name, description, endpoint string, input, output map[string]interface{}, risk skill.RiskLevel, permission string) skill.Action {
	return skill.Action{Name: name, Description: description, InputSchema: input, OutputSchema: output,
		Risk: risk, SideEffect: skill.SideEffectWrite, Permissions: []string{permission},
		Timeout: capability.Duration(15 * time.Minute), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		Idempotency: skill.IdempotencyRequired, Transport: &skill.TransportReference{Kind: "tool", Endpoint: endpoint}}
}

func relativePathProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "maxLength": 4096, "pattern": `^(?:\.|[^/][^\x00]*)?$`, "default": "."}
}

func pathInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path"}, "properties": map[string]interface{}{"path": relativePathProperty(), "maxBytes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1048576, "default": 262144}}}
}

func listInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"path": relativePathProperty(), "depth": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 8, "default": 2}, "maxEntries": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 2000, "default": 500}}}
}

func searchInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"query"}, "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 4096}, "path": relativePathProperty(), "glob": map[string]interface{}{"type": "string", "maxLength": 512}, "regex": map[string]interface{}{"type": "boolean", "default": false}, "maxResults": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1000, "default": 200}}}
}

func writeInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "content"}, "properties": map[string]interface{}{"path": relativePathProperty(), "content": map[string]interface{}{"type": "string", "maxLength": 1048576}, "createParents": map[string]interface{}{"type": "boolean", "default": true}}}
}

func patchInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"patch"}, "properties": map[string]interface{}{"patch": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1048576}}}
}

func commandInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"executable"}, "properties": map[string]interface{}{"executable": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "pattern": `^[A-Za-z0-9._+/-]+$`}, "arguments": map[string]interface{}{"type": "array", "maxItems": 256, "items": map[string]interface{}{"type": "string", "maxLength": 8192}}, "workingDirectory": relativePathProperty(), "timeoutSeconds": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 900, "default": 300}}}
}

func listOutput() map[string]interface{} {
	return objectOutput(map[string]interface{}{"entries": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "type", "size"}, "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "type": map[string]interface{}{"type": "string", "enum": []interface{}{"file", "directory", "symlink"}}, "size": map[string]interface{}{"type": "integer", "minimum": 0}}}}, "truncated": map[string]interface{}{"type": "boolean"}}, []interface{}{"entries", "truncated"})
}
func fileOutput() map[string]interface{} {
	return objectOutput(map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "content": map[string]interface{}{"type": "string"}, "size": map[string]interface{}{"type": "integer", "minimum": 0}, "truncated": map[string]interface{}{"type": "boolean"}}, []interface{}{"path", "content", "size", "truncated"})
}
func searchOutput() map[string]interface{} {
	return objectOutput(map[string]interface{}{"matches": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "line", "text"}, "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "line": map[string]interface{}{"type": "integer", "minimum": 1}, "text": map[string]interface{}{"type": "string"}}}}, "truncated": map[string]interface{}{"type": "boolean"}}, []interface{}{"matches", "truncated"})
}
func writeOutput() map[string]interface{} {
	return objectOutput(map[string]interface{}{"success": map[string]interface{}{"type": "boolean", "const": true}, "changedPaths": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}}, []interface{}{"success", "changedPaths"})
}
func commandOutput() map[string]interface{} {
	return objectOutput(map[string]interface{}{"exitCode": map[string]interface{}{"type": "integer"}, "stdout": map[string]interface{}{"type": "string"}, "stderr": map[string]interface{}{"type": "string"}, "truncated": map[string]interface{}{"type": "boolean"}}, []interface{}{"exitCode", "stdout", "stderr", "truncated"})
}

func objectOutput(properties map[string]interface{}, required []interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}
