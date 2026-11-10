//go:build windows

package client

import (
	"context"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"

	"github.com/axiom-studio/openseal/pkg/workspace"
	"golang.org/x/sys/windows"
)

// nonBlocking is no flag on Windows, whose folders hold no named pipes.
const nonBlocking = 0

// shellCommand runs command through PowerShell.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

// processTree is a command and every process it starts, held in a Job
// Object: children join their parent's job, so a grandchild whose parent
// already exited is still in it.
type processTree struct {
	job      windows.Handle
	attached atomic.Bool
}

// isolate makes cancelling kill the command's whole process tree: the job
// once attach put the command in it, else taskkill on the live tree.
func isolate(cmd *exec.Cmd) *processTree {
	tree := &processTree{}
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		tree.job = job
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if tree.attached.Load() && windows.TerminateJobObject(tree.job, workspace.ExitTimedOut) == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return tree
}

// attach puts the started command in the job.
func (t *processTree) attach(cmd *exec.Cmd) {
	if t.job == 0 {
		return
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(process)
	if windows.AssignProcessToJobObject(t.job, process) == nil {
		t.attached.Store(true)
	}
}

// close releases the job once the command was waited for. Processes the
// command left running in the background keep running, as on macOS and
// Linux.
func (t *processTree) close() {
	if t.job != 0 {
		windows.CloseHandle(t.job)
	}
}

func isWindows() bool { return true }
