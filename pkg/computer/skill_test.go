package computer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func asMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestSkillDefinitionRegistersWithPolicies(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "openseal.computer" || d.Version != "1.0.0" || d.Name != "Computers" || d.Icon != "computer" || d.Category != "integrations" ||
		d.Transport.Kind != "tool" || d.Transport.Endpoint != SkillID || d.BindingTools != nil || len(d.Actions) != len(Actions()) || len(Actions()) != 9 {
		t.Fatalf("definition = %#v", d)
	}
	for _, name := range []string{ListComputers, ListDirectory, ReadFile, SearchFiles} {
		action := d.Actions[name]
		if !Reads(name) || action.SideEffect != skill.SideEffectRead || action.Risk != skill.RiskLevelRead || action.AlwaysReview() {
			t.Errorf("%s must be a read", name)
		}
	}
	for _, name := range []string{WriteFile, EditFile, DeleteFile, MoveFile, RunCommand} {
		action := d.Actions[name]
		if Reads(name) || action.SideEffect != skill.SideEffectWrite || action.Risk != skill.RiskLevelWrite || action.Idempotency != skill.IdempotencyRequired || action.AlwaysReview() {
			t.Errorf("%s must be a write requiring idempotency; review comes from the host policy", name)
		}
	}
	if d.Actions[RunCommand].Timeout.Duration() != 660*time.Second || !d.Actions[RunCommand].Repeatable {
		t.Fatalf("run_command = %#v", d.Actions[RunCommand])
	}
	// The host stops waiting at op timeout + DeadlineSlack; the kernel's
	// action timeout must come later so the host's timeout error wins.
	for _, name := range workspace.MachineOps {
		if timeout := d.Actions[name].Timeout.Duration(); name != RunCommand && (timeout != 90*time.Second || timeout <= FileOpTimeout+DeadlineSlack) {
			t.Errorf("%s timeout = %s", name, timeout)
		}
	}
	for name, action := range d.Actions {
		if action.Retry.MaxAttempts != 1 || action.InputSchema["additionalProperties"] != false || action.Timeout <= 0 {
			t.Errorf("%s must have a closed input, a timeout and no automatic retry", name)
		}
	}
}

// The computer's actions are the secure VM's machine operations: the same
// arguments, plus computer, minus chat.
func TestActionsAreTheSecureVMsOperations(t *testing.T) {
	d := SkillDefinition()
	chat := map[string]workspace.Operation{}
	for _, operation := range workspace.Operations(workspace.ChatAuthority("c")) {
		chat[strings.TrimPrefix(operation.Name, workspace.OperationPrefix)] = operation
	}
	for _, op := range workspace.MachineOps {
		action, ok := d.Actions[op]
		if !ok {
			t.Fatalf("missing %s", op)
		}
		if !strings.HasPrefix(action.Description, workspace.ComputerPreamble) {
			t.Errorf("%s must say it acts on the person's computer: %q", op, action.Description)
		}
		mine := action.InputSchema["properties"].(map[string]interface{})
		theirs := chat[op].InputSchema["properties"].(map[string]interface{})
		if _, ok := mine[ComputerArgument]; !ok {
			t.Errorf("%s has no computer argument", op)
		}
		for name := range theirs {
			if _, ok := mine[name]; !ok && name != workspace.ChatArgument {
				t.Errorf("%s lacks the secure VM's %s", op, name)
			}
		}
		for name := range mine {
			if _, ok := theirs[name]; !ok && name != ComputerArgument {
				t.Errorf("%s has %s, which the secure VM lacks", op, name)
			}
		}
		if fmt.Sprint(action.InputSchema["required"]) != fmt.Sprint(chat[op].InputSchema["required"]) {
			t.Errorf("%s requires %v, the secure VM %v", op, action.InputSchema["required"], chat[op].InputSchema["required"])
		}
	}
}

func TestInputsAndOutputsMatchSchemas(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	source := skill.DefinitionSourceIdentity(d)
	valid := map[string]map[string]interface{}{
		ListComputers: {},
		ListDirectory: {"computer": "laptop", "path": "~/code", "depth": 3},
		ReadFile:      {"path": "/home/k/notes.md", "maxBytes": 1000},
		SearchFiles:   {"query": "TODO", "path": "~/code", "glob": "*.go", "regex": false},
		WriteFile:     {"path": "~/notes.md", "content": ""},
		EditFile:      {"path": "~/notes.md", "oldText": "a", "newText": "b", "replaceAll": true},
		DeleteFile:    {"path": "~/notes.md"},
		MoveFile:      {"path": "~/a.md", "destination": "~/b.md"},
		RunCommand:    {"computer": "Studio Mac", "command": "git status", "workingDirectory": "~/code/app", "timeoutSeconds": 600},
	}
	for action, input := range valid {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, source, action, input); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
	invalid := map[string]map[string]interface{}{
		RunCommand:    {"command": "sleep 1000", "timeoutSeconds": 601},
		ListDirectory: {"path": "~", "depth": 9},
		ReadFile:      {"path": "~/a", "chat": "c2"},
		WriteFile:     {"path": "~/a"},
		MoveFile:      {"path": "~/a"},
	}
	for action, input := range invalid {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, source, action, input); err == nil {
			t.Errorf("%s accepted %#v", action, input)
		}
	}

	seen := time.Unix(1_800_000_000, 0).UTC()
	outputs := map[string]interface{}{
		ListComputers: ListComputersOutput{Computers: []ComputerInfo{
			{Name: "Studio Mac", OS: "darwin", Online: true, LastSeenAt: &seen, Folders: []Folder{{Path: "~/code", Access: "write"}}, IsDefault: true},
			{Name: "Old laptop", OS: "linux", Folders: []Folder{}},
		}},
		ListDirectory: workspace.ListDirectoryResult{Entries: []workspace.Entry{{Path: "/Users/k/src", Type: workspace.EntryDirectory}, {Path: "/Users/k/a.md", Type: workspace.EntryFile, Size: workspace.FileSize(1), MediaType: "text/markdown"}}},
		ReadFile:      workspace.ReadFileResult{Path: "/Users/k/a.md", MediaType: "text/markdown", Size: 1, Content: "x"},
		SearchFiles:   workspace.SearchFilesResult{Matches: []workspace.SearchMatch{{Path: "/Users/k/a.md", Line: 1, Text: "x"}}},
		WriteFile:     workspace.ChangeResult{Success: true, ChangedPaths: []string{"/Users/k/a.md"}},
		EditFile:      workspace.ChangeResult{Success: true, ChangedPaths: []string{"/Users/k/a.md"}, Replacements: 2},
		DeleteFile:    workspace.ChangeResult{Success: true, ChangedPaths: []string{"/Users/k/a.md"}},
		MoveFile:      workspace.ChangeResult{Success: true, ChangedPaths: []string{"/Users/k/a.md", "/Users/k/b.md"}},
		RunCommand:    workspace.CommandResult{ExitStatus: 1, Stderr: "boom", DurationMs: 12},
	}
	binding := &skill.Binding{ID: "computer"}
	for action, output := range outputs {
		bound := &skill.BoundAction{Definition: d, Binding: binding, Action: d.Actions[action]}
		if err := catalog.ValidateOutput(t.Context(), bound, asMap(t, output)); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
}

func TestComputerBindingsHoldAConnection(t *testing.T) {
	if !skill.BindingHoldsConnection(&skill.Binding{SkillID: SkillID}) {
		t.Fatalf("a %s binding must count as a connection, so Team members and connection-free activations leave it out", SkillID)
	}
}
