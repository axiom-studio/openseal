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

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

func TestRunCommandTimeoutKillsTheProcessGroup(t *testing.T) {
	w := newWorkspace(t)
	pidFile := filepath.Join(w.write, "child.pid")
	started := time.Now()
	result := call(t, w, bridge.OpRunCommand, bridge.RunCommandInput{
		Command:        "sleep 60 & echo $! > child.pid; echo started; wait",
		TimeoutSeconds: 1,
	})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("took %s; the timeout did not stop the command", elapsed)
	}
	var output bridge.RunCommandOutput
	if err := json.Unmarshal(result.Result, &output); err != nil || !result.OK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !output.TimedOut || output.ExitCode != exitTimedOut || output.Stdout != "started\n" || !strings.Contains(output.Stderr, "stopped after 1s") {
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
