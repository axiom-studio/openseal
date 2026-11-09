package workspace

// The results of the machine operations. Every machine returns exactly these
// fields; paths in them are in the machine's path style (chat paths, or a
// computer's absolute resolved paths). A chat adds "version", the chat's
// files snapshot the operation read or made, to its file results, and a
// command in a chat's sandbox adds what it changed in the chat's files
// (changedPaths, deletedPaths, version and notes about saving them); nothing
// else differs.
//
// A failure is not a result: it is an error whose text the model reads,
// built by the functions in files.go and command.go so both machines word it
// alike.
//
// The payload above, and the failure texts, are the contract. How they reach
// the model is the one deliberate difference, set by how each machine is
// reached rather than by the operations:
//   - A chat's operations are host tools. The model reads the result itself,
//     or a failure as {"success": false, "isError": true, "error": text}, and
//     a command with a non-zero exitStatus is marked as an error.
//   - A computer's are openseal.computer actions, governed like every
//     Skill's: the model reads {"evidenceRef", "output": result}, or a
//     failure as {"evidenceRef", "status": "failed", "error": text,
//     "errorCode"}, and a command that ran is a succeeded action whatever its
//     exitStatus. The evidenceRef is what approvals and reviews cite.
//
// Both tell the model a non-zero exitStatus means the command failed
// (CommandOutputSentence).

// Entry types of list_directory. A computer lists a symbolic link as a
// symlink without following it.
const (
	EntryFile      = "file"
	EntryDirectory = "directory"
	EntrySymlink   = "symlink"
)

// Entry is one file or directory list_directory lists. A file has its size
// and media type; a directory and a symlink have neither.
type Entry struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	Size      *int64 `json:"size,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}

// ListDirectoryResult lists the entries below a path in path order, a
// directory before what it holds, at most depth levels deep and maxEntries
// long; Truncated says more were left out.
type ListDirectoryResult struct {
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// ReadFileResult is a file's text: its first maxBytes bytes, cut back to a
// whole character (Truncated says the file holds more). A binary file reads
// as empty Content with a Note; a PDF or Word document reads as its
// extracted text with a Note.
type ReadFileResult struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
}

// SearchMatch is one matching line: its 1-based number and at most
// SearchLineBytes of its text.
type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchFilesResult holds the matching lines in path order, at most
// maxResults; Truncated says more matched.
type SearchFilesResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// ChangeResult answers write_file, edit_file, delete_file and move_file:
// ChangedPaths are the files written, removed or moved (both their old and
// new paths), sorted. Replacements is how many places edit_file changed.
type ChangeResult struct {
	Success      bool     `json:"success"`
	ChangedPaths []string `json:"changedPaths"`
	Replacements int      `json:"replacements,omitempty"`
}

// CommandResult answers run_command. Stdout and Stderr are the last
// CommandOutputBytes of each, as valid UTF-8; Truncated says either lost its
// start. A command still running at its timeout is stopped with its whole
// process tree: TimedOut, ExitStatus ExitTimedOut and a StoppedNote at the
// end of Stderr. One that could not start has ExitStatus ExitNotStarted.
type CommandResult struct {
	ExitStatus int    `json:"exitStatus"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timedOut"`
	DurationMs int64  `json:"durationMs"`
}

// Map is the entry as a chat's operations return it.
func (e Entry) Map() map[string]interface{} {
	entry := map[string]interface{}{"path": e.Path, "type": e.Type}
	if e.Size != nil {
		entry["size"] = *e.Size
	}
	if e.MediaType != "" {
		entry["mediaType"] = e.MediaType
	}
	return entry
}

// Map is the result as a chat's operations return it.
func (r ListDirectoryResult) Map() map[string]interface{} {
	entries := make([]map[string]interface{}, 0, len(r.Entries))
	for _, entry := range r.Entries {
		entries = append(entries, entry.Map())
	}
	return map[string]interface{}{"entries": entries, "truncated": r.Truncated}
}

// Map is the result as a chat's operations return it.
func (r ReadFileResult) Map() map[string]interface{} {
	result := map[string]interface{}{"path": r.Path, "mediaType": r.MediaType, "size": r.Size, "content": r.Content, "truncated": r.Truncated}
	if r.Note != "" {
		result["note"] = r.Note
	}
	return result
}

// Map is the result as a chat's operations return it.
func (r SearchFilesResult) Map() map[string]interface{} {
	matches := make([]map[string]interface{}, 0, len(r.Matches))
	for _, match := range r.Matches {
		matches = append(matches, map[string]interface{}{"path": match.Path, "line": match.Line, "text": match.Text})
	}
	return map[string]interface{}{"matches": matches, "truncated": r.Truncated}
}

// Map is the result as a chat's operations return it.
func (r ChangeResult) Map() map[string]interface{} {
	changed := r.ChangedPaths
	if changed == nil {
		changed = []string{}
	}
	result := map[string]interface{}{"success": r.Success, "changedPaths": changed}
	if r.Replacements > 0 {
		result["replacements"] = r.Replacements
	}
	return result
}

func machineOutputSchema(op string) map[string]interface{} {
	str := map[string]interface{}{"type": "string"}
	integer := map[string]interface{}{"type": "integer"}
	boolean := map[string]interface{}{"type": "boolean"}
	object := func(required []interface{}, properties map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"type": "object", "required": required, "properties": properties}
	}
	switch op {
	case OpListDirectory:
		return object([]interface{}{"entries", "truncated"}, map[string]interface{}{
			"entries": map[string]interface{}{"type": "array", "items": object([]interface{}{"path", "type"}, map[string]interface{}{
				"path": str, "size": integer, "mediaType": str,
				"type": map[string]interface{}{"type": "string", "enum": []interface{}{EntryFile, EntryDirectory, EntrySymlink}},
			})},
			"truncated": boolean,
		})
	case OpReadFile:
		return object([]interface{}{"path", "mediaType", "size", "content", "truncated"}, map[string]interface{}{
			"path": str, "mediaType": str, "size": integer, "content": str, "truncated": boolean, "note": str,
		})
	case OpSearchFiles:
		return object([]interface{}{"matches", "truncated"}, map[string]interface{}{
			"matches": map[string]interface{}{"type": "array", "items": object([]interface{}{"path", "line", "text"}, map[string]interface{}{
				"path": str, "line": integer, "text": str,
			})},
			"truncated": boolean,
		})
	case OpWriteFile, OpEditFile, OpDeleteFile, OpMoveFile:
		return object([]interface{}{"success", "changedPaths"}, map[string]interface{}{
			"success": boolean, "changedPaths": map[string]interface{}{"type": "array", "items": str}, "replacements": integer,
		})
	case OpRunCommand:
		return object([]interface{}{"exitStatus", "stdout", "stderr", "truncated", "timedOut", "durationMs"}, map[string]interface{}{
			"exitStatus": integer, "stdout": str, "stderr": str, "truncated": boolean, "timedOut": boolean, "durationMs": integer,
		})
	}
	return nil
}
