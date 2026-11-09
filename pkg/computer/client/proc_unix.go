//go:build !windows

package client

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs command through a login shell, so the person's PATH
// applies.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-lc", command)
}

// processTree is a command and the processes it starts: its process group.
type processTree struct{}

// isolate puts the command in its own process group and makes cancelling
// kill the whole group, so children it started stop too.
func isolate(cmd *exec.Cmd) *processTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return &processTree{}
}

// attach does nothing: Setpgid already grouped the command at start.
func (*processTree) attach(*exec.Cmd) {}

func (*processTree) close() {}

func isWindows() bool { return false }
