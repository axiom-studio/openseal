// Package workspacetest holds every machine to the machine operations'
// contract (pkg/workspace machine.go): a chat's files, a chat's sandbox and
// a person's computer must answer the same calls with the same results and
// the same failures, differing only in how paths look.
package workspacetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Machine is one implementation of the machine operations, holding no files
// when the factory makes it.
type Machine interface {
	// Path is how the machine's arguments name rel, a slash path from the
	// top of its files ("" is the top).
	Path(rel string) string
	// Shown is how its results and failures show rel.
	Shown(rel string) string
	// Do runs one machine operation (a bare name, such as read_file). A
	// failure the model reads is an error whose text is that message.
	Do(ctx context.Context, op string, args map[string]interface{}) (map[string]interface{}, error)
}

// Options say what a machine runs.
type Options struct {
	// Commands runs run_command with a POSIX shell.
	Commands bool
	// Files runs the file operations.
	Files bool
}

// ChatExtras are the result fields a chat adds to the contract's (see
// workspace results.go): Conformance ignores them.
var ChatExtras = map[string]bool{"version": true, "changedPaths": true, "deletedPaths": true, "diskFull": true,
	"filesNotSaved": true, "tooLargeToSave": true, "recalculationFailed": true}

// Conformance runs the contract against fresh machines from factory.
func Conformance(t *testing.T, factory func(t *testing.T) Machine, options Options) {
	if options.Files {
		for name, check := range fileChecks {
			t.Run(name, func(t *testing.T) { check(t, &harness{t: t, m: factory(t)}) })
		}
	}
	if options.Commands {
		for name, check := range commandChecks {
			t.Run(name, func(t *testing.T) { check(t, &harness{t: t, m: factory(t)}) })
		}
	}
}

type harness struct {
	t *testing.T
	m Machine
}

func (h *harness) do(op string, args map[string]interface{}) (map[string]interface{}, error) {
	h.t.Helper()
	if !workspace.MachineOp(op) {
		h.t.Fatalf("%s is not a machine operation", op)
	}
	return h.m.Do(context.Background(), op, args)
}

// ok runs op and returns its result as JSON values.
func (h *harness) ok(op string, args map[string]interface{}) map[string]interface{} {
	h.t.Helper()
	result, err := h.do(op, args)
	if err != nil {
		h.t.Fatalf("%s %v: %v", op, args, err)
	}
	return normalize(h.t, result, op == workspace.OpRunCommand)
}

// want runs op and checks its result is want, ignoring chat extras.
func (h *harness) want(op string, args map[string]interface{}, want interface{}) {
	h.t.Helper()
	got := h.ok(op, args)
	expected := normalize(h.t, want, false)
	if !reflect.DeepEqual(got, expected) {
		h.t.Fatalf("%s %v\n got %s\nwant %s", op, args, encode(got), encode(expected))
	}
}

// fails runs op and checks it fails with message.
func (h *harness) fails(op string, args map[string]interface{}, message error) {
	h.t.Helper()
	result, err := h.do(op, args)
	if err == nil {
		h.t.Fatalf("%s %v = %v, want failure %q", op, args, result, message)
	}
	if err.Error() != message.Error() {
		h.t.Fatalf("%s %v failed with %q, want %q", op, args, err, message)
	}
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	h.want(workspace.OpWriteFile, map[string]interface{}{"path": h.m.Path(rel), "content": content},
		workspace.ChangeResult{Success: true, ChangedPaths: []string{h.m.Shown(rel)}})
}

func (h *harness) shown(rels ...string) []string {
	shown := make([]string, len(rels))
	for i, rel := range rels {
		shown[i] = h.m.Shown(rel)
	}
	return shown
}

func normalize(t *testing.T, value interface{}, command bool) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	delete(result, "version")
	if command {
		for name := range ChatExtras {
			delete(result, name)
		}
	}
	return result
}

func encode(value interface{}) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func file(path string, size int64, mediaType string) workspace.Entry {
	return workspace.Entry{Path: path, Type: workspace.EntryFile, Size: workspace.FileSize(size), MediaType: mediaType}
}

