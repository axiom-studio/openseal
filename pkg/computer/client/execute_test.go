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
)

// workspace is an allowed write folder, an allowed read-only folder and a
// folder outside both, all resolved.
type workspace struct {
	write, read, outside string
	folders              []bridge.Folder
}

func newWorkspace(t *testing.T) workspace {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := workspace{
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

func call(t *testing.T, w workspace, op string, args any) bridge.Result {
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
		args any
		code string
	}{
		{"dot-dot read", bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.write, "..", "secrets", "key.txt")}, bridge.ErrorOutsideFolder},
		{"relative dot-dot read", bridge.OpReadFile, bridge.ReadFileInput{Path: "../secrets/key.txt"}, bridge.ErrorOutsideFolder},
		{"absolute outside", bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.outside, "key.txt")}, bridge.ErrorOutsideFolder},
		{"dot-dot write", bridge.OpWriteFile, bridge.WriteFileInput{Path: filepath.Join(w.write, "..", "secrets", "x.txt"), Content: "x"}, bridge.ErrorOutsideFolder},
		{"list outside", bridge.OpListFiles, bridge.ListFilesInput{Path: w.outside}, bridge.ErrorOutsideFolder},
		{"write in read-only folder", bridge.OpWriteFile, bridge.WriteFileInput{Path: filepath.Join(w.read, "notes.txt"), Content: "x"}, bridge.ErrorReadOnly},
		{"command in read-only folder", bridge.OpRunCommand, bridge.RunCommandInput{Command: "true", Cwd: w.read}, bridge.ErrorReadOnly},
		{"command outside", bridge.OpRunCommand, bridge.RunCommandInput{Command: "true", Cwd: w.outside}, bridge.ErrorOutsideFolder},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, []struct {
			name string
			op   string
			args any
			code string
		}{
			{"symlinked folder read", bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.write, "escape", "key.txt")}, bridge.ErrorOutsideFolder},
			{"symlinked file read", bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.write, "key-link")}, bridge.ErrorOutsideFolder},
			{"symlinked folder write", bridge.OpWriteFile, bridge.WriteFileInput{Path: filepath.Join(w.write, "escape", "new.txt"), Content: "x"}, bridge.ErrorOutsideFolder},
			{"symlinked folder cwd", bridge.OpRunCommand, bridge.RunCommandInput{Command: "true", Cwd: filepath.Join(w.write, "escape")}, bridge.ErrorOutsideFolder},
			{"symlinked folder list", bridge.OpListFiles, bridge.ListFilesInput{Path: filepath.Join(w.write, "escape")}, bridge.ErrorOutsideFolder},
		}...)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, call(t, w, tc.op, tc.args), tc.code)
		})
	}

	if runtime.GOOS != "windows" {
		// A dangling link is refused rather than written through.
		result := call(t, w, bridge.OpWriteFile, bridge.WriteFileInput{Path: filepath.Join(w.write, "dangling"), Content: "x"})
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
	wantError(t, call(t, w, bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.read, "notes.txt")}), bridge.ErrorOutsideFolder)
	wantError(t, call(t, w, bridge.OpRunCommand, bridge.RunCommandInput{Command: "true"}), bridge.ErrorReadOnly)
}

func TestTildeExpandsToHome(t *testing.T) {
	w := newWorkspace(t)
	t.Setenv("HOME", w.write)
	t.Setenv("USERPROFILE", w.write)
	result := call(t, w, bridge.OpWriteFile, bridge.WriteFileInput{Path: "~/sub/hello.txt", Content: "hi"})
	if !result.OK {
		t.Fatalf("write: %+v", result.Error)
	}
	if data, err := os.ReadFile(filepath.Join(w.write, "sub", "hello.txt")); err != nil || string(data) != "hi" {
		t.Fatalf("file = %q, %v", data, err)
	}
	w.folders = []bridge.Folder{{Path: "~", Access: bridge.AccessRead}}
	result = call(t, w, bridge.OpReadFile, bridge.ReadFileInput{Path: "~/sub/hello.txt"})
	if !result.OK {
		t.Fatalf("read via ~ folder: %+v", result.Error)
	}
}

