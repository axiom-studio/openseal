package workspace

const (
	OperationListDirectory = "workspace.list_directory"
	OperationReadFile      = "workspace.read_file"
	OperationSearchFiles   = "workspace.search_files"
	OperationBatchRead     = "workspace.batch_read"
	OperationWriteFile     = "workspace.write_file"
	OperationEditFile      = "workspace.edit_file"
	OperationDeleteFile    = "workspace.delete_file"
	OperationMoveFile      = "workspace.move_file"
	OperationRunCommand    = "workspace.run_command"
	OperationListChats     = "workspace.list_chats"
	OperationCopyFromChat  = "workspace.copy_from_chat"
)

// ChatArgument names another chat of the same Agent in a read operation; the
// read then looks at that chat's files instead of this chat's.
const ChatArgument = "chat"

// Operation is one framework-native Workspace tool projected to a hosted
// Agent turn. It has no Skill, binding, catalog, or marketplace identity.
type Operation struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// Operations are the tools on a chat's files.
func Operations(authority *Authority) []Operation {
	if authority == nil || authority.Validate() != nil {
		return nil
	}
	return []Operation{
		{Name: OperationListDirectory, Description: "List the files below a path in this chat's files, or in another chat's when chat is set.", InputSchema: listInput()},
		{Name: OperationReadFile, Description: "Read a text file from this chat's files, or from another chat's when chat is set.", InputSchema: pathInput()},
		{Name: OperationSearchFiles, Description: "Search the text of this chat's files, or another chat's when chat is set, for text or a regular expression.", InputSchema: searchInput()},
		{Name: OperationBatchRead, Description: "Run two to eight independent list, read, or search requests concurrently and return their results in request order. Use only when no request depends on another result.", InputSchema: batchReadInput()},
		{Name: OperationWriteFile, Description: "Create or replace a text file in this chat's files.", InputSchema: writeInput()},
		{Name: OperationEditFile, Description: "Replace exact text in a file: oldText must match the file exactly, including whitespace, and appear once unless replaceAll is set. Read the file first.", InputSchema: editInput()},
		{Name: OperationDeleteFile, Description: "Delete a file, or a directory and everything in it.", InputSchema: pathOnlyInput()},
		{Name: OperationMoveFile, Description: "Move or rename a file or directory.", InputSchema: moveInput()},
		{Name: OperationRunCommand, Description: ChatSandboxDescription, InputSchema: shellInput()},
		{Name: OperationListChats, Description: "List your other chats, most recently active first, with their titles and files, to find work from an earlier chat. query matches chat titles and file paths. Read a chat's files by passing its id as chat to list_directory, read_file or search_files, and bring one into this chat with copy_from_chat.", InputSchema: listChatsInput()},
		{Name: OperationCopyFromChat, Description: "Copy a file, or a directory and everything in it, from another of your chats into this chat's files, so you can present, edit or build on it here.", InputSchema: copyInput()},
	}
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

func relativePathProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "maxLength": 4096, "pattern": `^(?:\.|[^/][^\x00]*)?$`, "default": "."}
}

func chatProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "The id of another of your chats, from list_chats. Leave out for this chat."}
}

func pathInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path"}, "properties": map[string]interface{}{"path": relativePathProperty(), "maxBytes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1048576, "default": 262144}, ChatArgument: chatProperty()}}
}

func listInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"path": relativePathProperty(), "depth": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 8, "default": 2}, "maxEntries": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 2000, "default": 500}, ChatArgument: chatProperty()}}
}

func searchInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"query"}, "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 4096}, "path": relativePathProperty(), "glob": map[string]interface{}{"type": "string", "maxLength": 512}, "regex": map[string]interface{}{"type": "boolean", "default": false}, "maxResults": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1000, "default": 200}, ChatArgument: chatProperty()}}
}

func writeInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "content"}, "properties": map[string]interface{}{"path": relativePathProperty(), "content": map[string]interface{}{"type": "string", "maxLength": 1048576}, "createParents": map[string]interface{}{"type": "boolean", "default": true}}}
}

func editInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "oldText", "newText"}, "properties": map[string]interface{}{
		"path":       relativePathProperty(),
		"oldText":    map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1048576},
		"newText":    map[string]interface{}{"type": "string", "maxLength": 1048576},
		"replaceAll": map[string]interface{}{"type": "boolean", "default": false},
	}}
}

func pathOnlyInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path"}, "properties": map[string]interface{}{"path": relativePathProperty()}}
}

func moveInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"path", "destination"}, "properties": map[string]interface{}{"path": relativePathProperty(), "destination": relativePathProperty()}}
}

func listChatsInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
		"query": map[string]interface{}{"type": "string", "maxLength": 200, "description": "Words to match in chat titles and file paths."},
		"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 50, "default": 20},
	}}
}

func copyInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{ChatArgument, "path"}, "properties": map[string]interface{}{
		ChatArgument:  chatProperty(),
		"path":        relativePathProperty(),
		"destination": map[string]interface{}{"type": "string", "maxLength": 4096, "description": "Where to put it in this chat's files. Defaults to the same path."},
	}}
}

func shellInput() map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"command"}, "properties": map[string]interface{}{
		"command":          map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 20000, "description": "A bash command, such as python3 make_chart.py or pandoc report.md -o docs/report.docx."},
		"workingDirectory": relativePathProperty(),
		"timeoutSeconds":   map[string]interface{}{"type": "integer", "minimum": 1, "maximum": ChatCommandSeconds, "default": 120},
	}}
}

// ChatSandboxDescription tells a model what a chat's sandbox can do, so it
// reaches for it when a task needs it.
const ChatSandboxDescription = "Run a bash command in this chat's sandbox, a Debian machine with internet access. The chat's files are the working directory: files a command creates, changes or deletes are saved to the chat's files, except installed packages and caches (node_modules, .venv, __pycache__, .cache). " +
	"Installed: Python 3 with numpy, pandas, matplotlib, seaborn, duckdb, openpyxl, xlsxwriter, python-docx, python-pptx, pypdf, pdfplumber, reportlab, weasyprint, beautifulsoup4, lxml, Pillow and requests; Node.js with npm and TypeScript (tsc); pandoc; ffmpeg; ImageMagick; poppler-utils (pdftotext, pdftoppm); qpdf; graphviz (dot); tesseract OCR (English); sqlite3; git, curl and jq. Install anything else, such as scipy, with pip install or npm install. " +
	"Use it to: read scanned PDFs and photos of text (pdftoppm, then tesseract); query large CSV or Excel files (duckdb); make charts (matplotlib or seaborn, saved as images); make Word, PowerPoint or Excel files (python-docx, python-pptx, openpyxl); turn HTML or Markdown into PDF (weasyprint, pandoc); merge, split or extract PDFs (qpdf, pdfplumber); convert or trim audio, video and images (ffmpeg, ImageMagick); draw diagrams (graphviz); type-check an app before save_app (tsc --noEmit). " +
	"Output is the last 64 KB of stdout and stderr; a non-zero exitStatus means the command failed, so read stderr and fix it."
