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
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Any change to an action's text, schema or risk must bump SkillVersion:
// bundled definitions are immutable per version. The machine operations'
// text and schemas come from pkg/workspace, so a change there is one too.
const (
	SkillID      = "openseal.computer"
	SkillVersion = "1.0.0"

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

	// ComputerArgument names the computer in every action's input.
	ComputerArgument = "computer"
)

// Actions are every action of openseal.computer.
func Actions() []string { return append([]string{ListComputers}, workspace.MachineOps...) }

// Reads reports whether an action only reads: list_computers and the
// machine reads. Every other action changes files or runs a command, and
// follows the computer's approval setting.
func Reads(action string) bool { return action == ListComputers || workspace.MachineOpReads(action) }

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
	// ListComputersTimeout bounds list_computers, which the host answers.
	ListComputersTimeout = 30 * time.Second
	// DeadlineSlack is added to an op's own timeout to get the deadline the
	// host waits for a result until.
	DeadlineSlack = 30 * time.Second
)

// OpTimeout is how long the bridge has for an op's call.
func OpTimeout(op string) time.Duration {
	if op == RunCommand {
		return workspace.ChatCommandSeconds * time.Second
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

func folderSchema() map[string]interface{} {
	return object([]interface{}{"path", "access"}, map[string]interface{}{
		"path":   stringProperty(),
		"access": map[string]interface{}{"type": "string", "enum": []interface{}{bridge.AccessRead, bridge.AccessWrite}},
	})
}

// SkillDefinition is the immutable contract of openseal.computer: the
// machine operations as workspace.ComputerTarget offers them, and
// list_computers.
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
		Description: "Work on your own computer like on the secure VM: list, read, search, write, edit, delete and move files and run commands, in the folders you allow, through the HelloSeal bridge.",
		Icon:        "computer", Category: "integrations", Tags: []string{"computer", "laptop", "desktop", "terminal", "shell", "files", "local"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions:   actions,
	}
}
