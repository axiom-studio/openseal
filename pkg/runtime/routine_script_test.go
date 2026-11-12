package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"

	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
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

func (f *routineScriptFiles) ResolveChatFile(_ context.Context, _ Scope, _ ObjectiveOwner, conversationID, path string, version int64) (*routine.ChatFile, error) {
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
			Run:   &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}},
			Bound: &skill.BoundAction{Definition: definition, Binding: &skill.Binding{ID: "routines"}, Action: definition.Actions[action]}, ApprovalMode: ConversationApprovalSkip,
			RoutineGrant: &RoutineGrant{Action: RunbookManagementSkillID + "." + action, Label: "anything"},
		})
		if err != nil || decision.Disposition != runtimeaction.ActionDispositionRequireApproval || decision.ApprovalTimeout == ApprovalTimeoutApprove {
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

func routineScriptRun() *AgentRun {
	return &AgentRun{ID: "occurrence", Scope: Scope{Kind: "tenant", ID: "one"}, AssignedAgentID: "agent", Context: map[string]interface{}{
		conversationRunContextConversationID: "chat",
		routine.RoutineScriptContextKey: map[string]interface{}{"path": "scripts/weekly.py", "digest": routineScriptDigestOf("a"), "filesVersion": float64(2),
			"command": "python3 scripts/weekly.py", "timeoutSeconds": float64(120)},
	}}
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

// loopRoutineScriptFixture is the claim state of routineScriptRun on its
// chat's workspace.
func loopRoutineScriptFixture(t *testing.T) *PreparedRoutineScript {
	t.Helper()
	routine, err := PrepareRoutineScript(routineScriptRun(), modelstep.HostToolSpecs(workspace.ChatAuthority("chat")))
	if err != nil || routine == nil {
		t.Fatalf("prepare = %#v, %v", routine, err)
	}
	return routine
}

// loopRunCommandEntry runs command through the host tool dispatcher with the
// claim's pins, the host replying reply (nil: transport failure), and
// returns the terminal tool entry the loop records from the outcome.
func loopRunCommandEntry(t *testing.T, routine *PreparedRoutineScript, sequence int64, arguments map[string]interface{}, reply map[string]interface{}) *SessionEntry {
	t.Helper()
	answer := modelsteptest.HostReplyError(errors.New("connection reset"))
	if reply != nil {
		answer = func(_ context.Context, request HostToolRequest) (HostToolResult, error) {
			result := map[string]interface{}{}
			for name, value := range reply {
				result[name] = value
			}
			// The sandbox echoes the pin it ran under, unless told otherwise.
			if _, refused := result[workspace.RefusedResult]; !refused && result[workspace.ExpectedDigestArgument] == nil {
				result[workspace.ExpectedDigestArgument] = request.Arguments[workspace.ExpectedDigestArgument]
			}
			if result[workspace.ExpectedDigestArgument] == false {
				delete(result, workspace.ExpectedDigestArgument)
			}
			return modelsteptest.HostReplyJSON(result, false)(context.Background(), request)
		}
	}
	dispatcher := modelstep.NewHostToolDispatcher(&modelsteptest.FakeHostToolHost{Replies: []func(context.Context, HostToolRequest) (HostToolResult, error){answer}},
		runtimeaction.NewActionCoordinator(nil, nil, nil, nil, nil), nil)
	call := modelsteptest.HostToolCallFixture(t, workspace.OperationRunCommand, arguments)
	call.ToolEntryID = fmt.Sprintf("ent_%d", sequence)
	call.CommandPins = routine.CommandPins()
	outcome, ok := dispatcher.Run(context.Background(), call)
	if !ok {
		t.Fatal("Run returned ok=false")
	}
	patch, err := outcome.KernelPatch()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(arguments)
	return &SessionEntry{ID: call.ToolEntryID, Sequence: sequence, RunID: "occurrence", StepID: "step", Type: SessionEntryTool, State: outcome.State,
		ToolCallID: fmt.Sprintf("call_%d", sequence),
		Tool:       &ToolEntry{Name: routineScriptRunCommandTool, Arguments: encoded, Result: outcome.Result, PinnedRun: patch.PinnedRun, OriginRef: patch.OriginRef}}
}

func TestLoopRoutineScriptPreparesPinsAndSection(t *testing.T) {
	tools := modelstep.HostToolSpecs(workspace.ChatAuthority("chat"))
	if routine, err := PrepareRoutineScript(&AgentRun{ID: "chat-run", Context: map[string]interface{}{}}, tools); routine != nil || err != nil {
		t.Fatalf("plain Run = %#v, %v", routine, err)
	}
	if routine, err := PrepareRoutineScript(nil, tools); routine != nil || err != nil {
		t.Fatalf("nil Run = %#v, %v", routine, err)
	}
	invalid := routineScriptRun()
	invalid.Context[routine.RoutineScriptContextKey] = map[string]interface{}{"path": "scripts/weekly.py", "command": "rm -rf /"}
	if _, err := PrepareRoutineScript(invalid, tools); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid script err = %v", err)
	}
	var withoutCommand []modelstep.HostToolSpec
	for _, spec := range tools {
		if spec.Operation != workspace.OperationRunCommand {
			withoutCommand = append(withoutCommand, spec)
		}
	}
	for _, offered := range [][]modelstep.HostToolSpec{nil, withoutCommand} {
		if _, err := PrepareRoutineScript(routineScriptRun(), offered); err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Fatalf("no run_command err = %v", err)
		}
	}

	routine := loopRoutineScriptFixture(t)
	want := workspace.CommandPin{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2, Command: "python3 scripts/weekly.py", TimeoutSeconds: 120}
	if pins := routine.CommandPins(); len(pins) != 1 || pins[0] != want {
		t.Fatalf("pins = %#v", pins)
	}
	routine.CommandPins()[0].Digest = "changed"
	if routine.Pins[0].Digest != want.Digest {
		t.Fatal("CommandPins shares the claim's pins")
	}
	var nilRoutine *PreparedRoutineScript
	if nilRoutine.CommandPins() != nil || nilRoutine.FinalFailure(routineScriptRun(), nil) != nil {
		t.Fatal("a non-routine claim pins or fails")
	}

	section := routine.Section()
	if section.ID != "routine_script" || section.Title != "Routine script" {
		t.Fatalf("section = %#v", section)
	}
	if !strings.HasPrefix(section.Text, RoutineScriptRulesText+"\n") ||
		!strings.HasSuffix(section.Text, "\nApproved script: scripts/weekly.py\nCommand: python3 scripts/weekly.py\ntimeoutSeconds: 120") {
		t.Fatalf("section text = %q", section.Text)
	}
	if !strings.Contains(section.Text, routineScriptRunCommandTool) || routineScriptRunCommandTool != "workspace_run_command" {
		t.Fatalf("section names another tool: %q", section.Text)
	}
	lower := strings.ToLower(section.Text)
	for _, word := range []string{"runoutput", "outputsummary", "continuationcheckpoint", "this turn", "proposal", "hosted-turn", "offeredactions", "_atlas", "workspace.run_command"} {
		if strings.Contains(lower, word) {
			t.Errorf("section contains form wording %q", word)
		}
	}
}

