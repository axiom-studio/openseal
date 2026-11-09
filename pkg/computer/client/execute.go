package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// Execute runs one call and returns its result. It never panics on bad
// input: every failure is a CallError the model can read.
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
	allowed := resolveFolders(call.Folders)
	var (
		output any
		err    *bridge.CallError
	)
	switch call.Op {
	case bridge.OpRunCommand:
		var input bridge.RunCommandInput
		if err = decodeArgs(call.Args, &input); err == nil {
			output, err = runCommand(ctx, allowed, input)
		}
	case bridge.OpReadFile:
		var input bridge.ReadFileInput
		if err = decodeArgs(call.Args, &input); err == nil {
			output, err = readFile(allowed, input)
		}
	case bridge.OpWriteFile:
		var input bridge.WriteFileInput
		if err = decodeArgs(call.Args, &input); err == nil {
			output, err = writeFile(allowed, input)
		}
	case bridge.OpListFiles:
		var input bridge.ListFilesInput
		if err = decodeArgs(call.Args, &input); err == nil {
			output, err = listFiles(ctx, allowed, input)
		}
	default:
		err = callError(bridge.ErrorFailed, fmt.Sprintf("This version of helloseal cannot run %q. Update it and run helloseal start again.", call.Op))
	}
	if err != nil {
		return failed(call.ID, err)
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

func decodeArgs(raw json.RawMessage, into any) *bridge.CallError {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return callError(bridge.ErrorFailed, "The call's arguments are not valid: "+err.Error())
	}
	return nil
}

// fileError turns an os error into a CallError without exposing more than
// the path the model already named.
func fileError(raw string, err error) *bridge.CallError {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return callError(bridge.ErrorNotFound, raw+" does not exist.")
	case errors.Is(err, fs.ErrPermission):
		return callError(bridge.ErrorFailed, "Permission denied for "+raw+".")
	default:
		return callError(bridge.ErrorFailed, fmt.Sprintf("%s: %v", raw, err))
	}
}

func readFile(allowed folders, input bridge.ReadFileInput) (bridge.ReadFileOutput, *bridge.CallError) {
	path, cerr := allowed.resolve(input.Path, false)
	if cerr != nil {
		return bridge.ReadFileOutput{}, cerr
	}
	file, err := os.Open(path)
	if err != nil {
		return bridge.ReadFileOutput{}, fileError(input.Path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return bridge.ReadFileOutput{}, fileError(input.Path, err)
	}
	if info.IsDir() {
		return bridge.ReadFileOutput{}, callError(bridge.ErrorFailed, input.Path+" is a folder. Use list_files to see what is in it.")
	}
	if !info.Mode().IsRegular() {
		return bridge.ReadFileOutput{}, callError(bridge.ErrorFailed, input.Path+" is not a regular file.")
	}
	offset := input.Offset
	if offset < 1 {
		offset = 1
	}
	limit := input.Limit
	if limit <= 0 {
		limit = bridge.DefaultReadLines
	}
	output := bridge.ReadFileOutput{Path: path}
	var content bytes.Buffer
	reader := bufio.NewReaderSize(file, 64<<10)
	line := 0
	lineStarted := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if !lineStarted {
				line++
				lineStarted = true
			}
			if line >= offset && line < offset+limit {
				if room := bridge.MaxReadFileBytes - content.Len(); len(chunk) > room {
					content.Write(chunk[:room])
					output.Truncated = true
				} else {
					content.Write(chunk)
				}
			} else if line >= offset+limit {
				output.Truncated = true
			}
			if chunk[len(chunk)-1] == '\n' {
				lineStarted = false
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bridge.ReadFileOutput{}, fileError(input.Path, err)
		}
	}
	output.TotalLines = line
	output.Content = string(bytes.ToValidUTF8(content.Bytes(), []byte("�")))
	return output, nil
}

func writeFile(allowed folders, input bridge.WriteFileInput) (bridge.WriteFileOutput, *bridge.CallError) {
	if len(input.Content) > bridge.MaxWriteBytes {
		return bridge.WriteFileOutput{}, callError(bridge.ErrorTooLarge, fmt.Sprintf("The content is %d bytes; write_file takes at most %d.", len(input.Content), bridge.MaxWriteBytes))
	}
	path, cerr := allowed.resolve(input.Path, true)
	if cerr != nil {
		return bridge.WriteFileOutput{}, cerr
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return bridge.WriteFileOutput{}, callError(bridge.ErrorFailed, input.Path+" is a folder.")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return bridge.WriteFileOutput{}, fileError(input.Path, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if input.Append {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return bridge.WriteFileOutput{}, fileError(input.Path, err)
	}
	written, err := file.WriteString(input.Content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return bridge.WriteFileOutput{}, fileError(input.Path, err)
	}
	return bridge.WriteFileOutput{Path: path, Bytes: int64(written)}, nil
}

func listFiles(ctx context.Context, allowed folders, input bridge.ListFilesInput) (bridge.ListFilesOutput, *bridge.CallError) {
	root, cerr := allowed.resolve(input.Path, false)
	if cerr != nil {
		return bridge.ListFilesOutput{}, cerr
	}
	info, err := os.Stat(root)
	if err != nil {
		return bridge.ListFilesOutput{}, fileError(input.Path, err)
	}
	if !info.IsDir() {
		return bridge.ListFilesOutput{}, callError(bridge.ErrorFailed, input.Path+" is a file. Use read_file to read it.")
	}
	depth := input.Depth
	if depth <= 0 {
		depth = bridge.DefaultListDepth
	}
	if depth > bridge.MaxListDepth {
		depth = bridge.MaxListDepth
	}
	output := bridge.ListFilesOutput{Path: root, Entries: []bridge.FileEntry{}}
	errStop := errors.New("stop")
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if path == root {
			return err
		}
		if err != nil {
			// An unreadable subfolder is skipped, not fatal.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(output.Entries) >= bridge.MaxListEntries {
			output.Truncated = true
			return errStop
		}
		item := bridge.FileEntry{Path: path, Type: bridge.EntryFile}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			item.Type = bridge.EntrySymlink
		case entry.IsDir():
			item.Type = bridge.EntryDir
		default:
			if info, err := entry.Info(); err == nil {
				item.Size = info.Size()
			}
		}
		output.Entries = append(output.Entries, item)
		if entry.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if 1+countSeparators(rel) >= depth {
				return fs.SkipDir
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		if ctx.Err() != nil {
			return bridge.ListFilesOutput{}, callError(bridge.ErrorTimeout, "Listing "+input.Path+" took too long.")
		}
		return bridge.ListFilesOutput{}, fileError(input.Path, walkErr)
	}
	return output, nil
}

func countSeparators(path string) int {
	count := 0
	for i := 0; i < len(path); i++ {
		if os.IsPathSeparator(path[i]) {
			count++
		}
	}
	return count
}
