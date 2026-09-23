package workspace

const (
	OperationListDirectory = "workspace.list_directory"
	OperationReadFile      = "workspace.read_file"
	OperationSearchFiles   = "workspace.search_files"
	OperationBatchRead     = "workspace.batch_read"
	OperationWriteFile     = "workspace.write_file"
	OperationApplyPatch    = "workspace.apply_patch"
	OperationRunCommand    = "workspace.run_command"
	OperationGitClone      = "workspace.git_clone"
	OperationGitPush       = "workspace.git_push"
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
		{Name: OperationBatchRead, Description: "Run two to eight independent Workspace list, read, or search requests concurrently and return their results in request order. Use only when no request depends on another result.", InputSchema: batchReadInput()},
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
	if authority.Workspace.Policy.Git.Enabled {
		result = append(result, Operation{Name: OperationGitClone, Description: "Clone one repository from an explicitly allowed HTTPS host into the Workspace.", InputSchema: gitCloneInput()})
		if authority.Workspace.Policy.Git.PushEnabled {
			result = append(result, Operation{Name: OperationGitPush, Description: "Push the current commit to one branch on an explicitly allowed HTTPS remote.", InputSchema: gitPushInput()})
		}
	}
	return result
}

func batchReadInput() map[string]interface{} {
	branches := []interface{}{}
	for _, item := range []struct {
		name   string
		schema map[string]interface{}
	}{{OperationListDirectory, listInput()}, {OperationReadFile, pathInput()}, {OperationSearchFiles, searchInput()}} {
		branches = append(branches, map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"required": []interface{}{"operation", "arguments"},
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{"type": "string", "const": item.name},
				"arguments": item.schema,
			},
		})
	}
	return map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"calls"},
		"properties": map[string]interface{}{
			"calls": map[string]interface{}{"type": "array", "minItems": 2, "maxItems": 8, "items": map[string]interface{}{"oneOf": branches}},
		},
	}
}

func gitCloneInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"repositoryUrl", "path"}, "properties": map[string]interface{}{
		"repositoryUrl": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 2048, "pattern": `^https://`},
		"path":          relativePathProperty(),
		"branch":        map[string]interface{}{"type": "string", "maxLength": 255, "pattern": `^[A-Za-z0-9][A-Za-z0-9._/-]*$`},
	}}
}

func gitPushInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "branch"}, "properties": map[string]interface{}{
		"path":   relativePathProperty(),
		"remote": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, "pattern": `^[A-Za-z0-9][A-Za-z0-9._-]*$`, "default": "origin"},
		"branch": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 255, "pattern": `^[A-Za-z0-9][A-Za-z0-9._/-]*$`},
	}}
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
