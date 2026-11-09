package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// Exit codes a command gets when it did not exit by itself, as in the chat
// sandbox.
const (
	exitTimedOut   = 124
	exitNotStarted = 127
)

// deadlineMargin leaves time to answer before HelloSeal stops waiting.
const deadlineMargin = 5 * time.Second

func runCommand(ctx context.Context, allowed folders, input bridge.RunCommandInput) (bridge.RunCommandOutput, *bridge.CallError) {
	if input.Command == "" {
		return bridge.RunCommandOutput{}, callError(bridge.ErrorFailed, "A command is required.")
	}
	if len(input.Command) > bridge.MaxCommandBytes {
		return bridge.RunCommandOutput{}, callError(bridge.ErrorTooLarge, fmt.Sprintf("The command is %d bytes; at most %d are allowed. Write a script with write_file and run that.", len(input.Command), bridge.MaxCommandBytes))
	}
	var cwd string
	if input.Cwd == "" {
		folder, ok := allowed.defaultFolder()
		if !ok || folder.access != bridge.AccessWrite {
			return bridge.RunCommandOutput{}, callError(bridge.ErrorReadOnly, "No folder on this computer allows writes, so commands cannot run. Allow one in Settings → Computers.")
		}
		cwd = folder.path
	} else {
		resolved, cerr := allowed.resolve(input.Cwd, true)
		if cerr != nil {
			return bridge.RunCommandOutput{}, cerr
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return bridge.RunCommandOutput{}, fileError(input.Cwd, err)
		}
		if !info.IsDir() {
			return bridge.RunCommandOutput{}, callError(bridge.ErrorFailed, input.Cwd+" is not a folder.")
		}
		cwd = resolved
	}

	seconds := input.TimeoutSeconds
	if seconds <= 0 {
		seconds = bridge.DefaultCommandSeconds
	}
	if seconds > bridge.MaxCommandSeconds {
		seconds = bridge.MaxCommandSeconds
	}
	timeout := time.Duration(seconds) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline) - deadlineMargin; left < timeout {
			timeout = max(left, time.Second)
		}
	}
	return execute(ctx, cwd, input.Command, timeout), nil
}

// execute runs command in cwd in its own process group and kills the whole
// group when timeout passes or ctx ends.
func execute(ctx context.Context, cwd, command string, timeout time.Duration) bridge.RunCommandOutput {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := &tail{limit: bridge.MaxStreamBytes}
	stderr := &tail{limit: bridge.MaxStreamBytes}
	cmd := shellCommand(ctx, command)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	isolate(cmd)

	started := time.Now()
	err := cmd.Run()
	output := bridge.RunCommandOutput{Cwd: cwd, DurationMs: time.Since(started).Milliseconds()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil && cmd.ProcessState != nil:
		output.TimedOut = true
		output.ExitCode = exitTimedOut
		fmt.Fprintf(stderr, "\nThe command was stopped after %s.\n", timeout.Round(time.Second))
	case err == nil:
		output.ExitCode = 0
	case errors.As(err, &exitErr):
		output.ExitCode = exitErr.ExitCode()
	case cmd.ProcessState != nil:
		// Exited but the output pipes stayed open past WaitDelay.
		output.ExitCode = cmd.ProcessState.ExitCode()
	default:
		output.ExitCode = exitNotStarted
		fmt.Fprintf(stderr, "The command could not start: %v\n", err)
	}
	output.Stdout, output.Stderr = stdout.String(), stderr.String()
	output.Truncated = stdout.truncated || stderr.truncated
	return output
}

// tail keeps the last limit bytes written to it.
type tail struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.truncated = true
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// String is the kept output as valid UTF-8; a rune cut by the limit is
// dropped.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := t.buf
	if t.truncated {
		for i := 0; i < utf8.UTFMax && len(buf) > 0 && !utf8.RuneStart(buf[0]); i++ {
			buf = buf[1:]
		}
	}
	return string(bytes.ToValidUTF8(buf, []byte("\uFFFD")))
}
