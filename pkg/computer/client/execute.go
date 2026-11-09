package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/axiom-studio/openseal/pkg/chatpath"
	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Execute runs one call and returns its result. It never panics on bad
// input: every failure is a CallError the model can read. The ops are the
// machine operations of pkg/workspace, with the same arguments, results and
// failure texts as a chat's secure VM; paths are this computer's, inside the
// call's folders.
func Execute(ctx context.Context, call bridge.Call) bridge.Result {
	if call.DeadlineUnixMs > 0 {
		deadline := call.Deadline()
		if !time.Now().Before(deadline) {
			return failed(call.ID, callError(bridge.ErrorTimeout, "The call arrived after HelloSeal stopped waiting for it."))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	args := map[string]interface{}{}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return failed(call.ID, callError(bridge.ErrorFailed, "The call's arguments are not valid: "+err.Error()))
		}
	}
	m := machine{allowed: resolveFolders(call.Folders)}
	var (
		output any
		err    error
	)
	switch call.Op {
	case workspace.OpListDirectory:
		output, err = m.list(ctx, args)
	case workspace.OpReadFile:
		output, err = m.read(ctx, args)
	case workspace.OpSearchFiles:
		output, err = m.search(ctx, args)
	case workspace.OpWriteFile:
		output, err = m.write(args)
	case workspace.OpEditFile:
		output, err = m.edit(args)
	case workspace.OpDeleteFile:
		output, err = m.remove(ctx, args)
	case workspace.OpMoveFile:
		output, err = m.move(ctx, args)
	case workspace.OpRunCommand:
		output, err = runCommand(ctx, m.allowed, args)
	default:
		err = callError(bridge.ErrorFailed, fmt.Sprintf("This version of helloseal cannot run %q. Update it and run helloseal start again.", call.Op))
	}
	if err != nil {
		return failed(call.ID, asCallError(err))
	}
	encoded, marshalErr := json.Marshal(output)
	if marshalErr != nil {
		return failed(call.ID, callError(bridge.ErrorFailed, "Could not encode the result."))
	}
	return bridge.Result{ID: call.ID, OK: true, Result: encoded}
}

func failed(id string, err *bridge.CallError) bridge.Result {
	return bridge.Result{ID: id, Error: err}
}

// asCallError carries an operation's failure to HelloSeal with the text a
// chat would show.
func asCallError(err error) *bridge.CallError {
	var callErr *bridge.CallError
	switch {
	case errors.As(err, &callErr):
		return callErr
	case errors.Is(err, workspace.ErrNotFound):
		return callError(bridge.ErrorNotFound, err.Error())
	default:
		return callError(bridge.ErrorFailed, err.Error())
	}
}

// fileError turns an os error into a CallError without exposing more than
// the path the model already named.
func fileError(shown string, err error) *bridge.CallError {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return asCallError(workspace.NotFound(shown))
	case errors.Is(err, fs.ErrPermission):
		return callError(bridge.ErrorFailed, "Permission denied for "+shown+".")
	default:
		return callError(bridge.ErrorFailed, fmt.Sprintf("%s: %v", shown, err))
	}
}

// machine runs the machine operations inside a call's folders.
type machine struct {
	allowed folders
}

// pathArgument is a path argument, the default folder when it is left out.
func pathArgument(args map[string]interface{}, key string) string {
	if raw := workspace.Text(args, key); strings.TrimSpace(raw) != "" {
		return raw
	}
	return "."
}

// answerBy is when an operation that walks folders stops to answer in time.
func answerBy(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline.Add(-deadlineMargin)
	}
	return time.Time{}
}

func late(ctx context.Context, stopAt time.Time) bool {
	return ctx.Err() != nil || !stopAt.IsZero() && !time.Now().Before(stopAt)
}

// mediaTypeOf is a regular file's media type, as every machine gives it
// (workspace.MachineMediaType): from its name, else from its first bytes.
func mediaTypeOf(path string) string {
	name := filepath.Base(path)
	if known, ok := chatpath.KnownMediaType(name); ok {
		return known
	}
	head, _ := readPrefix(path, workspace.SniffBytes)
	return workspace.MachineMediaType(name, head)
}

// openRegular opens a regular file to read without ever blocking: a named
// pipe, socket or device is refused instead of waiting for a writer.
func openRegular(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|nonBlocking, 0)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errNotRegular
		}
		return nil, err
	}
	return file, nil
}

var errNotRegular = errors.New("not a regular file")

// notRegular refuses a path that is not a regular file or a directory.
func notRegular(path string) *bridge.CallError {
	return callError(bridge.ErrorFailed, path+" is not a regular file.")
}

// children are a directory's entries in the order of their paths (see
// workspace.ListOrder).
func children(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	sort.SliceStable(entries, func(i, j int) bool {
		return workspace.ListOrder(entries[i].Name(), isDir(entries[i])) < workspace.ListOrder(entries[j].Name(), isDir(entries[j]))
	})
	return entries, err
}

