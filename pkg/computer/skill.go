// Package computer defines openseal.computer, the bundled skill whose actions
// run on a person's own computer through the helloseal bridge
// (pkg/computer/bridge). The host binds it per run, only for an Agent run
// whose acting person owns a computer, and queues each call for that
// computer's bridge.
package computer

import (
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Any change to an action's text, schema or risk must bump SkillVersion:
// bundled definitions are immutable per version. The machine operations'
// text and schemas come from pkg/workspace, so a change there is one too.
const (
	SkillID      = "openseal.computer"
	SkillVersion = "1.1.0"

	// ListComputers is the only computer-only action; the host answers it.
	ListComputers = "list_computers"

	// The machine operations, the same as a chat's secure VM offers
	// (pkg/workspace machine.go). The bridge runs them.
	ListDirectory = workspace.OpListDirectory
	ReadFile      = workspace.OpReadFile
	SearchFiles   = workspace.OpSearchFiles
	WriteFile     = workspace.OpWriteFile
	EditFile      = workspace.OpEditFile
	DeleteFile    = workspace.OpDeleteFile
	MoveFile      = workspace.OpMoveFile
	RunCommand    = workspace.OpRunCommand

	// Present hands files from the computer into the chat. It is no machine
	// operation (a chat's files are already in the chat); the bridge runs it
	// as bridge.OpPresent.
	Present = bridge.OpPresent

	// ComputerArgument names the computer in every action's input.
	ComputerArgument = "computer"
)

// Actions are every action of openseal.computer.
func Actions() []string {
	return append(append([]string{ListComputers}, workspace.MachineOps...), Present)
}

// Reads reports whether an action only reads the computer: list_computers,
// the machine reads, and present, which copies files into the chat but
// changes nothing on the computer. Every other action changes files or runs
// a command, and follows the computer's approval setting.
func Reads(action string) bool {
	return action == ListComputers || action == Present || workspace.MachineOpReads(action)
}

const (
	// An action's timeout is its op's timeout plus DeadlineSlack twice: once
	// for the deadline the host waits until, once more so the host's own
	// expiry, with its readable timeout error, always comes before the
	// kernel's.
	//
	// RunCommandTimeout covers the longest command.
	RunCommandTimeout = workspace.ChatCommandSeconds*time.Second + 2*DeadlineSlack
	// FileOpTimeout is how long the bridge has for a file action, and
	// FileTimeout the action's whole timeout.
	FileOpTimeout = 30 * time.Second
	FileTimeout   = FileOpTimeout + 2*DeadlineSlack
	// PresentOpTimeout is how long the bridge has to send a present call's
	// files: ten of bridge.MaxPresentBytes at a few megabits a second.
	PresentOpTimeout = 10 * time.Minute
	// ListComputersTimeout bounds list_computers, which the host answers.
	ListComputersTimeout = 30 * time.Second
	// DeadlineSlack is added to an op's own timeout to get the deadline the
	// host waits for a result until.
	DeadlineSlack = 30 * time.Second
)

// OpTimeout is how long the bridge has for an op's call.
func OpTimeout(op string) time.Duration {
	switch op {
	case RunCommand:
		return workspace.ChatCommandSeconds * time.Second
	case Present:
		return PresentOpTimeout
	}
	return FileOpTimeout
}

// Folder is one folder the person allowed on a computer.
type Folder = bridge.Folder

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

func object(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func stringProperty() map[string]interface{} { return map[string]interface{}{"type": "string"} }

func booleanProperty() map[string]interface{} { return map[string]interface{}{"type": "boolean"} }

func computerProperty() map[string]interface{} {
	return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128,
		"description": "The computer's name from list_computers. Required for changes and commands when the person has more than one computer. Reads without it use the person's default computer, or their only one online."}
}

// artifactRefs is the files present shows, as openseal.builtin's present
// returns them, so the kernel attaches them to the answer the same way.
func artifactRefs() map[string]interface{} {
	return map[string]interface{}{"type": "array", "minItems": 1, "items": object([]interface{}{"id", "version"}, map[string]interface{}{
		"id": stringProperty(), "version": map[string]interface{}{"type": "integer", "minimum": 1},
		// The chat path the file now has, for handing it over by path.
		"path": stringProperty(),
	})}
}

func presentAction() skill.Action {
	return skill.Action{
		Name: Present,
		Description: "Show the person a file from their own computer in this chat: a PDF, image, deck, spreadsheet or any other file, " +
			"as a card they can open or download. It copies each file into the chat's files under computers/<computer>/ and returns artifactRefs " +
			"with that chat path; name the path (or id) in your final answer's files to hand the file over. Use it instead of reading a file " +
			"that is not text, or re-creating it on the secure VM. Paths are full paths on that computer inside its allowed folders, " +
			fmt.Sprintf("at most %d files of %d MB each.", bridge.MaxPresentFiles, bridge.MaxPresentBytes>>20),
		InputSchema: object([]interface{}{"paths"}, map[string]interface{}{
			ComputerArgument: computerProperty(),
			"paths": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": bridge.MaxPresentFiles, "uniqueItems": true,
				"items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": workspace.MaximumPathBytes,
					"description": "A file on the computer, such as ~/Documents/report.pdf."}},
		}),
		OutputSchema: object([]interface{}{"artifactRefs"}, map[string]interface{}{"artifactRefs": artifactRefs()}),
		SideEffect:   skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		Timeout: capability.Duration(PresentOpTimeout + 2*DeadlineSlack), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}