func TestWriteReadAndListInsideFolders(t *testing.T) {
	w := newWorkspace(t)
	target := filepath.Join(w.write, "a", "b.txt")
	if result := call(t, w, bridge.OpWriteFile, bridge.WriteFileInput{Path: target, Content: "line1\n"}); !result.OK {
		t.Fatalf("write: %+v", result.Error)
	}
	result := call(t, w, bridge.OpWriteFile, bridge.WriteFileInput{Path: target, Content: "line2\n", Append: true})
	var written bridge.WriteFileOutput
	_ = json.Unmarshal(result.Result, &written)
	if !result.OK || written.Bytes != 6 || written.Path != target {
		t.Fatalf("append = %+v %+v", result, written)
	}

	result = call(t, w, bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.read, "notes.txt"), Offset: 2, Limit: 1})
	var read bridge.ReadFileOutput
	_ = json.Unmarshal(result.Result, &read)
	if !result.OK || read.Content != "two\n" || read.TotalLines != 3 || !read.Truncated {
		t.Fatalf("read = %+v %+v", result.Error, read)
	}
	wantError(t, call(t, w, bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.write, "missing.txt")}), bridge.ErrorNotFound)

	result = call(t, w, bridge.OpListFiles, bridge.ListFilesInput{Path: w.write})
	var listed bridge.ListFilesOutput
	_ = json.Unmarshal(result.Result, &listed)
	if !result.OK || len(listed.Entries) != 1 || listed.Entries[0].Type != bridge.EntryDir {
		t.Fatalf("depth 1 list = %+v %+v", result.Error, listed)
	}
	result = call(t, w, bridge.OpListFiles, bridge.ListFilesInput{Path: w.write, Depth: 2})
	listed = bridge.ListFilesOutput{}
	_ = json.Unmarshal(result.Result, &listed)
	if !result.OK || len(listed.Entries) != 2 || listed.Entries[1].Path != target || listed.Entries[1].Size != 12 {
		t.Fatalf("depth 2 list = %+v %+v", result.Error, listed)
	}
}

func TestReadFileCapsContent(t *testing.T) {
	w := newWorkspace(t)
	big := strings.Repeat("x", bridge.MaxReadFileBytes+100)
	mustWrite(t, filepath.Join(w.write, "big.txt"), big+"\nend\n")
	result := call(t, w, bridge.OpReadFile, bridge.ReadFileInput{Path: filepath.Join(w.write, "big.txt")})
	var read bridge.ReadFileOutput
	_ = json.Unmarshal(result.Result, &read)
	if !result.OK || len(read.Content) != bridge.MaxReadFileBytes || !read.Truncated || read.TotalLines != 2 {
		t.Fatalf("read = %+v len %d truncated %v lines %d", result.Error, len(read.Content), read.Truncated, read.TotalLines)
	}
}

func TestReadFileStopsCountingAtTheDeadline(t *testing.T) {
	w := newWorkspace(t)
	path := filepath.Join(w.write, "huge.log")
	mustWrite(t, path, strings.Repeat("line\n", 10*readCheckEvery))
	// Past the point where the bridge must answer, but not yet cancelled.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(deadlineMargin-time.Second))
	defer cancel()

	read, cerr := readFile(ctx, resolveFolders(w.folders), bridge.ReadFileInput{Path: path, Limit: 10})
	if cerr != nil || read.Content != strings.Repeat("line\n", 10) || !read.Truncated || read.TotalLines >= 10*readCheckEvery {
		t.Fatalf("read = %+v, %v", read, cerr)
	}

	_, cerr = readFile(ctx, resolveFolders(w.folders), bridge.ReadFileInput{Path: path, Limit: 5 * readCheckEvery})
	if cerr == nil || cerr.Code != bridge.ErrorTimeout {
		t.Fatalf("an unfinished window must time out, got %v", cerr)
	}
}

func TestWriteFileRefusesTooLarge(t *testing.T) {
	w := newWorkspace(t)
	wantError(t, call(t, w, bridge.OpWriteFile, bridge.WriteFileInput{Path: filepath.Join(w.write, "x"), Content: strings.Repeat("x", bridge.MaxWriteBytes+1)}), bridge.ErrorTooLarge)
}

func TestCallPastItsDeadlineIsNotRun(t *testing.T) {
	w := newWorkspace(t)
	raw, _ := json.Marshal(bridge.WriteFileInput{Path: filepath.Join(w.write, "late.txt"), Content: "x"})
	result := Execute(context.Background(), bridge.Call{ID: "c", Op: bridge.OpWriteFile, Args: raw, Folders: w.folders, DeadlineUnixMs: time.Now().Add(-time.Second).UnixMilli()})
	wantError(t, result, bridge.ErrorTimeout)
	if _, err := os.Stat(filepath.Join(w.write, "late.txt")); err == nil {
		t.Fatal("a late call ran")
	}
}

func TestTailKeepsTheEndAsValidUTF8(t *testing.T) {
	out := &tail{limit: 4}
	_, _ = out.Write([]byte("abc"))
	_, _ = out.Write([]byte("dé€"))
	// "abcdé€" is 4+2+3 bytes; the last 4 are é's second byte and €.
	if got := out.String(); got != "€" || !out.truncated {
		t.Fatalf("tail = %q truncated %v", got, out.truncated)
	}
}
