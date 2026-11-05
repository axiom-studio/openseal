package runtime

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/chatpath"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// The scripted first step of a script routine's occurrence.
//
// When a Run started by a script routine takes its first Turn, the kernel
// sets HostedTurnRequest.PinnedScript. The host must then, before any model
// call in that Turn:
//
//  1. In the chat's sandbox (the chat named by request.Workspace), restore
//     PinnedScript.Path to the content of that path in chat files snapshot
//     PinnedScript.FilesVersion, after checking the entry's digest equals
//     PinnedScript.Digest. A mismatch or missing entry is a failed run
//     (HostedPinnedScriptResult.Error), never a run of another version.
//  2. Run exactly PinnedScript.Command from the chat files root, with
//     PinnedScript.TimeoutSeconds as its limit, the same way it runs
//     workspace.run_command (files it creates or changes are committed to
//     the chat's files). Bridged tool calls made by the script are
//     authorized as the Run's own actions, so the routine's grants apply.
//     PinnedScript.ExecutionKey identifies this one step: a retried Turn
//     carrying the same key must reuse the recorded result instead of
//     running the script again.
//  3. Build the model call from ApplyPinnedScriptResult(request, result):
//     it puts the result in continuationCheckpoint._opensealRoutineScript
//     and, when the script failed, reduces the Turn to a final failure
//     explanation with no actions or Workspace tools.
//  4. Return the Turn's response with PinnedScriptResult set to result.
//
// The kernel records the result in the Run's checkpoint, so later Turns of
// the Run never run the script again. A failed script fails the Run with the
// model's explanation as its reply.

// HostedPinnedScript is the routine script a host runs before the Turn's
// model call. It is absent from model input.
type HostedPinnedScript struct {
	ExecutionKey   string `json:"executionKey"`
	Path           string `json:"path"`
	Digest         string `json:"digest"`
	FilesVersion   int64  `json:"filesVersion"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// HostedPinnedScriptResult is what the host observed running a
// HostedPinnedScript.
type HostedPinnedScriptResult struct {
	// Digest is the version the host restored and ran; it must equal the
	// request's. It may be empty only with Error, when nothing ran.
	Digest string `json:"digest,omitempty"`
	// ExitCode is the command's exit status.
	ExitCode int  `json:"exitCode"`
	TimedOut bool `json:"timedOut,omitempty"`
	// Error says why the host could not run the script at all (the pinned
	// version is unavailable, the sandbox could not start).
	Error string `json:"error,omitempty"`
	// StdoutTail and StderrTail are the last bytes of each stream, at most
	// 16 KiB each.
	StdoutTail string `json:"stdoutTail,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
	// ChangedFiles are the chat file paths the run created, changed or
	// deleted, at most 200.
	ChangedFiles []string `json:"changedFiles,omitempty"`
	// FilesVersion is the chat files snapshot holding the run's changes, or
	// 0 when it changed nothing.
	FilesVersion int64 `json:"filesVersion,omitempty"`
	DurationMS   int64 `json:"durationMs,omitempty"`
}

// Failed reports whether the scripted step did not succeed.
func (r HostedPinnedScriptResult) Failed() bool {
	return strings.TrimSpace(r.Error) != "" || r.TimedOut || r.ExitCode != 0
}

// RoutineScriptInstruction applies to every Turn of a script routine's Run.
const RoutineScriptInstruction = "This run belongs to a routine that runs an approved script. Its first step already ran that script once, before your turn: continuationCheckpoint._opensealRoutineScript holds the script (path, digest, command) and its result (exitCode, timedOut, error, stdoutTail, stderrTail, changedFiles). Write this run's report from that result, as the goal asks: what the script found or made, naming the files it produced so they are shared with the report. You may read the files it produced. Do not run, edit, rewrite or replace the script, and do not redo its work with other tools."

// RoutineScriptFailureInstruction applies to the Turn after a failed script.
const RoutineScriptFailureInstruction = "The routine's approved script failed (continuationCheckpoint._opensealRoutineScript.result). This run has failed. Explain plainly what failed: the command, its exit status or timeout, and the relevant last lines of its output. Suggest what the person could change. Do not rerun, edit, repair or work around the script, and do not try the work another way."

// routineScriptReadOperations are the Workspace tools a script routine's
// Turns keep: they read what the script made, never change or rerun it.
var routineScriptReadOperations = map[string]bool{
	workspace.OperationListDirectory: true, workspace.OperationReadFile: true,
	workspace.OperationSearchFiles: true, workspace.OperationBatchRead: true,
}

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

// projectRoutineScript shapes a script routine's Turn: read-only Workspace
// tools, the report instruction, and, until the step has run, the script
// the host runs before the model call.
func projectRoutineScript(request *HostedTurnRequest, run *AgentRun) error {
	script, ok, err := routineScriptFromRun(run)
	if err != nil || !ok {
		return err
	}
	if request.Workspace == nil {
		return errors.New("a routine script runs only in its chat")
	}
	operations := request.WorkspaceOperations[:0:0]
	for _, operation := range request.WorkspaceOperations {
		if routineScriptReadOperations[operation.Name] {
			operations = append(operations, operation)
		}
	}
	request.WorkspaceOperations = operations
	request.SystemInstructions = append(request.SystemInstructions, RoutineScriptInstruction)
	if _, ran := run.Checkpoint[RoutineScriptCheckpointKey]; ran {
		return nil
	}
	request.PinnedScript = &HostedPinnedScript{
		ExecutionKey: "routine-script:" + run.ID + ":" + script.Digest,
		Path:         script.Path, Digest: script.Digest, FilesVersion: script.FilesVersion,
		Command: script.Command, TimeoutSeconds: script.TimeoutSeconds,
	}
	return nil
}