func directory(path string) workspace.Entry {
	return workspace.Entry{Path: path, Type: workspace.EntryDirectory}
}

var fileChecks = map[string]func(t *testing.T, h *harness){
	"write_file then read_file": func(t *testing.T, h *harness) {
		h.write("notes/plan.md", "# Plan\nship it\n")
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("notes/plan.md")},
			workspace.ReadFileResult{Path: h.m.Shown("notes/plan.md"), MediaType: "text/markdown", Size: 15, Content: "# Plan\nship it\n"})
		// A name without a type is text.
		h.write("Makefile", "all:\n\techo hi\n")
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("Makefile")},
			workspace.ReadFileResult{Path: h.m.Shown("Makefile"), MediaType: "text/plain", Size: 14, Content: "all:\n\techo hi\n"})
		// Writing again replaces the file.
		h.write("notes/plan.md", "v2")
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("notes/plan.md")},
			workspace.ReadFileResult{Path: h.m.Shown("notes/plan.md"), MediaType: "text/markdown", Size: 2, Content: "v2"})
	},
	"read_file cuts at maxBytes to a whole character": func(t *testing.T, h *harness) {
		h.write("accents.txt", "abcé and more")
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("accents.txt"), "maxBytes": 4},
			workspace.ReadFileResult{Path: h.m.Shown("accents.txt"), MediaType: "text/plain", Size: 14, Content: "abc", Truncated: true})
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("accents.txt"), "maxBytes": 5},
			workspace.ReadFileResult{Path: h.m.Shown("accents.txt"), MediaType: "text/plain", Size: 14, Content: "abcé", Truncated: true})
		big := strings.Repeat("0123456789", workspace.DefaultReadBytes/10+1)
		h.write("big.txt", big)
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("big.txt")},
			workspace.ReadFileResult{Path: h.m.Shown("big.txt"), MediaType: "text/plain", Size: int64(len(big)), Content: big[:workspace.DefaultReadBytes], Truncated: true})
	},
	"read_file fails on what is not a file": func(t *testing.T, h *harness) {
		h.write("dir/a.txt", "a")
		h.fails(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("missing.txt")}, workspace.NotFound(h.m.Shown("missing.txt")))
		h.fails(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("dir")}, workspace.IsADirectory(h.m.Shown("dir")))
	},
	"list_directory lists in path order to a depth": func(t *testing.T, h *harness) {
		h.write("a.txt", "1")
		h.write("a/b.txt", "22")
		h.write("a/c/d.txt", "333")
		h.write("a-b.txt", "4444")
		h.write("z.md", "55555")
		h.want(workspace.OpListDirectory, map[string]interface{}{}, workspace.ListDirectoryResult{Entries: []workspace.Entry{
			file(h.m.Shown("a-b.txt"), 4, "text/plain"), file(h.m.Shown("a.txt"), 1, "text/plain"), directory(h.m.Shown("a")),
			file(h.m.Shown("a/b.txt"), 2, "text/plain"), directory(h.m.Shown("a/c")), file(h.m.Shown("z.md"), 5, "text/markdown"),
		}})
		h.want(workspace.OpListDirectory, map[string]interface{}{"depth": 8}, workspace.ListDirectoryResult{Entries: []workspace.Entry{
			file(h.m.Shown("a-b.txt"), 4, "text/plain"), file(h.m.Shown("a.txt"), 1, "text/plain"), directory(h.m.Shown("a")),
			file(h.m.Shown("a/b.txt"), 2, "text/plain"), directory(h.m.Shown("a/c")), file(h.m.Shown("a/c/d.txt"), 3, "text/plain"),
			file(h.m.Shown("z.md"), 5, "text/markdown"),
		}})
		h.want(workspace.OpListDirectory, map[string]interface{}{"path": h.m.Path("a"), "depth": 1}, workspace.ListDirectoryResult{Entries: []workspace.Entry{
			file(h.m.Shown("a/b.txt"), 2, "text/plain"), directory(h.m.Shown("a/c")),
		}})
		h.want(workspace.OpListDirectory, map[string]interface{}{"maxEntries": 2}, workspace.ListDirectoryResult{Entries: []workspace.Entry{
			file(h.m.Shown("a-b.txt"), 4, "text/plain"), file(h.m.Shown("a.txt"), 1, "text/plain"),
		}, Truncated: true})
		h.fails(workspace.OpListDirectory, map[string]interface{}{"path": h.m.Path("z.md")}, workspace.IsAFile(h.m.Shown("z.md")))
		h.fails(workspace.OpListDirectory, map[string]interface{}{"path": h.m.Path("nowhere")}, workspace.NotFound(h.m.Shown("nowhere")))
	},
	"search_files finds lines": func(t *testing.T, h *harness) {
		h.write("src/main.go", "package main\n// TODO: ship\nfunc main() {}\n")
		h.write("src/util/util.go", "package util\n// todo lower\n// TODO: test\n")
		h.write("README.md", "# TODO list\n")
		all := []workspace.SearchMatch{
			{Path: h.m.Shown("README.md"), Line: 1, Text: "# TODO list"},
			{Path: h.m.Shown("src/main.go"), Line: 2, Text: "// TODO: ship"},
			{Path: h.m.Shown("src/util/util.go"), Line: 3, Text: "// TODO: test"},
		}
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO"}, workspace.SearchFilesResult{Matches: all})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO", "maxResults": 2}, workspace.SearchFilesResult{Matches: all[:2], Truncated: true})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO", "path": h.m.Path("src/util")}, workspace.SearchFilesResult{Matches: all[2:]})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO", "glob": "*.go"}, workspace.SearchFilesResult{Matches: all[1:]})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO", "glob": "src/*.go"}, workspace.SearchFilesResult{Matches: all[1:2]})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": `(?i)^// todo`, "regex": true}, workspace.SearchFilesResult{Matches: []workspace.SearchMatch{
			{Path: h.m.Shown("src/main.go"), Line: 2, Text: "// TODO: ship"},
			{Path: h.m.Shown("src/util/util.go"), Line: 2, Text: "// todo lower"},
			{Path: h.m.Shown("src/util/util.go"), Line: 3, Text: "// TODO: test"},
		}})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "nothing like this"}, workspace.SearchFilesResult{Matches: []workspace.SearchMatch{}})
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "TODO", "path": h.m.Path("nowhere")}, workspace.SearchFilesResult{Matches: []workspace.SearchMatch{}})
		h.fails(workspace.OpSearchFiles, map[string]interface{}{"query": "(", "regex": true}, fmt.Errorf("%w: invalid search regular expression", workspace.ErrInvalidInput))
		h.fails(workspace.OpSearchFiles, map[string]interface{}{}, fmt.Errorf("%w: query is required", workspace.ErrInvalidInput))
	},
	"search_files cuts long lines": func(t *testing.T, h *harness) {
		line := strings.Repeat("é", workspace.SearchLineBytes) // two bytes each
		h.write("long.txt", "needle "+line+"\n")
		h.want(workspace.OpSearchFiles, map[string]interface{}{"query": "needle"}, workspace.SearchFilesResult{Matches: []workspace.SearchMatch{
			{Path: h.m.Shown("long.txt"), Line: 1, Text: ("needle " + line)[:workspace.SearchLineBytes-1]},
		}})
	},
	"write_file refuses what is not text": func(t *testing.T, h *harness) {
		h.fails(workspace.OpWriteFile, map[string]interface{}{"path": h.m.Path("logo.png"), "content": "x"},
			fmt.Errorf("%w: %s is a image/png file; write_file writes text files only", workspace.ErrInvalidInput, h.m.Shown("logo.png")))
		h.fails(workspace.OpWriteFile, map[string]interface{}{"path": h.m.Path("huge.txt"), "content": strings.Repeat("x", workspace.MaximumFileBytes+1)},
			fmt.Errorf("%w: a written file holds at most %d bytes", workspace.ErrInvalidInput, workspace.MaximumFileBytes))
		h.fails(workspace.OpWriteFile, map[string]interface{}{"path": h.m.Path("empty.txt")}, fmt.Errorf("%w: content is required", workspace.ErrInvalidInput))
	},
	"edit_file replaces exact text": func(t *testing.T, h *harness) {
		h.write("app.ts", "let a = 1\nlet b = 1\n")
		path := h.m.Path("app.ts")
		h.want(workspace.OpEditFile, map[string]interface{}{"path": path, "oldText": "let a = 1", "newText": "let a = 2"},
			workspace.ChangeResult{Success: true, ChangedPaths: h.shown("app.ts"), Replacements: 1})
		h.want(workspace.OpReadFile, map[string]interface{}{"path": path},
			workspace.ReadFileResult{Path: h.m.Shown("app.ts"), MediaType: "text/typescript", Size: 20, Content: "let a = 2\nlet b = 1\n"})
	},
	"edit_file needs a unique match unless replaceAll": func(t *testing.T, h *harness) {
		h.write("app.ts", "x = 1\ny = 1\nz = 1\n")
		path := h.m.Path("app.ts")
		h.fails(workspace.OpEditFile, map[string]interface{}{"path": path, "oldText": "= 1", "newText": "= 2"},
			fmt.Errorf("%w: oldText appears 3 times in %s; include more surrounding text to make it unique, or set replaceAll", workspace.ErrInvalidInput, h.m.Shown("app.ts")))
		h.fails(workspace.OpEditFile, map[string]interface{}{"path": path, "oldText": "= 9", "newText": "= 2"},
			fmt.Errorf("%w: oldText was not found in %s; read the file and copy the exact text, including whitespace", workspace.ErrInvalidInput, h.m.Shown("app.ts")))
		h.fails(workspace.OpEditFile, map[string]interface{}{"path": path, "oldText": "x", "newText": "x"},
			fmt.Errorf("%w: oldText and newText are the same", workspace.ErrInvalidInput))
		h.fails(workspace.OpEditFile, map[string]interface{}{"path": path, "newText": "x"},
			fmt.Errorf("%w: oldText is required; use write_file to create a file", workspace.ErrInvalidInput))
		h.want(workspace.OpEditFile, map[string]interface{}{"path": path, "oldText": "= 1", "newText": "= 2", "replaceAll": true},
			workspace.ChangeResult{Success: true, ChangedPaths: h.shown("app.ts"), Replacements: 3})
		h.want(workspace.OpReadFile, map[string]interface{}{"path": path},
			workspace.ReadFileResult{Path: h.m.Shown("app.ts"), MediaType: "text/typescript", Size: 18, Content: "x = 2\ny = 2\nz = 2\n"})
		h.fails(workspace.OpEditFile, map[string]interface{}{"path": h.m.Path("gone.ts"), "oldText": "a", "newText": "b"}, workspace.NotFound(h.m.Shown("gone.ts")))
	},
	"delete_file removes a file or a directory": func(t *testing.T, h *harness) {
		h.write("keep.txt", "k")
		h.write("old/a.txt", "a")
		h.write("old/sub/b.txt", "b")
		h.want(workspace.OpDeleteFile, map[string]interface{}{"path": h.m.Path("keep.txt")}, workspace.ChangeResult{Success: true, ChangedPaths: h.shown("keep.txt")})
		h.want(workspace.OpDeleteFile, map[string]interface{}{"path": h.m.Path("old")}, workspace.ChangeResult{Success: true, ChangedPaths: h.shown("old/a.txt", "old/sub/b.txt")})
		h.fails(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("old/a.txt")}, workspace.NotFound(h.m.Shown("old/a.txt")))
		h.fails(workspace.OpDeleteFile, map[string]interface{}{"path": h.m.Path("old")}, workspace.NotFound(h.m.Shown("old")))
	},
	"move_file renames a file or a directory": func(t *testing.T, h *harness) {
		h.write("draft.md", "d")
		h.write("src/a.txt", "a")
		h.write("src/b/c.txt", "c")
		h.write("taken.md", "t")
		h.want(workspace.OpMoveFile, map[string]interface{}{"path": h.m.Path("draft.md"), "destination": h.m.Path("docs/final.md")},
			workspace.ChangeResult{Success: true, ChangedPaths: h.shown("docs/final.md", "draft.md")})
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("docs/final.md")},
			workspace.ReadFileResult{Path: h.m.Shown("docs/final.md"), MediaType: "text/markdown", Size: 1, Content: "d"})
		h.want(workspace.OpMoveFile, map[string]interface{}{"path": h.m.Path("src"), "destination": h.m.Path("lib")},
			workspace.ChangeResult{Success: true, ChangedPaths: h.shown("lib/a.txt", "lib/b/c.txt", "src/a.txt", "src/b/c.txt")})
		h.fails(workspace.OpMoveFile, map[string]interface{}{"path": h.m.Path("lib"), "destination": h.m.Path("lib/inner")}, workspace.MoveIntoItself(h.m.Shown("lib")))
		h.fails(workspace.OpMoveFile, map[string]interface{}{"path": h.m.Path("docs/final.md"), "destination": h.m.Path("taken.md")}, workspace.AlreadyExists(h.m.Shown("taken.md")))
		h.fails(workspace.OpMoveFile, map[string]interface{}{"path": h.m.Path("draft.md"), "destination": h.m.Path("x.md")}, workspace.NotFound(h.m.Shown("draft.md")))
	},
}

var commandChecks = map[string]func(t *testing.T, h *harness){
	"run_command returns status and output": func(t *testing.T, h *harness) {
		got := h.ok(workspace.OpRunCommand, map[string]interface{}{"command": "printf out; printf err >&2; exit 3"})
		checkCommand(t, got, workspace.CommandResult{ExitStatus: 3, Stdout: "out", Stderr: "err"})
		got = h.ok(workspace.OpRunCommand, map[string]interface{}{"command": "true"})
		checkCommand(t, got, workspace.CommandResult{})
	},
	"run_command keeps the end of long output": func(t *testing.T, h *harness) {
		got := h.ok(workspace.OpRunCommand, map[string]interface{}{"command": "i=0; while [ $i -lt 2000 ]; do printf '%080d\\n' $i; i=$((i+1)); done; printf END"})
		stdout, _ := got["stdout"].(string)
		if len(stdout) != workspace.CommandOutputBytes || !strings.HasSuffix(stdout, "\nEND") {
			t.Fatalf("stdout is %d bytes ending %q", len(stdout), stdout[max(0, len(stdout)-10):])
		}
		got["stdout"] = ""
		checkCommand(t, got, workspace.CommandResult{Truncated: true})
	},
	"run_command stops at its timeout": func(t *testing.T, h *harness) {
		got := h.ok(workspace.OpRunCommand, map[string]interface{}{"command": "echo started; sleep 30", "timeoutSeconds": 1})
		checkCommand(t, got, workspace.CommandResult{ExitStatus: workspace.ExitTimedOut, Stdout: "started\n", Stderr: workspace.StoppedNote(1), TimedOut: true})
	},
	"run_command works in the machine's files": func(t *testing.T, h *harness) {
		h.write("sub/in.txt", "hello")
		got := h.ok(workspace.OpRunCommand, map[string]interface{}{"command": "cat in.txt; printf made > out.txt", "workingDirectory": h.m.Path("sub")})
		checkCommand(t, got, workspace.CommandResult{Stdout: "hello"})
		h.want(workspace.OpReadFile, map[string]interface{}{"path": h.m.Path("sub/out.txt")},
			workspace.ReadFileResult{Path: h.m.Shown("sub/out.txt"), MediaType: "text/plain", Size: 4, Content: "made"})
	},
}

// checkCommand compares a command's result with want, but only checks that
// durationMs is a count of milliseconds.
func checkCommand(t *testing.T, got map[string]interface{}, want workspace.CommandResult) {
	t.Helper()
	duration, ok := got["durationMs"].(float64)
	if !ok || duration < 0 {
		t.Fatalf("durationMs = %v", got["durationMs"])
	}
	got["durationMs"] = float64(0)
	expected := normalize(t, want, true)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("run_command\n got %s\nwant %s", encode(got), encode(expected))
	}
}
