package computer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
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
		d.Transport.Kind != "tool" || d.Transport.Endpoint != SkillID || d.BindingTools != nil || len(d.Actions) != 5 {
		t.Fatalf("definition = %#v", d)
	}
	for _, name := range []string{ListComputers, ReadFile, ListFiles} {
		action := d.Actions[name]
		if action.SideEffect != skill.SideEffectRead || action.Risk != skill.RiskLevelRead || action.AlwaysReview() {
			t.Errorf("%s must be a read", name)
		}
	}
	for _, name := range []string{RunCommand, WriteFile} {
		action := d.Actions[name]
		if action.SideEffect != skill.SideEffectWrite || action.Risk != skill.RiskLevelWrite || action.Idempotency != skill.IdempotencyRequired || action.AlwaysReview() {
			t.Errorf("%s must be a write requiring idempotency; review comes from the host policy", name)
		}
	}
	if d.Actions[RunCommand].Timeout.Duration() != 660*time.Second {
		t.Fatalf("run_command timeout = %s", d.Actions[RunCommand].Timeout.Duration())
	}
	// The host stops waiting at op timeout + DeadlineSlack; the kernel's
	// action timeout must come later so the host's timeout error wins.
	for _, name := range []string{ReadFile, WriteFile, ListFiles} {
		if timeout := d.Actions[name].Timeout.Duration(); timeout != 90*time.Second || timeout <= FileOpTimeout+DeadlineSlack {
			t.Errorf("%s timeout = %s", name, timeout)
		}
	}
	for name, action := range d.Actions {
		if action.Retry.MaxAttempts != 1 || action.InputSchema["additionalProperties"] != false || action.Timeout <= 0 {
			t.Errorf("%s must have a closed input, a timeout and no automatic retry", name)
		}
	}
}

func TestTypedInputsMatchSchemas(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	source := skill.DefinitionSourceIdentity(d)
	valid := map[string]interface{}{
		ListComputers: ListComputersInput{},
		RunCommand:    RunCommandInput{Computer: "Studio Mac", Command: "git status", Cwd: "~/code/app", TimeoutSeconds: 600},
		ReadFile:      ReadFileInput{Path: "/home/k/notes.md", Offset: 10, Limit: 50},
		WriteFile:     WriteFileInput{Path: "~/notes.md", Content: "", Append: true},
		ListFiles:     ListFilesInput{Computer: "laptop", Path: "~/code", Depth: 3},
	}
	for action, input := range valid {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, source, action, asMap(t, input)); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
	invalid := map[string]map[string]interface{}{
		RunCommand: asMap(t, RunCommandInput{Command: "sleep 1000", TimeoutSeconds: 601}),
		ListFiles:  asMap(t, ListFilesInput{Path: "~", Depth: 4}),
		ReadFile:   {"path": "~/a", "folder": "~"},
		WriteFile:  {"path": "~/a"},
	}
	for action, input := range invalid {
		if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, source, action, input); err == nil {
			t.Errorf("%s accepted %#v", action, input)
		}
	}
}

func TestTypedOutputsMatchSchemas(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	seen := time.Unix(1_800_000_000, 0).UTC()
	outputs := map[string]interface{}{
		ListComputers: ListComputersOutput{Computers: []ComputerInfo{
			{Name: "Studio Mac", OS: "darwin", Online: true, LastSeenAt: &seen, Folders: []Folder{{Path: "~/code", Access: "write"}}, IsDefault: true},
			{Name: "Old laptop", OS: "linux", Folders: []Folder{}},
		}},
		RunCommand: RunCommandOutput{Computer: "Studio Mac", Cwd: "/Users/k/code", ExitCode: 1, Stderr: "boom", DurationMs: 12},
		ReadFile:   ReadFileOutput{Computer: "Studio Mac", Path: "/Users/k/a.md", Content: "x", TotalLines: 1},
		WriteFile:  WriteFileOutput{Computer: "Studio Mac", Path: "/Users/k/a.md", Bytes: 1},
		ListFiles:  ListFilesOutput{Computer: "Studio Mac", Path: "/Users/k", Entries: []FileEntry{{Path: "/Users/k/a.md", Type: "file", Size: 1}}},
	}
	binding := &skill.Binding{ID: "computer"}
	for action, output := range outputs {
		bound := &skill.BoundAction{Definition: d, Binding: binding, Action: d.Actions[action]}
		if err := catalog.ValidateOutput(t.Context(), bound, asMap(t, output)); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
}