// isDir is a real directory, not a link to one.
func isDir(entry fs.DirEntry) bool { return entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 }

func (m machine) list(ctx context.Context, args map[string]interface{}) (workspace.ListDirectoryResult, error) {
	root, cerr := m.allowed.resolve(pathArgument(args, "path"), false)
	if cerr != nil {
		return workspace.ListDirectoryResult{}, cerr
	}
	info, err := os.Stat(root)
	if err != nil {
		return workspace.ListDirectoryResult{}, fileError(root, err)
	}
	if !info.IsDir() {
		return workspace.ListDirectoryResult{}, workspace.IsAFile(root)
	}
	depth := workspace.Integer(args, "depth", workspace.DefaultListDepth, workspace.MaximumListDepth)
	listing := workspace.NewListing(func(parts []string) string { return filepath.Join(append([]string{root}, parts...)...) },
		depth, workspace.Integer(args, "maxEntries", workspace.DefaultListEntries, workspace.MaximumListEntries))
	stopAt := answerBy(ctx)
	// walk adds the leaves below dir: files, symlinks (never followed),
	// empty directories, and directories at the depth limit.
	var walk func(dir string, parts []string) bool
	walk = func(dir string, parts []string) bool {
		entries, _ := children(dir)
		for _, entry := range entries {
			if late(ctx, stopAt) {
				listing.Truncate()
				return false
			}
			here := append(append([]string{}, parts...), entry.Name())
			path := filepath.Join(dir, entry.Name())
			switch {
			case entry.Type()&fs.ModeSymlink != 0:
				if !listing.Add(here, workspace.Entry{Type: workspace.EntrySymlink}) {
					return false
				}
			case entry.IsDir():
				if len(here) < depth {
					if inner, err := os.ReadDir(path); err == nil && len(inner) > 0 {
						if !walk(path, here) {
							return false
						}
						continue
					}
				}
				if !listing.Add(here, workspace.Entry{Type: workspace.EntryDirectory}) {
					return false
				}
			case entry.Type().IsRegular():
				leaf := workspace.Entry{Type: workspace.EntryFile, Size: workspace.FileSize(0), MediaType: mediaTypeOf(path)}
				if info, err := entry.Info(); err == nil {
					leaf.Size = workspace.FileSize(info.Size())
				}
				if !listing.Add(here, leaf) {
					return false
				}
			}
			// A named pipe, socket or device is no file a machine
			// operation reads, and a chat never holds one: it is left out.
		}
		return true
	}
	walk(root, nil)
	return listing.Result(), nil
}

func (m machine) read(ctx context.Context, args map[string]interface{}) (workspace.ReadFileResult, error) {
	path, cerr := m.allowed.resolve(workspace.Text(args, "path"), false)
	if cerr != nil {
		return workspace.ReadFileResult{}, cerr
	}
	info, err := os.Stat(path)
	if err != nil {
		return workspace.ReadFileResult{}, fileError(path, err)
	}
	if info.IsDir() {
		return workspace.ReadFileResult{}, workspace.IsADirectory(path)
	}
	if !info.Mode().IsRegular() {
		return workspace.ReadFileResult{}, notRegular(path)
	}
	result, err := workspace.ReadContent(ctx, path, mediaTypeOf(path), info.Size(), workspace.Integer(args, "maxBytes", workspace.DefaultReadBytes, workspace.MaximumReadBytes),
		func(limit int64) ([]byte, error) { return readPrefix(path, limit) })
	if err != nil {
		return workspace.ReadFileResult{}, fileError(path, err)
	}
	return result, nil
}

// readPrefix reads at most limit bytes from the start of a file.
func readPrefix(path string, limit int64) ([]byte, error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

func (m machine) search(ctx context.Context, args map[string]interface{}) (workspace.SearchFilesResult, error) {
	search, err := workspace.NewSearch(args)
	if err != nil {
		return workspace.SearchFilesResult{}, err
	}
	root, cerr := m.allowed.resolve(pathArgument(args, "path"), false)
	if cerr != nil {
		return workspace.SearchFilesResult{}, cerr
	}
	// A glob matches a file's name or its path from the top of its allowed
	// folder, as a chat's glob matches paths from the top of its files.
	top, _ := m.allowed.containing(root)
	stopAt := answerBy(ctx)
	scan := func(path string, size int64) bool {
		rel, err := filepath.Rel(top.path, path)
		if err != nil || !search.Wants(filepath.ToSlash(rel)) || !workspace.Searchable(mediaTypeOf(path), size) {
			return true
		}
		data, err := readPrefix(path, workspace.SearchFileBytes)
		if err != nil {
			return true
		}
		return search.Scan(path, data)
	}
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, _ := children(dir)
		for _, entry := range entries {
			if late(ctx, stopAt) {
				search.Truncate()
				return false
			}
			path := filepath.Join(dir, entry.Name())
			switch {
			case entry.Type()&fs.ModeSymlink != 0:
			case entry.IsDir():
				if !walk(path) {
					return false
				}
			case entry.Type().IsRegular():
				info, err := entry.Info()
				if err == nil && !scan(path, info.Size()) {
					return false
				}
			}
		}
		return true
	}
	// Like a chat's, a search below a path that names nothing finds nothing.
	if info, err := os.Stat(root); err == nil {
		if info.IsDir() {
			walk(root)
		} else if info.Mode().IsRegular() {
			scan(root, info.Size())
		}
	}
	return search.Result(), nil
}

