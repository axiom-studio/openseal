package runloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Script routines on the loop. A script routine's occurrence is an ordinary
// agent Run: the model runs the approved script itself with
// workspace_run_command and then reports. Two kernel guards keep it to the
// approved script:
//
//  1. Pin: the claim pins the approved version (PreparedRoutineScript.Pins). The
//     host tool dispatcher attaches it as expectedDigest to each run_command
//     that invokes the script, so an edited script never runs, and stores
//     what each pinned command did on its tool entry (ToolEntry.PinnedRun).
//  2. Completion check: a final answer of a Run whose tool entries show no
//     clean run of the approved command at the approved version (exit 0)
//     does not complete it. The Run records a routine_script failure with the
//     real exit status and output tail, or that the script did not run; its
//     next step is the tool-free explanation, and the Run then fails.

// RoutineScriptRulesText is the routine_script system section's rules. The
// section adds the routine's exact script, command and timeout after it.
const RoutineScriptRulesText = "This Run belongs to a routine that runs an approved script from this chat's files. First run that script with workspace_run_command, using exactly the command below from the chat files root (leave workingDirectory out); the approved version runs even if the file changed since. Then do what the goal asks with its result: report what the script found or made, naming the files it produced so they are shared with the report, and use the tools the goal needs. Do not edit, rewrite, replace or work around the script, and do not redo its work with other tools. If it fails, times out or is refused, do not rerun, edit or repair it and do not try the work another way: explain plainly what failed (the command, its exit status or timeout, or that it did not run, and the relevant last lines of its output) and what the person could change. The Run succeeds only if the approved script ran and exited with status 0."

// RunFailureCodeRoutineScript is the FailureExplanation code of a script
// routine whose approved script failed or did not run.
const RunFailureCodeRoutineScript = "routine_script_failed"

// routineScriptRunCommandTool is the loop tool name of workspace.run_command.
var routineScriptRunCommandTool = modelstep.HostToolName(workspace.OperationRunCommand)

// PreparedRoutineScript is a claim's routine script: the approved script and the
// pins every run_command of the claim carries.
type PreparedRoutineScript struct {
	Script *models.RoutineScript
	Pins   []workspace.CommandPin
}

// PrepareRoutineScript reads the pinned script of run once per claim.
// It is nil, nil for a Run that is no script routine. A script routine runs
// only where workspace_run_command is offered (hostTools of the claim's chat
// workspace); otherwise, or when the pinned script is invalid, the claim
// cannot run.
func PrepareRoutineScript(run *models.AgentRun, hostTools []modelstep.HostToolSpec) (*PreparedRoutineScript, error) {
	if run == nil || run.Context == nil {
		return nil, nil
	}
	raw, ok := run.Context[routine.RoutineScriptContextKey]
	if !ok || raw == nil {
		return nil, nil
	}
	script, err := routine.DecodeScript(raw)
	if err != nil {
		return nil, fmt.Errorf("routine script in Run context is invalid: %w", err)
	}
	if _, offered := modelstep.LookupHostTool(hostTools, workspace.OperationRunCommand); !offered {
		return nil, errors.New("a routine script runs only in its chat's sandbox")
	}
	pin := workspace.CommandPin{Path: script.Path, Digest: script.Digest, FilesVersion: script.FilesVersion, Command: script.Command, TimeoutSeconds: script.TimeoutSeconds}
	return &PreparedRoutineScript{Script: script, Pins: []workspace.CommandPin{pin}}, nil
}

// CommandPins are the pins of the claim's run_command calls (HostToolCall.CommandPins).
func (s *PreparedRoutineScript) CommandPins() []workspace.CommandPin {
	if s == nil {
		return nil
	}
	return append([]workspace.CommandPin(nil), s.Pins...)
}

// Section is the routine_script kernel system section. A routine's session
// is its own (a fresh run session), so the script's facts are stable for the
// session and belong in the section.
func (s *PreparedRoutineScript) Section() models.ModelStepSystemSection {
	return models.ModelStepSystemSection{
		ID:    modelstep.LoopSectionRoutineScript,
		Title: modelstep.LoopKernelSectionTitles[modelstep.LoopSectionRoutineScript],
		Text:  routineScriptSectionText(s.Script),
	}
}

func routineScriptSectionText(script *models.RoutineScript) string {
	return fmt.Sprintf("%s\nApproved script: %s\nCommand: %s\ntimeoutSeconds: %d", RoutineScriptRulesText, script.Path, script.Command, script.TimeoutSeconds)
}