// The completion check reads the pinned records on the Run's run_command tool
// entries, as the host tool dispatcher stored them.
func TestLoopRoutineScriptCompletionCheckReadsToolEntries(t *testing.T) {
	routine := loopRoutineScriptFixture(t)
	approved := map[string]interface{}{"command": "python3 scripts/weekly.py"}
	clean := map[string]interface{}{"exitStatus": float64(0), "stdout": "412 signups\n"}
	for name, testCase := range map[string]struct {
		entries func(*testing.T) []*SessionEntry
		want    []string // "" alone: the Run may complete
	}{
		"clean run": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, approved, clean)}
			},
			want: []string{""},
		},
		"clean run with extra spaces": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, map[string]interface{}{"command": "python3   scripts/weekly.py"}, clean)}
			},
			want: []string{""},
		},
		"clean run then an unrelated failing command": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{
					loopRunCommandEntry(t, routine, 5, map[string]interface{}{"command": "ls out"}, map[string]interface{}{"exitStatus": float64(2)}),
					loopRunCommandEntry(t, routine, 3, approved, clean),
				}
			},
			want: []string{""},
		},
		"never ran": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, map[string]interface{}{"command": "ls"}, map[string]interface{}{"exitStatus": float64(0)})}
			},
			want: []string{"The routine's script did not run: python3 scripts/weekly.py"},
		},
		"pending entry does not count": {
			entries: func(t *testing.T) []*SessionEntry {
				entry := loopRunCommandEntry(t, routine, 3, approved, clean)
				entry.State, entry.Tool.Result = ToolEntryPending, nil
				return []*SessionEntry{entry}
			},
			want: []string{"The routine's script did not run"},
		},
		"failed": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, approved, map[string]interface{}{"exitStatus": float64(1), "stderr": "Traceback (most recent call last):\nKeyError: 'signup_date'"})}
			},
			want: []string{"exited with status 1: python3 scripts/weekly.py", "Last output:\nTraceback (most recent call last):\nKeyError: 'signup_date'"},
		},
		"failed again after a clean-looking unconfirmed run": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{
					loopRunCommandEntry(t, routine, 3, approved, map[string]interface{}{"exitStatus": float64(0), workspace.ExpectedDigestArgument: false}),
					loopRunCommandEntry(t, routine, 4, approved, map[string]interface{}{"exitStatus": float64(3)}),
				}
			},
			want: []string{"exited with status 3"},
		},
		"timed out": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, approved, map[string]interface{}{"exitStatus": float64(-1), "timedOut": true})}
			},
			want: []string{"timed out after 120 seconds: python3 scripts/weekly.py"},
		},
		"edited script refused": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, approved, map[string]interface{}{workspace.RefusedResult: "the chat's files hold another version of scripts/weekly.py than the approved one; it was not run"})}
			},
			want: []string{"The routine's script scripts/weekly.py could not run: the chat's files hold another version"},
		},
		"other command": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, map[string]interface{}{"command": "python3 scripts/weekly.py --skip-checks"}, clean)}
			},
			want: []string{"did not run as approved: python3 scripts/weekly.py"},
		},
		"interrupted": {
			entries: func(t *testing.T) []*SessionEntry {
				return []*SessionEntry{loopRunCommandEntry(t, routine, 3, approved, nil)}
			},
			want: []string{"interrupted before it finished: python3 scripts/weekly.py"},
		},
		"interrupted on resume without a record": {
			entries: func(t *testing.T) []*SessionEntry {
				entry := loopRunCommandEntry(t, routine, 3, approved, nil)
				entry.Tool.PinnedRun = nil
				return []*SessionEntry{entry}
			},
			want: []string{"interrupted before it finished"},
		},
		"interrupted unrelated command": {
			entries: func(t *testing.T) []*SessionEntry {
				entry := loopRunCommandEntry(t, routine, 3, map[string]interface{}{"command": "ls"}, nil)
				return []*SessionEntry{entry}
			},
			want: []string{"The routine's script did not run"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			entries := testCase.entries(t)
			entries = append(entries, &SessionEntry{ID: "note", Sequence: 1, Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteRunContext, Text: "x"}}, nil)
			message := routine.CompletionFailure(entries)
			if testCase.want[0] == "" {
				if message != "" {
					t.Fatalf("message = %q, want none", message)
				}
				if routine.FinalFailure(routineScriptRun(), entries) != nil {
					t.Fatal("a clean run fails the Run")
				}
				return
			}
			for _, want := range testCase.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message %q lacks %q", message, want)
				}
			}
		})
	}
}

