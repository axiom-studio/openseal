package workspace

const (
	OperationListDirectory = "workspace.list_directory"
	OperationReadFile      = "workspace.read_file"
	OperationSearchFiles   = "workspace.search_files"
	OperationWriteFile     = "workspace.write_file"
	OperationApplyPatch    = "workspace.apply_patch"
	OperationRunCommand    = "workspace.run_command"
)

// Operation is one framework-native Workspace tool projected to a hosted
// Agent turn. It has no Skill, binding, catalog, or marketplace identity.
type Operation struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func Operations(authority *Authority) []Operation {
	if authority == nil || authority.Validate() != nil {
		return nil
	}
	result := []Operation{
		{Name: OperationListDirectory, Description: "List bounded entries below a Workspace-relative path.", InputSchema: listInput()},
		{Name: OperationReadFile, Description: "Read a bounded UTF-8 file from the Workspace.", InputSchema: pathInput()},
		{Name: OperationSearchFiles, Description: "Search Workspace files for text or a regular expression.", InputSchema: searchInput()},
	}
	if authority.Workspace.Policy.Filesystem == AccessReadWrite {
		result = append(result,
			Operation{Name: OperationWriteFile, Description: "Create or replace a UTF-8 Workspace file.", InputSchema: writeInput()},
			Operation{Name: OperationApplyPatch, Description: "Apply a unified diff inside the Workspace.", InputSchema: patchInput()},
		)
	}
	if authority.Workspace.Policy.Commands.Enabled {
		result = append(result, Operation{Name: OperationRunCommand, Description: "Run one bounded executable with an explicit argument vector in the Workspace.", InputSchema: commandInput(authority.Workspace.Policy.Commands.MaxDurationSeconds)})
	}
	return result
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

func commandInput(maximum int) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"executable"}, "properties": map[string]interface{}{"executable": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "pattern": `^[A-Za-z0-9._+/-]+$`}, "arguments": map[string]interface{}{"type": "array", "maxItems": 256, "items": map[string]interface{}{"type": "string", "maxLength": 8192}}, "workingDirectory": relativePathProperty(), "timeoutSeconds": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maximum, "default": maximum}}}
}