func (m machine) write(args map[string]interface{}) (workspace.ChangeResult, error) {
	path, cerr := m.allowed.resolve(workspace.Text(args, "path"), true)
	if cerr != nil {
		return workspace.ChangeResult{}, cerr
	}
	content, err := workspace.WriteContent(args)
	if err != nil {
		return workspace.ChangeResult{}, err
	}
	if _, err := workspace.WrittenMediaType(path, filepath.Base(path), content); err != nil {
		return workspace.ChangeResult{}, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return workspace.ChangeResult{}, workspace.IsADirectory(path)
	} else if err == nil && !info.Mode().IsRegular() {
		return workspace.ChangeResult{}, notRegular(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	if err := writeFile(path, content); err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	return workspace.ChangeResult{Success: true, ChangedPaths: []string{path}}, nil
}

// writeFile replaces a file's content, keeping its mode when it exists. It
// never blocks on a named pipe that took the file's place.
func writeFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|nonBlocking, 0o644)
	if err != nil {
		return err
	}
	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (m machine) edit(args map[string]interface{}) (workspace.ChangeResult, error) {
	path, cerr := m.allowed.resolve(workspace.Text(args, "path"), true)
	if cerr != nil {
		return workspace.ChangeResult{}, cerr
	}
	edit, err := workspace.NewEdit(args)
	if err != nil {
		return workspace.ChangeResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	if info.IsDir() {
		return workspace.ChangeResult{}, workspace.IsADirectory(path)
	}
	if !info.Mode().IsRegular() {
		return workspace.ChangeResult{}, notRegular(path)
	}
	if !chatpath.Textual(mediaTypeOf(path)) {
		return workspace.ChangeResult{}, workspace.NotText(path)
	}
	if info.Size() > workspace.MaximumFileBytes {
		return workspace.ChangeResult{}, workspace.TooLargeToEdit(path)
	}
	data, err := readPrefix(path, workspace.MaximumFileBytes+1)
	if err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	content, replacements, err := edit.Apply(path, string(data))
	if err != nil {
		return workspace.ChangeResult{}, err
	}
	if err := writeFile(path, content); err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	return workspace.ChangeResult{Success: true, ChangedPaths: []string{path}, Replacements: replacements}, nil
}

// leaves are the files and links at or below path, in path order: what
// deleting or moving path changes. An empty directory is its own leaf.
func leaves(ctx context.Context, path string) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var found []string
	err = filepath.WalkDir(path, func(at string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.IsDir() {
			found = append(found, at)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		found = []string{path}
	}
	sort.Strings(found)
	return found, nil
}

func (m machine) remove(ctx context.Context, args map[string]interface{}) (workspace.ChangeResult, error) {
	path, cerr := m.allowed.resolveEntry(workspace.Text(args, "path"))
	if cerr != nil {
		return workspace.ChangeResult{}, cerr
	}
	removed, err := leaves(ctx, path)
	if err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return workspace.ChangeResult{}, fileError(path, err)
	}
	return workspace.ChangeResult{Success: true, ChangedPaths: removed}, nil
}

func (m machine) move(ctx context.Context, args map[string]interface{}) (workspace.ChangeResult, error) {
	from, cerr := m.allowed.resolveEntry(workspace.Text(args, "path"))
	if cerr != nil {
		return workspace.ChangeResult{}, cerr
	}
	to, cerr := m.allowed.resolveEntry(workspace.Text(args, "destination"))
	if cerr != nil {
		return workspace.ChangeResult{}, cerr
	}
	if from == to || lexicallyWithin(from, to) {
		return workspace.ChangeResult{}, workspace.MoveIntoItself(from)
	}
	moved, err := leaves(ctx, from)
	if err != nil {
		return workspace.ChangeResult{}, fileError(from, err)
	}
	if _, err := os.Lstat(to); err == nil {
		return workspace.ChangeResult{}, workspace.AlreadyExists(to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return workspace.ChangeResult{}, fileError(to, err)
	}
	if err := os.Rename(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return workspace.ChangeResult{}, callError(bridge.ErrorFailed, fmt.Sprintf("%s and %s are on different drives, so it cannot be moved; copy it with run_command instead.", from, to))
		}
		return workspace.ChangeResult{}, fileError(from, err)
	}
	changed := make([]string, 0, 2*len(moved))
	for _, source := range moved {
		changed = append(changed, source, to+strings.TrimPrefix(source, from))
	}
	sort.Strings(changed)
	return workspace.ChangeResult{Success: true, ChangedPaths: changed}, nil
}