// CompletionFailure is why the Run cannot complete with the run_command tool
// entries among entries (any order), or "" when the approved command ran
// cleanly at the approved version.
func (s *PreparedRoutineScript) CompletionFailure(entries []*models.SessionEntry) string {
	pin := s.Pins[0]
	script := s.Script
	var last *routineScriptAttempt
	for _, attempt := range routineScriptAttempts(s.Pins, entries) {
		if attempt.record != nil && attempt.record.Runs(pin) && attempt.record.Succeeded(script.Digest) {
			return ""
		}
		if attempt.interrupted || attempt.record.Path == script.Path {
			current := attempt
			last = &current
		}
	}
	var message string
	switch {
	case last == nil:
		return "The routine's script did not run: " + script.Command
	case last.interrupted:
		message = "The routine's script was interrupted before it finished: " + script.Command
	case last.record.Refused != "":
		message = "The routine's script " + script.Path + " could not run: " + strings.TrimSpace(last.record.Refused)
	case last.record.TimedOut:
		message = fmt.Sprintf("The routine's script timed out after %d seconds: %s", script.TimeoutSeconds, strings.TrimSpace(last.record.Command))
	case last.record.ExitCode != 0:
		message = fmt.Sprintf("The routine's script exited with status %d: %s", last.record.ExitCode, strings.TrimSpace(last.record.Command))
	default:
		// It ran, but not as approved: another command or directory.
		return "The routine's script did not run as approved: " + script.Command
	}
	if last.record != nil {
		if output := strings.TrimSpace(last.record.OutputTail); output != "" {
			const lastBytes = 1500
			if len(output) > lastBytes {
				output = output[len(output)-lastBytes:]
				for !utf8.ValidString(output) {
					output = output[1:]
				}
			}
			message += "\nLast output:\n" + output
		}
	}
	if len(message) > 4000 {
		message = message[:4000]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

// FinalFailure is the FailureExplanation that replaces a final answer of a
// script routine whose approved script did not run cleanly, or nil when the
// Run may complete. The caller stores it with TransitionRun (the Run stays
// running in explain mode), appends failureExplanationNote, and the Run
// fails after the tool-free explanation step. The full message, with the
// output tail's line breaks, is StepError's source too, bounded like it.
func (s *PreparedRoutineScript) FinalFailure(run *models.AgentRun, entries []*models.SessionEntry) *models.RunFailureExplanation {
	if s == nil {
		return nil
	}
	message := s.CompletionFailure(entries)
	if message == "" {
		return nil
	}
	return conversation.BeginRunFailureExplanation(run, conversation.RunFailureKindRoutineScript, RunFailureCodeRoutineScript, message, "", "")
}

// routineScriptAttempt is one run_command tool entry of the Run: its pinned
// record, or interrupted when its outcome is unknown.
type routineScriptAttempt struct {
	record      *workspace.CommandRecord
	interrupted bool
}

// routineScriptAttempts are the Run's terminal run_command entries that ran
// (or tried to run) a pinned file, oldest first. An interrupted entry whose
// command invokes a pinned file counts even without a record (a resumed
// write has none): the script may have started.
func routineScriptAttempts(pins []workspace.CommandPin, entries []*models.SessionEntry) []routineScriptAttempt {
	ordered := make([]*models.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.Type != models.SessionEntryTool || entry.Tool == nil || !entry.State.Terminal() {
			continue
		}
		if entry.Tool.Name != routineScriptRunCommandTool && entry.Tool.Name != workspace.OperationRunCommand {
			continue
		}
		ordered = append(ordered, entry)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	attempts := make([]routineScriptAttempt, 0, len(ordered))
	for _, entry := range ordered {
		var record *workspace.CommandRecord
		if len(entry.Tool.PinnedRun) > 0 {
			var decoded workspace.CommandRecord
			if json.Unmarshal(entry.Tool.PinnedRun, &decoded) == nil {
				record = &decoded
			}
		}
		interrupted := entry.State == models.ToolEntryInterrupted
		if record == nil && !(interrupted && invokesPinnedFile(pins, entry.Tool.Arguments)) {
			continue // a command that ran no pinned file
		}
		attempts = append(attempts, routineScriptAttempt{record: record, interrupted: interrupted})
	}
	return attempts
}

func invokesPinnedFile(pins []workspace.CommandPin, raw json.RawMessage) bool {
	var arguments map[string]interface{}
	if len(raw) == 0 || json.Unmarshal(raw, &arguments) != nil {
		return false
	}
	_, pin := workspace.PinRunCommand(pins, arguments)
	return pin != nil
}