func (r HostedPinnedScriptResult) validate(script *HostedPinnedScript) error {
	if script == nil {
		return errors.New("turn host ran a routine script the Turn did not request")
	}
	if len(r.Error) > 4096 || !utf8.ValidString(r.Error) {
		return errors.New("routine script error must be valid text of at most 4096 bytes")
	}
	if r.Digest != script.Digest && (r.Digest != "" || strings.TrimSpace(r.Error) == "") {
		return errors.New("turn host ran another version of the routine script")
	}
	if r.ExitCode < -1 || r.ExitCode > 255 || r.FilesVersion < 0 || r.DurationMS < 0 {
		return errors.New("routine script result is out of range")
	}
	for _, tail := range []string{r.StdoutTail, r.StderrTail} {
		if len(tail) > maximumRoutineScriptOutputTail || !utf8.ValidString(tail) {
			return fmt.Errorf("routine script output tails must be valid text of at most %d bytes", maximumRoutineScriptOutputTail)
		}
	}
	if len(r.ChangedFiles) > maximumRoutineScriptChangedFiles {
		return fmt.Errorf("routine script result lists more than %d changed files", maximumRoutineScriptChangedFiles)
	}
	for _, name := range r.ChangedFiles {
		if clean, err := chatpath.Clean(name); err != nil || clean != name {
			return fmt.Errorf("routine script changed file %q is not a chat file path", name)
		}
	}
	return nil
}

// failureMessage is the kernel's account of a failed script: the command,
// how it ended, and the last lines of its output.
func (r HostedPinnedScriptResult) failureMessage(script *HostedPinnedScript) string {
	var message string
	switch {
	case strings.TrimSpace(r.Error) != "":
		message = "The routine's script " + script.Path + " could not run: " + strings.TrimSpace(r.Error)
	case r.TimedOut:
		message = fmt.Sprintf("The routine's script timed out after %d seconds: %s", script.TimeoutSeconds, script.Command)
	default:
		message = fmt.Sprintf("The routine's script exited with status %d: %s", r.ExitCode, script.Command)
	}
	tail := strings.TrimSpace(r.StderrTail)
	if tail == "" {
		tail = strings.TrimSpace(r.StdoutTail)
	}
	if tail != "" {
		const lastBytes = 1500
		if len(tail) > lastBytes {
			tail = tail[len(tail)-lastBytes:]
			for !utf8.ValidString(tail) {
				tail = tail[1:]
			}
		}
		message += "\nLast output:\n" + tail
	}
	if len(message) > 4000 {
		message = message[:4000]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

// routineScriptCheckpointRecord is the kernel-owned record of the step.
func routineScriptCheckpointRecord(script *HostedPinnedScript, result HostedPinnedScriptResult) map[string]interface{} {
	changed := make([]interface{}, 0, len(result.ChangedFiles))
	for _, name := range result.ChangedFiles {
		changed = append(changed, name)
	}
	outcome := map[string]interface{}{
		"digest": result.Digest, "exitCode": result.ExitCode, "timedOut": result.TimedOut,
		"stdoutTail": result.StdoutTail, "stderrTail": result.StderrTail, "changedFiles": changed,
		"filesVersion": result.FilesVersion, "durationMs": result.DurationMS,
	}
	if result.Error != "" {
		outcome["error"] = result.Error
	}
	return map[string]interface{}{
		"script": map[string]interface{}{
			"path": script.Path, "digest": script.Digest, "filesVersion": script.FilesVersion,
			"command": script.Command, "timeoutSeconds": script.TimeoutSeconds,
		},
		"result": outcome, "failed": result.Failed(),
	}
}

// ApplyPinnedScriptResult returns the request a host makes its model call
// with after running request.PinnedScript: the step's record is in
// continuationCheckpoint._opensealRoutineScript, PinnedScript is cleared, and
// a failed script leaves only a final failure explanation on offer. The
// kernel validates the Turn's response against the same projection.
func ApplyPinnedScriptResult(request HostedTurnRequest, result HostedPinnedScriptResult) (HostedTurnRequest, error) {
	script := request.PinnedScript
	if err := result.validate(script); err != nil {
		return HostedTurnRequest{}, err
	}
	checkpoint := deepCloneCheckpointMap(request.ContinuationCheckpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	checkpoint[RoutineScriptCheckpointKey] = routineScriptCheckpointRecord(script, result)
	request.ContinuationCheckpoint = checkpoint
	request.PinnedScript = nil
	if result.Failed() {
		request.ContinuationCheckpoint = checkpointFinalFailureExplanation(checkpoint, routineScriptFailureKind, result.failureMessage(script))
		projectFinalFailureExplanation(&request)
		request.SystemInstructions = append(request.SystemInstructions, RoutineScriptFailureInstruction)
	}
	return request, nil
}
