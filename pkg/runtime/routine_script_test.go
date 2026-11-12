package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"

	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// routineScriptFiles is a chat files store: immutable snapshots per chat.
type routineScriptFiles struct {
	snapshots map[string][]map[string]string // conversation -> version-1 -> path -> digest
}

func (f *routineScriptFiles) write(conversationID, path, digest string) {
	if f.snapshots == nil {
		f.snapshots = map[string][]map[string]string{}
	}
	versions := f.snapshots[conversationID]
	next := map[string]string{}
	if len(versions) > 0 {
		for name, value := range versions[len(versions)-1] {
			next[name] = value
		}
	}
	if digest == "" {
		delete(next, path)
	} else {
		next[path] = digest
	}
	f.snapshots[conversationID] = append(versions, next)
}

func (f *routineScriptFiles) ResolveChatFile(_ context.Context, _ models.Scope, _ models.ObjectiveOwner, conversationID, path string, version int64) (*routine.ChatFile, error) {
	versions := f.snapshots[conversationID]
	if version == 0 {
		version = int64(len(versions))
	}
	if version < 1 || version > int64(len(versions)) {
		return nil, errors.New("chat files version is unavailable")
	}
	digest, ok := versions[version-1][path]
	if !ok {
		return nil, routine.ErrChatFileNotFound
	}
	return &routine.ChatFile{Path: path, Digest: digest, FilesVersion: version, SizeBytes: 10}, nil
}

func routineScriptDigestOf(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)[:64]
}

// The 1.9.0 definition is immutable once published (it is not yet): any
// change to its descriptions or schemas after release needs a new version. Its create_task runs saved
// scripts, documents testing before scheduling and starts routines on a schedule or an event.
func TestRoutineScriptContractVersions(t *testing.T) {
	if routine.RunbookManagementSkillVersion != "1.9.0" {
		t.Fatalf("version=%s", routine.RunbookManagementSkillVersion)
	}
	current := routine.RunbookManagementSkill()
	encoded, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	const published190SHA256 = "db7540c0cbf02cf684c40c955ccb75dc37d6b44d73294d82b654bdb368d51082"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != published190SHA256 {
		t.Fatalf("published 1.9.0 definition changed: got %s", got)
	}
	for _, action := range []string{runtimeaction.RunbookActionCreateTask, routine.RunbookActionEventSources, routine.RunbookActionSendTestEvent, runtimeaction.RunbookActionSetScript, runtimeaction.RunbookActionList, runtimeaction.RunbookActionSetStatus, routine.RunbookActionStart, routine.RunbookActionReplaceSchedule} {
		if _, ok := current.Actions[action]; !ok {
			t.Fatalf("1.9.0 lacks %s", action)
		}
	}
	if len(current.Actions) != 8 || current.Name != "Routines" {
		t.Fatalf("definition=%s with %d actions", current.Name, len(current.Actions))
	}
	create := current.Actions[runtimeaction.RunbookActionCreateTask]
	if _, ok := create.InputSchema["properties"].(map[string]interface{})["script"]; !ok || !strings.Contains(create.Description, "test it in this chat's sandbox with run_command") {
		t.Fatalf("create_task contract=%s", create.Description)
	}
	if setScript, ok := current.Actions[runtimeaction.RunbookActionSetScript]; !ok || !setScript.AlwaysReview() {
		t.Fatal("set_script must always be reviewed")
	}
	if !strings.Contains(workspace.ChatSandboxDescription, "a routine can run that exact script") {
		t.Fatal("run_command does not mention scheduling a tested script")
	}
	if err := skill.NewCatalog().Register(t.Context(), current); err != nil {
		t.Fatal(err)
	}
}

