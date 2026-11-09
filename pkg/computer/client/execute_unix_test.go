//go:build !windows

package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestRunCommandTimeoutKillsTheProcessGroup(t *testing.T) {
	w := newWorkspace(t)
	pidFile := filepath.Join(w.write, "child.pid")
	started := time.Now()
	result := call(t, w, workspace.OpRunCommand, map[string]any{
		"command":        "sleep 60 & echo $! > child.pid; echo started; wait",
		"timeoutSeconds": 1,
	})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("took %s; the timeout did not stop the command", elapsed)
	}
	var output workspace.CommandResult
	if err := json.Unmarshal(result.Result, &output); err != nil || !result.OK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !output.TimedOut || output.ExitStatus != workspace.ExitTimedOut || output.Stdout != "started\n" || !strings.Contains(output.Stderr, "stopped after 1 seconds") {
		t.Fatalf("output = %+v", output)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// The background child is gone (or a zombie being reaped by init).
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && !isZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] == "Z"
}

// A named pipe in a folder never blocks a call: listing and searching leave
// it out, and reading, editing or writing it is refused at once.
func TestNamedPipesNeverBlockACall(t *testing.T) {
	w := newWorkspace(t)
	pipe := filepath.Join(w.write, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	mustWrite(t, filepath.Join(w.write, "a.txt"), "needle")
	done := make(chan struct{})
	go func() {
		defer close(done)
		result := call(t, w, workspace.OpListDirectory, map[string]any{"path": w.write})
		var listing workspace.ListDirectoryResult
		if err := json.Unmarshal(result.Result, &listing); err != nil || !result.OK || len(listing.Entries) != 1 || listing.Entries[0].Path != filepath.Join(w.write, "a.txt") {
			t.Errorf("list_directory = %+v %s", result, result.Result)
		}
		result = call(t, w, workspace.OpSearchFiles, map[string]any{"query": "needle", "path": w.write})
		if !result.OK || strings.Count(string(result.Result), `"line"`) != 1 {
			t.Errorf("search_files = %+v %s", result, result.Result)
		}
		for _, op := range []string{workspace.OpReadFile, workspace.OpWriteFile, workspace.OpEditFile} {
			result = call(t, w, op, map[string]any{"path": pipe, "content": "x", "oldText": "a", "newText": "b"})
			if result.OK || result.Error == nil || result.Error.Message != pipe+" is not a regular file." {
				t.Errorf("%s on a pipe = %+v", op, result)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a call blocked on a named pipe")
	}
}