func folderSchema() map[string]interface{} {
	return object([]interface{}{"path", "access"}, map[string]interface{}{
		"path":   stringProperty(),
		"access": map[string]interface{}{"type": "string", "enum": []interface{}{bridge.AccessRead, bridge.AccessWrite}},
	})
}

// SkillDefinition is the immutable contract of openseal.computer: the
// machine operations as workspace.ComputerTarget offers them,
// list_computers and present.
func SkillDefinition() *skill.Definition {
	actions := map[string]skill.Action{
		ListComputers: {
			Name:        ListComputers,
			Description: "List the person's own computers connected to HelloSeal, with whether each is online, the folders it allows (read or write) and which is their default. Use it when the person asks for something on their computer and you need its name or folders.",
			InputSchema: object([]interface{}{}, map[string]interface{}{}),
			OutputSchema: object([]interface{}{"computers"}, map[string]interface{}{
				"computers": map[string]interface{}{"type": "array", "items": object([]interface{}{"name", "os", "online", "folders", "isDefault"}, map[string]interface{}{
					"name": stringProperty(), "os": stringProperty(), "online": booleanProperty(),
					"lastSeenAt": map[string]interface{}{"type": "string", "format": "date-time"},
					"folders":    map[string]interface{}{"type": "array", "items": folderSchema()},
					"isDefault":  booleanProperty(),
				})},
			}),
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			Timeout: capability.Duration(ListComputersTimeout), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		},
		Present: presentAction(),
	}
	for _, operation := range workspace.MachineOperations(workspace.ComputerTarget(ComputerArgument, computerProperty())) {
		action := skill.Action{
			Name: operation.Op, Description: operation.Description, InputSchema: operation.InputSchema, OutputSchema: operation.OutputSchema,
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			Timeout: capability.Duration(OpTimeout(operation.Op) + 2*DeadlineSlack), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		}
		if !operation.Reads {
			action.SideEffect, action.Risk, action.Idempotency = skill.SideEffectWrite, skill.RiskLevelWrite, skill.IdempotencyRequired
		}
		// The same change again is a next step, as on the secure VM.
		action.Repeatable = operation.Repeatable
		actions[operation.Op] = action
	}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Computers",
		Description: "Work on your own computer like on the secure VM: list, read, search, write, edit, delete and move files and run commands, in the folders you allow, through the HelloSeal bridge, and show its files in the chat.",
		Icon:        "computer", Category: "integrations", Tags: []string{"computer", "laptop", "desktop", "terminal", "shell", "files", "local"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions:   actions,
	}
}
