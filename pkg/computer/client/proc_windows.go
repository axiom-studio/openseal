//go:build windows

package client

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
)

// shellCommand runs command through PowerShell.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

// isolate starts the command in its own process group and makes cancelling
// kill its whole process tree.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

func isWindows() bool { return true }
