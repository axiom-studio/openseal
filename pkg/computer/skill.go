// Package computer defines openseal.computer, the bundled skill whose actions
// run on a person's own computer through the helloseal bridge
// (pkg/computer/bridge). The host binds it per run, only for an Agent run
// whose acting person owns a computer, and queues each call for that
// computer's bridge.
package computer

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Any change to an action's text, schema or risk must bump SkillVersion:
// bundled definitions are immutable per version.
const (
	SkillID      = "openseal.computer"
	SkillVersion = "1.0.0"

	ListComputers = "list_computers"
	RunCommand    = bridge.OpRunCommand
	ReadFile      = bridge.OpReadFile
	WriteFile     = bridge.OpWriteFile
	ListFiles     = bridge.OpListFiles
)

const (
	// An action's timeout is its op's timeout plus DeadlineSlack twice: once
	// for the deadline the host waits until, once more so the host's own
	// expiry, with its readable timeout error, always comes before the
	// kernel's.
	//
	// RunCommandTimeout covers the longest command (MaxCommandSeconds).
	RunCommandTimeout = bridge.MaxCommandSeconds*time.Second + 2*DeadlineSlack
	// FileOpTimeout is how long the bridge has for a file action, and
	// FileTimeout the action's whole timeout.
	FileOpTimeout = 30 * time.Second
	FileTimeout   = FileOpTimeout + 2*DeadlineSlack
	// ListComputersTimeout bounds list_computers, which the host answers.
	ListComputersTimeout = 30 * time.Second
	// DeadlineSlack is added to an op's own timeout to get the deadline the
	// host waits for a result until.
	DeadlineSlack = 30 * time.Second
)

// Action inputs and outputs. The bridge's ops share them with the wire
// protocol.
type (
	RunCommandInput  = bridge.RunCommandInput
	ReadFileInput    = bridge.ReadFileInput
	WriteFileInput   = bridge.WriteFileInput
	ListFilesInput   = bridge.ListFilesInput
	RunCommandOutput = bridge.RunCommandOutput
	ReadFileOutput   = bridge.ReadFileOutput
	WriteFileOutput  = bridge.WriteFileOutput
	ListFilesOutput  = bridge.ListFilesOutput
	FileEntry        = bridge.FileEntry
	Folder           = bridge.Folder
)

// ListComputersInput is list_computers' input; it takes nothing.
type ListComputersInput struct{}

type ListComputersOutput struct {
	Computers []ComputerInfo `json:"computers"`
}

// ComputerInfo is one of the person's computers as the model sees it.
type ComputerInfo struct {
	Name       string     `json:"name"`
	OS         string     `json:"os"`
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	Folders    []Folder   `json:"folders"`
	IsDefault  bool       `json:"isDefault"`
}

const descriptionPrefix = "On the person's own computer, not the secure VM. Use only when the person asks for something on their computer or names one; use the secure VM for everything else. "

func object(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func text(description string, min, max int) map[string]interface{} {
	return map[string]interface{}{"type": "string", "minLength": min, "maxLength": max, "description": description}
}

func integer(description string, min, max, def int) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "minimum": min, "maximum": max, "default": def, "description": description}
}

func computerProperty() map[string]interface{} {
	return text("The computer's name from list_computers. Required for run_command and write_file when the person has more than one computer. Reads without it use the person's default computer, or their only one online.", 1, 128)
}

func pathProperty(description string) map[string]interface{} {
	return text(description+" An absolute path or ~/…, inside a folder the person allowed.", 1, bridge.MaxPathBytes)
}

func stringProperty() map[string]interface{} { return map[string]interface{}{"type": "string"} }

func integerProperty() map[string]interface{} { return map[string]interface{}{"type": "integer"} }

func booleanProperty() map[string]interface{} { return map[string]interface{}{"type": "boolean"} }

func folderSchema() map[string]interface{} {
	return object([]interface{}{"path", "access"}, map[string]interface{}{
		"path":   stringProperty(),
		"access": map[string]interface{}{"type": "string", "enum": []interface{}{bridge.AccessRead, bridge.AccessWrite}},
	})
}

