package workspace

// A machine is somewhere an Agent works with files and runs commands: a
// chat's secure VM (its chat's files and sandbox) or a person's own computer
// (the openseal.computer skill, through the helloseal bridge). Both offer the
// same machine operations, defined once here: the same names, inputs,
// results and errors. Only the target differs, as parameters of
// MachineOperations: the wording, the path style (chat paths relative to the
// chat's files, or absolute and ~/ paths inside a computer's allowed
// folders), the chat-only chat argument on reads, and the computer-only
// computer argument. workspacetest.Conformance holds every implementation to
// this contract.
//
// Chat-only operations (batch_read, list_chats, copy_from_chat) and the
// computer-only list_computers are not machine operations.

// Machine operation names. A chat's host tools carry them with the
// "workspace." prefix (OperationListDirectory and the rest); the
// openseal.computer actions and the bridge's calls carry them bare.
const (
	OpListDirectory = "list_directory"
	OpReadFile      = "read_file"
	OpSearchFiles   = "search_files"
	OpWriteFile     = "write_file"
	OpEditFile      = "edit_file"
	OpDeleteFile    = "delete_file"
	OpMoveFile      = "move_file"
	OpRunCommand    = "run_command"
)

// MachineOps are the machine operations in the order tools list them.
var MachineOps = []string{OpListDirectory, OpReadFile, OpSearchFiles, OpWriteFile, OpEditFile, OpDeleteFile, OpMoveFile, OpRunCommand}

// MachineOpReads reports whether op only reads. The others change files or
// run a command.
func MachineOpReads(op string) bool {
	return op == OpListDirectory || op == OpReadFile || op == OpSearchFiles
}

// MachineOp reports whether op is a machine operation.
func MachineOp(op string) bool {
	for _, known := range MachineOps {
		if op == known {
			return true
		}
	}
	return false
}

// Limits of the machine operations, the same on every machine.
const (
	// DefaultReadBytes and MaximumReadBytes bound read_file's maxBytes.
	DefaultReadBytes = 256 << 10
	MaximumReadBytes = 1 << 20
	// DefaultListDepth, MaximumListDepth, DefaultListEntries and
	// MaximumListEntries bound list_directory.
	DefaultListDepth   = 2
	MaximumListDepth   = 8
	DefaultListEntries = 500
	MaximumListEntries = 2000
	// DefaultSearchResults and MaximumSearchResults bound search_files;
	// SearchFileBytes is the largest file it reads and SearchLineBytes the
	// most of a matching line it returns.
	DefaultSearchResults = 200
	MaximumSearchResults = 1000
	SearchFileBytes      = 4 << 20
	SearchLineBytes      = 2000
	// MaximumWriteContent bounds write_file's content, edit_file's texts
	// (schema) and MaximumFileBytes a file write_file or edit_file leaves.
	MaximumWriteContent = 1 << 20
	MaximumFileBytes    = 4 << 20
	// MaximumPathBytes bounds a path.
	MaximumPathBytes = 4096
	// MaximumCommandBytes bounds a command, DefaultCommandSeconds is its
	// timeout when none is given, and ChatCommandSeconds (authority.go) the
	// longest it may run.
	MaximumCommandBytes   = 20000
	DefaultCommandSeconds = 120
	// CommandOutputBytes is how much of stdout and of stderr a command
	// returns: the last bytes of each.
	CommandOutputBytes = 64 << 10
)

// Exit statuses of a command that did not exit by itself.
const (
	ExitTimedOut   = 124
	ExitNotStarted = 127
)

// MachineTarget is what MachineOperations describes: a chat's secure VM or a
// person's computer.
type MachineTarget struct {
	// Computer selects a person's computer: its wording and absolute or ~/
	// paths inside allowed folders. Otherwise it is a chat's secure VM, with
	// paths relative to the chat's files.
	Computer bool
	// ReadArguments are added to the inputs of the reads (list_directory,
	// read_file, search_files); Arguments to every input.
	ReadArguments map[string]map[string]interface{}
	Arguments     map[string]map[string]interface{}
}

// ChatTarget is a chat's secure VM, whose reads can look at another chat.
func ChatTarget() MachineTarget {
	return MachineTarget{ReadArguments: map[string]map[string]interface{}{ChatArgument: chatProperty()}}
}

// ComputerTarget is a person's computer; computer is the property that names
// one, added to every input.
func ComputerTarget(argument string, computer map[string]interface{}) MachineTarget {
	return MachineTarget{Computer: true, Arguments: map[string]map[string]interface{}{argument: computer}}
}

// MachineOperation is one machine operation as a target offers it.
type MachineOperation struct {
	Op    string
	Reads bool
	// Repeatable says doing the same change again is an ordinary next step,
	// never a duplicate to suppress: every change, as a chat's host tools
	// are. Deleting a build folder again after a rebuild, restoring a file's
	// earlier content or running the tests again after a fix must happen.
	Repeatable   bool
	Description  string
	InputSchema  map[string]interface{}
	OutputSchema map[string]interface{}
}

// ComputerPreamble starts every computer operation's description.
const ComputerPreamble = "On the person's own computer, not the secure VM. Use only when the person asks for something on their computer or names one; use the secure VM for everything else. "

// CommandOutputSentence says how to read a command's result, on any machine.
const CommandOutputSentence = "Output is the last 64 KB of stdout and stderr; a non-zero exitStatus means the command failed, so read stderr and fix it. "

// ComputerCommandDescription is the core of run_command on a person's
// computer.
const ComputerCommandDescription = "Run a shell command on the person's computer as their own user: /bin/sh on macOS and Linux, PowerShell on Windows. workingDirectory must be in a folder the person allowed with write access, and defaults to the first one. " +
	CommandOutputSentence + "A command still running at timeoutSeconds is stopped."