// A failed script puts the Run in explain mode with a routine_script
// FailureExplanation; the first recorded failure stays the reported one.
func TestLoopRoutineScriptFinalFailureExplainsAndFails(t *testing.T) {
	const failureTextLimit = 1024 // the bound of a Run failure message
	routine := loopRoutineScriptFixture(t)
	output := strings.Repeat("line of output\n", 400) + "KeyError: 'signup_date'"
	entries := []*SessionEntry{loopRunCommandEntry(t, routine, 3, map[string]interface{}{"command": "python3 scripts/weekly.py"}, map[string]interface{}{"exitStatus": float64(1), "stderr": output})}
	if message := routine.CompletionFailure(entries); len(message) > 4000 || !strings.HasSuffix(message, "KeyError: 'signup_date'") {
		t.Fatalf("message (%d bytes) = %q", len(message), message)
	}
	run := routineScriptRun()
	run.Status = AgentRunStatusRunning
	failure := routine.FinalFailure(run, entries)
	if failure == nil || failure.Kind != "routine_script" || failure.Code != RunFailureCodeRoutineScript || failure.Explained {
		t.Fatalf("failure = %#v", failure)
	}
	if !strings.HasPrefix(failure.Message, "The routine's script exited with status 1: python3 scripts/weekly.py") ||
		!strings.HasSuffix(failure.Message, "…") || len(failure.Message) > failureTextLimit+len("…") {
		t.Fatalf("message = %q", failure.Message)
	}
	run.FailureExplanation = failure
	if !conversation.RunExplainingFailure(run) {
		t.Fatal("the Run is not in explain mode")
	}
	note := conversation.FailureExplanationNote("ses", run.ID, "step_2", failure)
	if note == nil || note.Note.Kind != NoteFailureExplanation || !strings.Contains(note.Note.Text, "This attempt has stopped: The routine's script exited with status 1") {
		t.Fatalf("note = %#v", note)
	}

	earlier := &RunFailureExplanation{Kind: conversation.RunFailureKindExecution, Code: "execution_failed", Message: "The model provider failed."}
	run.FailureExplanation = earlier
	if kept := routine.FinalFailure(run, entries); kept == nil || *kept != *earlier {
		t.Fatalf("kept = %#v", kept)
	}
}
