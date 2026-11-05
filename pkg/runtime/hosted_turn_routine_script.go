package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/workspace"
)

// A script routine's occurrence is an ordinary agent run in its chat. The
// model runs the approved script itself with workspace.run_command, as it
// would in the chat, and then reports. Two kernel guards keep it to the
// approved script:
//
//  1. Pin: every Turn of the Run carries the approved version as
//     HostedTurnRequest.CommandPins. The host attaches it as expectedDigest to
//     each run_command that invokes the script (workspace.PinRunCommand), so
//     an edited script never runs, and returns what each pinned command did
//     in HostedTurnResponse.PinnedCommands. The kernel keeps those records in
//     the Run's checkpoint (PinnedCommandsCheckpointKey).
//  2. Completion check: a Turn that would complete the Run without a clean
//     run of the approved command at the approved version (exit 0) does not
//     complete it. The kernel records a script failure with the real exit
//     status and output tail, or that the script did not run, and the model
//     explains it in place of its report; the Run then fails.

// PinnedCommandsCheckpointKey is kernel-owned: the records of a Run's pinned
// run_command calls, newest last.
const PinnedCommandsCheckpointKey = "_opensealPinnedCommands"

// maximumPinnedCommandsKept bounds the records a Run's checkpoint keeps.
const maximumPinnedCommandsKept = 16

// RoutineScriptInstruction applies to every Turn of a script routine's Run,
// followed by the routine's exact script and command.
const RoutineScriptInstruction = "This run belongs to a routine that runs an approved script from this chat's files. First run that script with workspace.run_command, using exactly the command below from the chat files root (leave workingDirectory out); the approved version runs even if the file changed since. Then do what the goal asks with its result: report what the script found or made, naming the files it produced so they are shared with the report, and use the actions the goal needs. Do not edit, rewrite, replace or work around the script, and do not redo its work with other tools. If it fails, do not rerun or repair it; explain what failed. The run succeeds only if the approved script ran and exited with status 0."

// RoutineScriptFailureInstruction applies to the explanation of a failed or
// skipped script.
const RoutineScriptFailureInstruction = "The routine's approved script did not complete (continuationCheckpoint._opensealFinalFailureExplanation), so this run has failed. Explain plainly what failed: the command, its exit status or timeout, or that it did not run, and the relevant last lines of its output. Suggest what the person could change. Do not rerun, edit, repair or work around the script, and do not try the work another way."

// routineScriptFromRun returns the pinned script of a script routine's Run.
func routineScriptFromRun(run *AgentRun) (*RoutineScript, bool, error) {
	if run == nil || run.Context == nil {
		return nil, false, nil
	}
	raw, ok := run.Context[RoutineScriptContextKey]
	if !ok || raw == nil {
		return nil, false, nil
	}
	script, err := decodeRoutineScript(raw)
	if err != nil {
		return nil, true, fmt.Errorf("routine script in Run context is invalid: %w", err)
	}
	return script, true, nil
}

func routineScriptPin(script *RoutineScript) workspace.CommandPin {
	return workspace.CommandPin{Path: script.Path, Digest: script.Digest, FilesVersion: script.FilesVersion, Command: script.Command, TimeoutSeconds: script.TimeoutSeconds}
}

func routineScriptInstruction(script *RoutineScript) string {
	return fmt.Sprintf("%s\nApproved script: %s\nCommand: %s\ntimeoutSeconds: %d", RoutineScriptInstruction, script.Path, script.Command, script.TimeoutSeconds)
}

// projectRoutineScript pins a script routine's Turn to its approved script
// and tells the model to run it. It reports whether the Run is one.
func projectRoutineScript(request *HostedTurnRequest, run *AgentRun) (bool, error) {
	script, ok, err := routineScriptFromRun(run)
	if err != nil || !ok {
		return false, err
	}
	if request.Workspace == nil || !offersOperation(request.WorkspaceOperations, workspace.OperationRunCommand) {
		return true, errors.New("a routine script runs only in its chat's sandbox")
	}
	request.CommandPins = []workspace.CommandPin{routineScriptPin(script)}
	request.SystemInstructions = append(request.SystemInstructions, routineScriptInstruction(script))
	return true, nil
}

func offersOperation(operations []workspace.Operation, name string) bool {
	for _, operation := range operations {
		if operation.Name == name {
			return true
		}
	}
	return false
}

// validatePinnedCommands checks the host's records of pinned commands.
func validatePinnedCommands(request HostedTurnRequest, records []workspace.CommandRecord) error {
	if len(records) == 0 {
		return nil
	}
	if len(request.CommandPins) == 0 {
		return errors.New("turn host reported pinned commands the Turn did not pin")
	}
	if len(records) > workspace.MaximumPinnedCommandRecords {
		return fmt.Errorf("turn host reported more than %d pinned commands", workspace.MaximumPinnedCommandRecords)
	}
	for _, record := range records {
		if err := record.Validate(request.CommandPins); err != nil {
			return err
		}
	}
	return nil
}

