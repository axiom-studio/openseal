package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// folderSet is an allowed write folder, an allowed read-only folder and a
// folder outside both, all resolved.
type folderSet struct {
	write, read, outside string
	folders              []bridge.Folder
}

func newWorkspace(t *testing.T) folderSet {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := folderSet{
		write:   filepath.Join(root, "project"),
		read:    filepath.Join(root, "docs"),
		outside: filepath.Join(root, "secrets"),
	}
	for _, dir := range []string{w.write, w.read, w.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(w.outside, "key.txt"), "secret")
	mustWrite(t, filepath.Join(w.read, "notes.txt"), "one\ntwo\nthree\n")
	w.folders = []bridge.Folder{{Path: w.write, Access: bridge.AccessWrite}, {Path: w.read, Access: bridge.AccessRead}}
	return w
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, w folderSet, op string, args map[string]any) bridge.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Execute(context.Background(), bridge.Call{
		ID: "c", Op: op, Args: raw, Folders: w.folders,
		DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	})
}

func wantError(t *testing.T, result bridge.Result, code string) {
	t.Helper()
	if result.OK || result.Error == nil || result.Error.Code != code {
		t.Fatalf("result = %+v (error %+v), want error %s", result, result.Error, code)
	}
}

func TestFolderEscapesAreRefused(t *testing.T) {
	w := newWorkspace(t)
	if runtime.GOOS != "windows" {
		if err := os.Symlink(w.outside, filepath.Join(w.write, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(w.outside, "key.txt"), filepath.Join(w.write, "key-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(w.outside, "new.txt"), filepath.Join(w.write, "dangling")); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		op   string
		args map[string]any
		code string
	}{
		{"dot-dot read", workspace.OpReadFile, map[string]any{"path": filepath.Join(w.write, "..", "secrets", "key.txt")}, bridge.ErrorOutsideFolder},
		{"relative dot-dot read", workspace.OpReadFile, map[string]any{"path": "../secrets/key.txt"}, bridge.ErrorOutsideFolder},
		{"absolute outside", workspace.OpReadFile, map[string]any{"path": filepath.Join(w.outside, "key.txt")}, bridge.ErrorOutsideFolder},
		{"dot-dot write", workspace.OpWriteFile, map[string]any{"path": filepath.Join(w.write, "..", "secrets", "x.txt"), "content": "x"}, bridge.ErrorOutsideFolder},
		{"list outside", workspace.OpListDirectory, map[string]any{"path": w.outside}, bridge.ErrorOutsideFolder},
		{"write in read-only folder", workspace.OpWriteFile, map[string]any{"path": filepath.Join(w.read, "notes.txt"), "content": "x"}, bridge.ErrorReadOnly},
		{"command in read-only folder", workspace.OpRunCommand, map[string]any{"command": "true", "workingDirectory": w.read}, bridge.ErrorReadOnly},
		{"command outside", workspace.OpRunCommand, map[string]any{"command": "true", "workingDirectory": w.outside}, bridge.ErrorOutsideFolder},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, []struct {
			name string
			op   string
			args map[string]any
			code string
		}{
			{"symlinked folder read", workspace.OpReadFile, map[string]any{"path": filepath.Join(w.write, "escape", "key.txt")}, bridge.ErrorOutsideFolder},
			{"symlinked file read", workspace.OpReadFile, map[string]any{"path": filepath.Join(w.write, "key-link")}, bridge.ErrorOutsideFolder},
			{"symlinked folder write", workspace.OpWriteFile, map[string]any{"path": filepath.Join(w.write, "escape", "new.txt"), "content": "x"}, bridge.ErrorOutsideFolder},
			{"symlinked folder cwd", workspace.OpRunCommand, map[string]any{"command": "true", "workingDirectory": filepath.Join(w.write, "escape")}, bridge.ErrorOutsideFolder},
			{"symlinked folder list", workspace.OpListDirectory, map[string]any{"path": filepath.Join(w.write, "escape")}, bridge.ErrorOutsideFolder},
		}...)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, call(t, w, tc.op, tc.args), tc.code)
		})
	}

	if runtime.GOOS != "windows" {
		// A dangling link is refused rather than written through.
		result := call(t, w, workspace.OpWriteFile, map[string]any{"path": filepath.Join(w.write, "dangling"), "content": "x"})
		if result.OK {
			t.Fatal("wrote through a dangling symlink")
		}
		if _, err := os.Stat(filepath.Join(w.outside, "new.txt")); err == nil {
			t.Fatal("a file appeared outside the allowed folders")
		}
	}
	if data, _ := os.ReadFile(filepath.Join(w.read, "notes.txt")); string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("read-only file changed: %q", data)
	}
}

