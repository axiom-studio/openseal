package workspace

// A chat's host tools: the machine operations (machine.go) with the
// "workspace." prefix, and the chat-only ones.
const (
	OperationPrefix = "workspace."

	OperationListDirectory = OperationPrefix + OpListDirectory
	OperationReadFile      = OperationPrefix + OpReadFile
	OperationSearchFiles   = OperationPrefix + OpSearchFiles
	OperationBatchRead     = OperationPrefix + "batch_read"
	OperationWriteFile     = OperationPrefix + OpWriteFile
	OperationEditFile      = OperationPrefix + OpEditFile
	OperationDeleteFile    = OperationPrefix + OpDeleteFile
	OperationMoveFile      = OperationPrefix + OpMoveFile
	OperationRunCommand    = OperationPrefix + OpRunCommand
	OperationListChats     = OperationPrefix + "list_chats"
	OperationCopyFromChat  = OperationPrefix + "copy_from_chat"
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
	machine := map[string]MachineOperation{}
	for _, operation := range MachineOperations(ChatTarget()) {
		machine[operation.Op] = operation
	}
	tool := func(op string) Operation {
		return Operation{Name: OperationPrefix + op, Description: machine[op].Description, InputSchema: machine[op].InputSchema}
	}
	return []Operation{
		tool(OpListDirectory),
		tool(OpReadFile),
		tool(OpSearchFiles),
		{Name: OperationBatchRead, Description: "Run two to eight independent list, read, or search requests concurrently and return their results in request order. Use only when no request depends on another result.", InputSchema: batchReadInput()},
		tool(OpWriteFile),
		tool(OpEditFile),
		tool(OpDeleteFile),
		tool(OpMoveFile),
		tool(OpRunCommand),
		{Name: OperationListChats, Description: "List your other chats, most recently active first, with their titles and files, to find work from an earlier chat. query matches chat titles and file paths. Read a chat's files by passing its id as chat to list_directory, read_file or search_files, and bring one into this chat with copy_from_chat.", InputSchema: listChatsInput()},
		{Name: OperationCopyFromChat, Description: "Copy a file, or a directory and everything in it, from another of your chats into this chat's files, so you can present, edit or build on it here.", InputSchema: copyInput()},
	}
}

func batchReadInput() map[string]interface{} {
	branches := []interface{}{}
	for _, item := range []struct {
		name   string
		schema map[string]interface{}
	}{{OperationListDirectory, ChatTarget().input(OpListDirectory)}, {OperationReadFile, ChatTarget().input(OpReadFile)}, {OperationSearchFiles, ChatTarget().input(OpSearchFiles)}} {
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

// ChatSandboxDescription tells a model what a chat's sandbox can do, so it
// reaches for it when a task needs it.
const ChatSandboxDescription = "Run a bash command in this chat's sandbox, a Debian machine with internet access. The chat's files are the working directory: files a command creates, changes or deletes are saved to the chat's files, except installed packages and caches (node_modules, .venv, __pycache__, .cache). " +
	"Installed: Python 3 with numpy, pandas, matplotlib, seaborn, duckdb, openpyxl, xlsxwriter, python-docx, python-pptx, pypdf, pdfplumber, reportlab, weasyprint, beautifulsoup4, lxml, Pillow and requests; Node.js with npm and TypeScript (tsc); pandoc; ffmpeg; ImageMagick; poppler-utils (pdftotext, pdftoppm); qpdf; LibreOffice Calc (soffice --headless) to convert or recalculate spreadsheets; graphviz (dot); tesseract OCR (English); sqlite3; git, curl and jq. Install anything else, such as scipy, with pip install or npm install. " +
	"Use it to: read scanned PDFs and photos of text (pdftoppm, then tesseract); query large CSV or Excel files (duckdb); make charts (matplotlib or seaborn, saved as images); make Word, PowerPoint or Excel files (python-docx, python-pptx, openpyxl); turn HTML or Markdown into PDF (weasyprint, pandoc); merge, split or extract PDFs (qpdf, pdfplumber); convert or trim audio, video and images (ffmpeg, ImageMagick); draw diagrams (graphviz); type-check an app before save_app (tsc --noEmit). " +
	CommandOutputSentence +
	"With GitHub connected, git clones, fetches and pulls any repository that connection can read, private ones too, from https://git.seal/github.com/<owner>/<repo>.git, with no token. Clone into ~/repos, outside the chat's files; clones stay for the sandbox's life. Add --filter=blob:none for a big repository (a clone transfers at most 2 GB). To answer questions about code, clone once and search with git log, git grep and grep rather than reading files one call at a time. " +
	"git push works only to a branch other than the repository's default branch, and each push waits for the user's approval; then open a pull request with the GitHub actions. " +
	"Inside a command, seal tools lists the tools you can call and seal call <tool> --json '{...}' calls one and prints its JSON result; files a tool returns are written under tool-output/ before it prints, and --file field=path passes a file to a field that takes file content. " +
	"Work that repeats with new data each time can be saved as a script in the chat's files: once it runs correctly here, a routine can run that exact script on every occurrence."