// Creating and re-pinning a script routine both always ask, in every mode.
func TestRoutineScriptChangesAlwaysNeedApproval(t *testing.T) {
	definition := routine.RunbookManagementSkill()
	for _, action := range []string{runtimeaction.RunbookActionCreateTask, runtimeaction.RunbookActionSetScript} {
		decision, err := runtimeaction.NewDefaultActionPolicy().EvaluateAction(t.Context(), runtimeaction.ActionPolicyInput{
			Run:   &models.AgentRun{ID: "run", Scope: models.Scope{Kind: "tenant", ID: "one"}},
			Bound: &skill.BoundAction{Definition: definition, Binding: &skill.Binding{ID: "routines"}, Action: definition.Actions[action]}, ApprovalMode: models.ConversationApprovalSkip,
			RoutineGrant: &models.RoutineGrant{Action: models.RunbookManagementSkillID + "." + action, Label: "anything"},
		})
		if err != nil || decision.Disposition != runtimeaction.ActionDispositionRequireApproval || decision.ApprovalTimeout == models.ApprovalTimeoutApprove {
			t.Fatalf("%s: %#v %v", action, decision, err)
		}
	}
}

func hasInstruction(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasInstructionPrefix(values []string, prefix, contains string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) && strings.Contains(value, contains) {
			return true
		}
	}
	return false
}

// The host attaches the routine's pin to every run_command that invokes the
// script, and only to those; the model cannot supply one.
func TestRoutineScriptPinIsAttachedToCommandsThatRunIt(t *testing.T) {
	pins := []workspace.CommandPin{{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2, Command: "python3 scripts/weekly.py", TimeoutSeconds: 420}}
	for _, arguments := range []map[string]interface{}{
		{"command": "python3 scripts/weekly.py"},
		{"command": "python3   scripts/weekly.py  --verbose", "timeoutSeconds": float64(30)},
		{"command": "pip install x && python3 scripts/weekly.py"},
		{"command": "python3 weekly.py", "workingDirectory": "scripts"},
		{"command": "python3 ./weekly.py", "workingDirectory": "./scripts/"},
	} {
		pinned, pin := workspace.PinRunCommand(pins, arguments)
		expected, ok := workspace.ExpectedDigestOf(pinned)
		if pin == nil || !ok || *expected != (workspace.ExpectedDigest{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2}) {
			t.Errorf("%v: not pinned: %#v", arguments, pinned)
		}
	}
	if pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py"}); pinned["timeoutSeconds"] != 420 {
		t.Fatalf("approved command timeout=%v", pinned["timeoutSeconds"])
	}
	if pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py --verbose", "timeoutSeconds": 30}); pinned["timeoutSeconds"] != 30 {
		t.Fatalf("other command timeout=%v", pinned["timeoutSeconds"])
	}
	forged := map[string]interface{}{"command": "ls data", workspace.ExpectedDigestArgument: map[string]interface{}{"path": "data/x", "digest": routineScriptDigestOf("f")}}
	if pinned, pin := workspace.PinRunCommand(pins, forged); pin != nil || pinned[workspace.ExpectedDigestArgument] != nil {
		t.Fatalf("unrelated command pinned: %#v", pinned)
	}
	if pinned, pin := workspace.PinRunCommand(nil, map[string]interface{}{"command": "python3 scripts/weekly.py"}); pin != nil || pinned[workspace.ExpectedDigestArgument] != nil {
		t.Fatal("chat command pinned without pins")
	}
	if _, ok := workspace.RecordPinnedCommand(map[string]interface{}{"command": "ls"}, map[string]interface{}{"exitStatus": float64(0)}); ok {
		t.Fatal("unpinned command recorded")
	}
	// A refusal, or a result that does not confirm the version, never
	// counts as the script having run.
	pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py"})
	if record, _ := workspace.RecordPinnedCommand(pinned, map[string]interface{}{"refused": "the approved version is not in this chat's files"}); record.Digest != "" || record.Refused == "" || record.Succeeded(pins[0].Digest) {
		t.Fatalf("refused=%#v", record)
	}
	if record, _ := workspace.RecordPinnedCommand(pinned, map[string]interface{}{"exitStatus": float64(0)}); record.Succeeded(pins[0].Digest) {
		t.Fatalf("unconfirmed=%#v", record)
	}
}