func TestNoFoldersMeansNothingRuns(t *testing.T) {
	w := newWorkspace(t)
	w.folders = nil
	wantError(t, call(t, w, workspace.OpReadFile, map[string]any{"path": filepath.Join(w.read, "notes.txt")}), bridge.ErrorOutsideFolder)
	wantError(t, call(t, w, workspace.OpRunCommand, map[string]any{"command": "true"}), bridge.ErrorReadOnly)
}

func TestTildeExpandsToHome(t *testing.T) {
	w := newWorkspace(t)
	t.Setenv("HOME", w.write)
	t.Setenv("USERPROFILE", w.write)
	result := call(t, w, workspace.OpWriteFile, map[string]any{"path": "~/sub/hello.txt", "content": "hi"})
	if !result.OK {
		t.Fatalf("write: %+v", result.Error)
	}
	if data, err := os.ReadFile(filepath.Join(w.write, "sub", "hello.txt")); err != nil || string(data) != "hi" {
		t.Fatalf("file = %q, %v", data, err)
	}
	w.folders = []bridge.Folder{{Path: "~", Access: bridge.AccessRead}}
	result = call(t, w, workspace.OpReadFile, map[string]any{"path": "~/sub/hello.txt"})
	if !result.OK {
		t.Fatalf("read via ~ folder: %+v", result.Error)
	}
}

func TestReadOnlyFoldersAreNeverChanged(t *testing.T) {
	w := newWorkspace(t)
	// A read-only folder inside a write folder stays read-only.
	inner := filepath.Join(w.write, "lib", "vendor")
	mustWrite(t, filepath.Join(w.write, "main.go"), "package main\n")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(inner, "lib.go"), "package lib\n")
	w.folders = append(w.folders, bridge.Folder{Path: inner, Access: bridge.AccessRead})
	notes := filepath.Join(w.read, "notes.txt")
	for name, tc := range map[string]struct {
		op   string
		args map[string]any
		code string
	}{
		"edit in read-only":         {workspace.OpEditFile, map[string]any{"path": notes, "oldText": "one", "newText": "1"}, bridge.ErrorReadOnly},
		"delete in read-only":       {workspace.OpDeleteFile, map[string]any{"path": notes}, bridge.ErrorReadOnly},
		"move out of read-only":     {workspace.OpMoveFile, map[string]any{"path": notes, "destination": filepath.Join(w.write, "n.txt")}, bridge.ErrorReadOnly},
		"move into read-only":       {workspace.OpMoveFile, map[string]any{"path": filepath.Join(w.write, "main.go"), "destination": filepath.Join(w.read, "main.go")}, bridge.ErrorReadOnly},
		"write nested read-only":    {workspace.OpWriteFile, map[string]any{"path": filepath.Join(inner, "lib.go"), "content": "x"}, bridge.ErrorReadOnly},
		"delete folder holding one": {workspace.OpDeleteFile, map[string]any{"path": w.write}, bridge.ErrorReadOnly},
		"delete an allowed folder":  {workspace.OpDeleteFile, map[string]any{"path": inner}, bridge.ErrorReadOnly},
		"move an allowed folder":    {workspace.OpMoveFile, map[string]any{"path": w.read, "destination": filepath.Join(w.write, "docs")}, bridge.ErrorReadOnly},
		"delete outside":            {workspace.OpDeleteFile, map[string]any{"path": filepath.Join(w.outside, "key.txt")}, bridge.ErrorOutsideFolder},
		"move outside":              {workspace.OpMoveFile, map[string]any{"path": filepath.Join(w.write, "main.go"), "destination": filepath.Join(w.outside, "main.go")}, bridge.ErrorOutsideFolder},
		"move in from outside":      {workspace.OpMoveFile, map[string]any{"path": filepath.Join(w.outside, "key.txt"), "destination": filepath.Join(w.write, "key.txt")}, bridge.ErrorOutsideFolder},
		"edit outside":              {workspace.OpEditFile, map[string]any{"path": filepath.Join(w.outside, "key.txt"), "oldText": "secret", "newText": "x"}, bridge.ErrorOutsideFolder},
		"search outside":            {workspace.OpSearchFiles, map[string]any{"query": "secret", "path": w.outside}, bridge.ErrorOutsideFolder},
		"delete with dot-dot":       {workspace.OpDeleteFile, map[string]any{"path": filepath.Join(w.write, "..", "secrets")}, bridge.ErrorOutsideFolder},
	} {
		t.Run(name, func(t *testing.T) { wantError(t, call(t, w, tc.op, tc.args), tc.code) })
	}
	if data, _ := os.ReadFile(notes); string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("read-only file changed: %q", data)
	}
	for _, path := range []string{filepath.Join(inner, "lib.go"), filepath.Join(w.outside, "key.txt"), filepath.Join(w.write, "main.go")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func TestSearchNeverFollowsLinksOutOfItsFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	w := newWorkspace(t)
	mustWrite(t, filepath.Join(w.write, "a.txt"), "secret here too\n")
	if err := os.Symlink(w.outside, filepath.Join(w.write, "escape")); err != nil {
		t.Fatal(err)
	}
	result := call(t, w, workspace.OpSearchFiles, map[string]any{"query": "secret"})
	var found workspace.SearchFilesResult
	_ = json.Unmarshal(result.Result, &found)
	if !result.OK || len(found.Matches) != 1 || found.Matches[0].Path != filepath.Join(w.write, "a.txt") {
		t.Fatalf("search = %+v %+v", result.Error, found)
	}
	result = call(t, w, workspace.OpListDirectory, map[string]any{"depth": 8})
	var listed workspace.ListDirectoryResult
	_ = json.Unmarshal(result.Result, &listed)
	if !result.OK || len(listed.Entries) != 2 || listed.Entries[1].Type != workspace.EntrySymlink {
		t.Fatalf("list = %+v %+v", result.Error, listed)
	}
	// Deleting the link removes the link, not what it points to.
	if result := call(t, w, workspace.OpDeleteFile, map[string]any{"path": filepath.Join(w.write, "escape")}); !result.OK {
		t.Fatalf("delete link: %+v", result.Error)
	}
	if _, err := os.Stat(filepath.Join(w.outside, "key.txt")); err != nil {
		t.Fatal("deleting a link removed its target")
	}
}