// MachineOperations are the machine operations as target offers them, in
// MachineOps order.
func MachineOperations(target MachineTarget) []MachineOperation {
	operations := make([]MachineOperation, 0, len(MachineOps))
	for _, op := range MachineOps {
		operations = append(operations, MachineOperation{
			Op: op, Reads: MachineOpReads(op), Repeatable: !MachineOpReads(op),
			Description:  target.description(op),
			InputSchema:  target.input(op),
			OutputSchema: machineOutputSchema(op),
		})
	}
	return operations
}

func (t MachineTarget) description(op string) string {
	if !t.Computer {
		switch op {
		case OpListDirectory:
			return "List the files below a path in this chat's files, or in another chat's when chat is set."
		case OpReadFile:
			return "Read a text file from this chat's files, or from another chat's when chat is set."
		case OpSearchFiles:
			return "Search the text of this chat's files, or another chat's when chat is set, for text or a regular expression."
		case OpWriteFile:
			return "Create or replace a text file in this chat's files."
		case OpEditFile:
			return "Replace exact text in a file: " + editRule
		case OpDeleteFile:
			return "Delete a file, or a directory and everything in it."
		case OpMoveFile:
			return "Move or rename a file or directory."
		case OpRunCommand:
			return ChatSandboxDescription
		}
		return ""
	}
	switch op {
	case OpListDirectory:
		return ComputerPreamble + "List the files below a path on the person's computer."
	case OpReadFile:
		return ComputerPreamble + "Read a text file on the person's computer."
	case OpSearchFiles:
		return ComputerPreamble + "Search the text of files below a path on the person's computer for text or a regular expression."
	case OpWriteFile:
		return ComputerPreamble + "Create or replace a text file on the person's computer, in a folder the person allowed with write access."
	case OpEditFile:
		return ComputerPreamble + "Replace exact text in a file on the person's computer: " + editRule
	case OpDeleteFile:
		return ComputerPreamble + "Delete a file, or a directory and everything in it, on the person's computer."
	case OpMoveFile:
		return ComputerPreamble + "Move or rename a file or directory on the person's computer. Both paths must be in folders the person allowed with write access."
	case OpRunCommand:
		return ComputerPreamble + ComputerCommandDescription
	}
	return ""
}

const editRule = "oldText must match the file exactly, including whitespace, and appear once unless replaceAll is set. Read the file first."

func (t MachineTarget) path() map[string]interface{} {
	if !t.Computer {
		return relativePathProperty()
	}
	return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumPathBytes,
		"description": "An absolute path or ~/…, inside a folder the person allowed. A relative path, or none, starts in the first folder that allows writes, else the first folder; listing or searching without a path covers only that folder."}
}

func (t MachineTarget) input(op string) map[string]interface{} {
	var required []interface{}
	properties := map[string]interface{}{}
	switch op {
	case OpListDirectory:
		properties["path"] = t.path()
		properties["depth"] = map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumListDepth, "default": DefaultListDepth}
		properties["maxEntries"] = map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumListEntries, "default": DefaultListEntries}
	case OpReadFile:
		required = []interface{}{"path"}
		properties["path"] = t.path()
		properties["maxBytes"] = map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumReadBytes, "default": DefaultReadBytes}
	case OpSearchFiles:
		required = []interface{}{"query"}
		properties["query"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 4096}
		properties["path"] = t.path()
		properties["glob"] = map[string]interface{}{"type": "string", "maxLength": 512}
		properties["regex"] = map[string]interface{}{"type": "boolean", "default": false}
		properties["maxResults"] = map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumSearchResults, "default": DefaultSearchResults}
	case OpWriteFile:
		required = []interface{}{"path", "content"}
		properties["path"] = t.path()
		properties["content"] = map[string]interface{}{"type": "string", "maxLength": MaximumWriteContent}
	case OpEditFile:
		required = []interface{}{"path", "oldText", "newText"}
		properties["path"] = t.path()
		properties["oldText"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumWriteContent}
		properties["newText"] = map[string]interface{}{"type": "string", "maxLength": MaximumWriteContent}
		properties["replaceAll"] = map[string]interface{}{"type": "boolean", "default": false}
	case OpDeleteFile:
		required = []interface{}{"path"}
		properties["path"] = t.path()
	case OpMoveFile:
		required = []interface{}{"path", "destination"}
		properties["path"] = t.path()
		properties["destination"] = t.path()
	case OpRunCommand:
		required = []interface{}{"command"}
		command := "A bash command, such as python3 make_chart.py or pandoc report.md -o docs/report.docx."
		if t.Computer {
			command = "A shell command, such as git status or npm test."
		}
		properties["command"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumCommandBytes, "description": command}
		properties["workingDirectory"] = t.path()
		properties["timeoutSeconds"] = map[string]interface{}{"type": "integer", "minimum": 1, "maximum": ChatCommandSeconds, "default": DefaultCommandSeconds}
	}
	if MachineOpReads(op) {
		for name, property := range t.ReadArguments {
			properties[name] = property
		}
	}
	for name, property := range t.Arguments {
		properties[name] = property
	}
	schema := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties}
	if required != nil {
		schema["required"] = required
	}
	return schema
}

func relativePathProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "maxLength": MaximumPathBytes, "pattern": `^(?:\.|[^/][^\x00]*)?$`, "default": "."}
}

func chatProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "The id of another of your chats, from list_chats. Leave out for this chat."}
}
