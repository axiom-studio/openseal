package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// deadlineMargin leaves time to answer before HelloSeal stops waiting.
const deadlineMargin = 5 * time.Second

func runCommand(ctx context.Context, allowed folders, args map[string]interface{}) (workspace.CommandResult, error) {
	command := workspace.Text(args, "command")
	if strings.TrimSpace(command) == "" {
		return workspace.CommandResult{}, callError(bridge.ErrorFailed, "A command is required.")
	}
	if len(command) > workspace.MaximumCommandBytes {
		return workspace.CommandResult{}, callError(bridge.ErrorTooLarge, fmt.Sprintf("The command is %d bytes; at most %d are allowed. Write a script with write_file and run that.", len(command), workspace.MaximumCommandBytes))
	}
	var cwd string
	if raw := workspace.Text(args, "workingDirectory"); strings.TrimSpace(raw) == "" {
		folder, ok := allowed.defaultFolder()
		if !ok || folder.access != bridge.AccessWrite {
			return workspace.CommandResult{}, callError(bridge.ErrorReadOnly, "No folder on this computer allows writes, so commands cannot run. Allow one in Settings → Computers.")
		}
		cwd = folder.path
	} else {
		resolved, cerr := allowed.resolve(raw, true)
		if cerr != nil {
			return workspace.CommandResult{}, cerr
		}
		info, err := os.Stat(resolved)
		if err != nil && !missing(err) {
			return workspace.CommandResult{}, fileError(resolved, err)
		}
		if err != nil || !info.IsDir() {
			return workspace.NoWorkingDirectory(resolved), nil
		}
		cwd = resolved
	}

	timeout := time.Duration(workspace.Integer(args, "timeoutSeconds", workspace.DefaultCommandSeconds, workspace.ChatCommandSeconds)) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline) - deadlineMargin; left < timeout {
			timeout = max(left.Truncate(time.Second), time.Second)
		}
	}
	return execute(ctx, cwd, command, timeout), nil
}

// execute runs command in cwd as its own process tree and kills the whole
// tree when timeout passes or ctx ends.
func execute(ctx context.Context, cwd, command string, timeout time.Duration) workspace.CommandResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := workspace.NewOutputTail(workspace.CommandOutputBytes)
	stderr := workspace.NewOutputTail(workspace.CommandOutputBytes)
	cmd := shellCommand(ctx, command)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	tree := isolate(cmd)
	defer tree.close()

	started := time.Now()
	err := cmd.Start()
	if err == nil {
		tree.attach(cmd)
		err = cmd.Wait()
	}
	output := workspace.CommandResult{DurationMs: time.Since(started).Milliseconds()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil && cmd.ProcessState != nil:
		output.TimedOut = true
		output.ExitStatus = workspace.ExitTimedOut
		_, _ = stderr.Write([]byte(workspace.StoppedNote(int(timeout / time.Second))))
	case err == nil:
		output.ExitStatus = 0
	case errors.As(err, &exitErr):
		output.ExitStatus = exitErr.ExitCode()
	case cmd.ProcessState != nil:
		// Exited but the output pipes stayed open past WaitDelay.
		output.ExitStatus = cmd.ProcessState.ExitCode()
	default:
		output.ExitStatus = workspace.ExitNotStarted
		_, _ = stderr.Write([]byte(workspace.NotStartedNote(err)))
	}
	output.Stdout, output.Stderr = stdout.String(), stderr.String()
	output.Truncated = stdout.Truncated() || stderr.Truncated()
	return output
}