func TestListShowsEmptyFolders(t *testing.T) {
	w := newWorkspace(t)
	if err := os.MkdirAll(filepath.Join(w.write, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := call(t, w, workspace.OpListDirectory, map[string]any{})
	var listed workspace.ListDirectoryResult
	_ = json.Unmarshal(result.Result, &listed)
	if !result.OK || len(listed.Entries) != 1 || listed.Entries[0].Path != filepath.Join(w.write, "empty") || listed.Entries[0].Type != workspace.EntryDirectory {
		t.Fatalf("list = %+v %+v", result.Error, listed)
	}
}

func TestSearchStopsAtTheDeadline(t *testing.T) {
	w := newWorkspace(t)
	mustWrite(t, filepath.Join(w.write, "a.txt"), "needle\n")
	// Past the point where the bridge must answer, but not yet cancelled.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(deadlineMargin-time.Second))
	defer cancel()
	found, err := machine{allowed: resolveFolders(w.folders)}.search(ctx, map[string]any{"query": "needle"})
	if err != nil || !found.Truncated || len(found.Matches) != 0 {
		t.Fatalf("search = %+v, %v", found, err)
	}
}

func TestAncestorIsComparesFoldersOnDisk(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(root, "proj")
	other := filepath.Join(root, "Proj")
	for _, dir := range []string{allowed, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !ancestorIs(allowed, filepath.Join(allowed, "src", "new.go")) || !ancestorIs(allowed, allowed) {
		t.Fatal("paths in the folder must be inside it")
	}
	if a, _ := os.Stat(allowed); func() bool { b, _ := os.Stat(other); return os.SameFile(a, b) }() {
		t.Skip("this file system ignores case")
	}
	// On a case-sensitive volume, a folder differing only in case is a
	// different folder.
	if ancestorIs(allowed, filepath.Join(other, "secret.txt")) || ancestorIs(allowed, root) {
		t.Fatal("a different folder must not count as the allowed one")
	}
}

func TestWriteFileRefusesTooLarge(t *testing.T) {
	w := newWorkspace(t)
	wantError(t, call(t, w, workspace.OpWriteFile, map[string]any{"path": filepath.Join(w.write, "x"), "content": strings.Repeat("x", workspace.MaximumFileBytes+1)}), bridge.ErrorFailed)
}

func TestCallPastItsDeadlineIsNotRun(t *testing.T) {
	w := newWorkspace(t)
	raw, _ := json.Marshal(map[string]any{"path": filepath.Join(w.write, "late.txt"), "content": "x"})
	result := Execute(context.Background(), bridge.Call{ID: "c", Op: workspace.OpWriteFile, Args: raw, Folders: w.folders, DeadlineUnixMs: time.Now().Add(-time.Second).UnixMilli()})
	wantError(t, result, bridge.ErrorTimeout)
	if _, err := os.Stat(filepath.Join(w.write, "late.txt")); err == nil {
		t.Fatal("a late call ran")
	}
}
