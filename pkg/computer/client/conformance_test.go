package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/workspace/workspacetest"
)

// computerMachine is this computer's bridge as a machine: one allowed write
// folder whose paths are absolute.
type computerMachine struct {
	root string
}

func (m computerMachine) Path(rel string) string {
	return filepath.Join(m.root, filepath.FromSlash(rel))
}
func (m computerMachine) Shown(rel string) string { return m.Path(rel) }

func (m computerMachine) Do(ctx context.Context, op string, args map[string]interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	result := Execute(ctx, bridge.Call{ID: "c", Op: op, Args: raw, Folders: []bridge.Folder{{Path: m.root, Access: bridge.AccessWrite}},
		DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	if !result.OK {
		return nil, errors.New(result.Error.Message)
	}
	var output map[string]interface{}
	return output, json.Unmarshal(result.Result, &output)
}

func (m computerMachine) Seed(t *testing.T, rel string, data []byte) {
	t.Helper()
	path := m.Path(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeConformsToTheMachineOperations(t *testing.T) {
	workspacetest.Conformance(t, func(t *testing.T) workspacetest.Machine {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(root, "project")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		return computerMachine{root: root}
	}, workspacetest.Options{Files: true, Commands: runtime.GOOS != "windows"})
}