// SkillDefinition is the immutable contract of openseal.computer.
func SkillDefinition() *skill.Definition {
	read := func(name, description string, input, output map[string]interface{}, timeout time.Duration) skill.Action {
		return skill.Action{
			Name: name, Description: description, InputSchema: input, OutputSchema: output,
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			Timeout: capability.Duration(timeout), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		}
	}
	write := func(name, description string, input, output map[string]interface{}, timeout time.Duration) skill.Action {
		return skill.Action{
			Name: name, Description: description, InputSchema: input, OutputSchema: output,
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Timeout: capability.Duration(timeout), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		}
	}
	runCommand := write(RunCommand,
		descriptionPrefix+"Run a shell command there as the person's own user: /bin/sh on macOS and Linux, PowerShell on Windows. cwd must be a folder the person allowed with write access, and defaults to the first one. "+
			"Output is the last 64 KB of stdout and stderr; a non-zero exitCode means the command failed, so read stderr and fix it. A command still running at timeoutSeconds is stopped.",
		object([]interface{}{"command"}, map[string]interface{}{
			"computer":       computerProperty(),
			"command":        text("A shell command, such as git status or npm test.", 1, bridge.MaxCommandBytes),
			"cwd":            pathProperty("The directory to run it in."),
			"timeoutSeconds": integer("How long it may run.", 1, bridge.MaxCommandSeconds, bridge.DefaultCommandSeconds),
		}),
		object([]interface{}{"computer", "cwd", "exitCode", "stdout", "stderr", "truncated", "durationMs", "timedOut"}, map[string]interface{}{
			"computer": stringProperty(), "cwd": stringProperty(), "exitCode": integerProperty(),
			"stdout": stringProperty(), "stderr": stringProperty(), "truncated": booleanProperty(),
			"durationMs": integerProperty(), "timedOut": booleanProperty(),
		}),
		RunCommandTimeout)
	// Running the same command again, such as tests after a fix, is an
	// ordinary next step, not a duplicate.
	runCommand.Repeatable = true
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Computers",
		Description: "Run commands and read and write files on your own computer, in the folders you allow, through the HelloSeal bridge.",
		Icon:        "computer", Category: "integrations", Tags: []string{"computer", "laptop", "desktop", "terminal", "shell", "files", "local"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{
			ListComputers: read(ListComputers,
				"List the person's own computers connected to HelloSeal, with whether each is online, the folders it allows (read or write) and which is their default. Use it when the person asks for something on their computer and you need its name or folders.",
				object([]interface{}{}, map[string]interface{}{}),
				object([]interface{}{"computers"}, map[string]interface{}{
					"computers": map[string]interface{}{"type": "array", "items": object([]interface{}{"name", "os", "online", "folders", "isDefault"}, map[string]interface{}{
						"name": stringProperty(), "os": stringProperty(), "online": booleanProperty(),
						"lastSeenAt": map[string]interface{}{"type": "string", "format": "date-time"},
						"folders":    map[string]interface{}{"type": "array", "items": folderSchema()},
						"isDefault":  booleanProperty(),
					})},
				}),
				ListComputersTimeout),
			RunCommand: runCommand,
			ReadFile: read(ReadFile,
				descriptionPrefix+"Read a text file there, by lines: offset is the first line (1-based) and limit how many, 2000 by default. At most 256 KB is returned; truncated says there is more, so read on from a later offset.",
				object([]interface{}{"path"}, map[string]interface{}{
					"computer": computerProperty(),
					"path":     pathProperty("The file to read."),
					"offset":   integer("The first line to read, 1-based.", 1, 1<<30, 1),
					"limit":    integer("How many lines to read.", 1, 1<<20, bridge.DefaultReadLines),
				}),
				object([]interface{}{"computer", "path", "content", "totalLines", "truncated"}, map[string]interface{}{
					"computer": stringProperty(), "path": stringProperty(), "content": stringProperty(),
					"totalLines": integerProperty(), "truncated": booleanProperty(),
				}),
				FileTimeout),
			WriteFile: write(WriteFile,
				descriptionPrefix+"Create or replace a text file there, or add to its end when append is true. The folder must allow write access; missing parent directories are created.",
				object([]interface{}{"path", "content"}, map[string]interface{}{
					"computer": computerProperty(),
					"path":     pathProperty("The file to write."),
					"content":  map[string]interface{}{"type": "string", "maxLength": bridge.MaxWriteBytes},
					"append":   map[string]interface{}{"type": "boolean", "default": false, "description": "Add content to the end of the file instead of replacing it."},
				}),
				object([]interface{}{"computer", "path", "bytes"}, map[string]interface{}{
					"computer": stringProperty(), "path": stringProperty(), "bytes": integerProperty(),
				}),
				FileTimeout),
			ListFiles: read(ListFiles,
				descriptionPrefix+"List the files and directories below a directory there, depth levels deep (1 by default, at most 3), up to 2000 entries.",
				object([]interface{}{"path"}, map[string]interface{}{
					"computer": computerProperty(),
					"path":     pathProperty("The directory to list."),
					"depth":    integer("How many levels below path to list.", 1, bridge.MaxListDepth, bridge.DefaultListDepth),
				}),
				object([]interface{}{"computer", "path", "entries", "truncated"}, map[string]interface{}{
					"computer": stringProperty(), "path": stringProperty(), "truncated": booleanProperty(),
					"entries": map[string]interface{}{"type": "array", "items": object([]interface{}{"path", "type", "size"}, map[string]interface{}{
						"path": stringProperty(), "size": integerProperty(),
						"type": map[string]interface{}{"type": "string", "enum": []interface{}{bridge.EntryFile, bridge.EntryDir, bridge.EntrySymlink}},
					})},
				}),
				FileTimeout),
		},
	}
}