// pinnedCommandRecords reads a checkpoint's pinned command records.
func pinnedCommandRecords(checkpoint map[string]interface{}) []workspace.CommandRecord {
	raw, ok := checkpoint[PinnedCommandsCheckpointKey]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var records []workspace.CommandRecord
	if json.Unmarshal(encoded, &records) != nil {
		return nil
	}
	return records
}

// checkpointPinnedCommands appends a Turn's records to the Run's.
func checkpointPinnedCommands(checkpoint map[string]interface{}, records []workspace.CommandRecord) {
	if len(records) == 0 {
		return
	}
	all := append(pinnedCommandRecords(checkpoint), records...)
	if len(all) > maximumPinnedCommandsKept {
		all = all[len(all)-maximumPinnedCommandsKept:]
	}
	values := make([]interface{}, 0, len(all))
	for _, record := range all {
		value := map[string]interface{}{"command": record.Command, "path": record.Path, "exitCode": record.ExitCode}
		if record.WorkingDirectory != "" {
			value["workingDirectory"] = record.WorkingDirectory
		}
		if record.Digest != "" {
			value["digest"] = record.Digest
		}
		if record.TimedOut {
			value["timedOut"] = true
		}
		if record.Refused != "" {
			value["refused"] = record.Refused
		}
		if record.OutputTail != "" {
			value["outputTail"] = record.OutputTail
		}
		values = append(values, value)
	}
	checkpoint[PinnedCommandsCheckpointKey] = values
}

// routineScriptCompletionFailure is why a script routine's Run cannot
// complete with the records in checkpoint, or "" when the approved command
// ran cleanly at the approved version.
func routineScriptCompletionFailure(script *RoutineScript, checkpoint map[string]interface{}) string {
	pin := routineScriptPin(script)
	var last *workspace.CommandRecord
	records := pinnedCommandRecords(checkpoint)
	for index := range records {
		record := records[index]
		if record.Runs(pin) && record.Succeeded(script.Digest) {
			return ""
		}
		if record.Path == script.Path {
			last = &records[index]
		}
	}
	var message string
	switch {
	case last == nil:
		return "The routine's script did not run: " + script.Command
	case last.Refused != "":
		message = "The routine's script " + script.Path + " could not run: " + strings.TrimSpace(last.Refused)
	case last.TimedOut:
		message = fmt.Sprintf("The routine's script timed out after %d seconds: %s", script.TimeoutSeconds, strings.TrimSpace(last.Command))
	case last.ExitCode != 0:
		message = fmt.Sprintf("The routine's script exited with status %d: %s", last.ExitCode, strings.TrimSpace(last.Command))
	default:
		// It ran, but not as approved: another command or directory.
		return "The routine's script did not run as approved: " + script.Command
	}
	if output := strings.TrimSpace(last.OutputTail); output != "" {
		const lastBytes = 1500
		if len(output) > lastBytes {
			output = output[len(output)-lastBytes:]
			for !utf8.ValidString(output) {
				output = output[1:]
			}
		}
		message += "\nLast output:\n" + output
	}
	if len(message) > 4000 {
		message = message[:4000]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

// explainRoutineScriptFailure replaces a script routine's completing Turn,
// whose approved script did not run cleanly, with the model's explanation of
// that failure; the Run then fails with it.
func (r *HostedTurnRunner) explainRoutineScriptFailure(ctx context.Context, input TurnExecutionContext, response *HostedTurnResponse, message string) (*TurnOutcome, error) {
	failed := cloneAgentRun(input.Run)
	failed.Checkpoint = checkpointFinalFailureExplanation(response.ContinuationCheckpoint, routineScriptFailureKind, message)
	explained := input
	explained.Run = failed
	outcome, err := r.runFinalFailureExplanation(ctx, explained)
	if err != nil {
		return nil, err
	}
	outcome.ContinuationCheckpoint = deepCloneCheckpointMap(outcome.ContinuationCheckpoint)
	if outcome.ContinuationCheckpoint == nil {
		outcome.ContinuationCheckpoint = map[string]interface{}{}
	}
	for _, key := range []string{FinalFailureExplanationCheckpointKey, PinnedCommandsCheckpointKey} {
		if value, ok := failed.Checkpoint[key]; ok {
			outcome.ContinuationCheckpoint[key] = deepCloneCheckpointValue(value)
		}
	}
	outcome.Usage = addTurnUsage(response.Usage, outcome.Usage)
	outcome.Decisions = append([]TurnDecision{{Summary: "The routine's approved script did not complete, so the run failed instead of reporting."}}, outcome.Decisions...)
	return outcome, nil
}

func addTurnUsage(a, b TurnUsage) TurnUsage {
	return TurnUsage{
		InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens, Cost: a.Cost + b.Cost,
		DurationMS: a.DurationMS + b.DurationMS, ProviderDurationMS: a.ProviderDurationMS + b.ProviderDurationMS,
		ValidationDurationMS: a.ValidationDurationMS + b.ValidationDurationMS, RepairAttempts: a.RepairAttempts + b.RepairAttempts,
		NativeOperations: a.NativeOperations + b.NativeOperations,
	}
}
