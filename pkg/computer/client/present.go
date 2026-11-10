package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Uploader sends the bytes of one file of a present call to HelloSeal. The
// API is the real one.
type Uploader interface {
	Upload(ctx context.Context, callID string, index int, path string, size int64, mediaType string) error
}

// Present runs a present call: it checks every path first, so one bad path
// sends nothing, then sends each file with upload, and answers with what it
// sent. The bytes go with the uploads, never in the result.
func Present(ctx context.Context, call bridge.Call, upload Uploader) bridge.Result {
	if call.DeadlineUnixMs > 0 {
		deadline := call.Deadline()
		if !time.Now().Before(deadline) {
			return failed(call.ID, callError(bridge.ErrorTimeout, "The call arrived after HelloSeal stopped waiting for it."))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return failed(call.ID, callError(bridge.ErrorFailed, "The call's arguments are not valid: "+err.Error()))
	}
	if len(args.Paths) == 0 || len(args.Paths) > bridge.MaxPresentFiles {
		return failed(call.ID, callError(bridge.ErrorFailed, fmt.Sprintf("Show one to %d files at a time.", bridge.MaxPresentFiles)))
	}
	m := machine{allowed: resolveFolders(call.Folders)}
	files := make([]bridge.PresentedFile, 0, len(args.Paths))
	for _, raw := range args.Paths {
		file, cerr := m.presentable(raw)
		if cerr != nil {
			return failed(call.ID, cerr)
		}
		files = append(files, file)
	}
	for index, file := range files {
		if err := upload.Upload(ctx, call.ID, index, file.Path, file.Size, file.MediaType); err != nil {
			return failed(call.ID, uploadError(file.Path, err))
		}
	}
	encoded, err := json.Marshal(bridge.PresentResult{Files: files})
	if err != nil {
		return failed(call.ID, callError(bridge.ErrorFailed, "Could not encode the result."))
	}
	return bridge.Result{ID: call.ID, OK: true, Result: encoded}
}

// presentable checks a path present may send: inside an allowed folder
// (reading is enough), a regular file, and no larger than
// bridge.MaxPresentBytes.
func (m machine) presentable(raw string) (bridge.PresentedFile, *bridge.CallError) {
	path, cerr := m.allowed.resolve(raw, false)
	if cerr != nil {
		return bridge.PresentedFile{}, cerr
	}
	info, err := os.Stat(path)
	if err != nil {
		return bridge.PresentedFile{}, fileError(path, err)
	}
	if info.IsDir() {
		return bridge.PresentedFile{}, callError(bridge.ErrorFailed, path+" is a folder, not a file. List it with list_directory and show the files in it.")
	}
	if !info.Mode().IsRegular() {
		return bridge.PresentedFile{}, notRegular(path)
	}
	if info.Size() > bridge.MaxPresentBytes {
		return bridge.PresentedFile{}, callError(bridge.ErrorTooLarge, fmt.Sprintf("%s is %s, more than the %d MB a file shown in the chat may be.",
			path, megabytes(info.Size()), bridge.MaxPresentBytes>>20))
	}
	return bridge.PresentedFile{Path: path, Size: info.Size(), MediaType: mediaTypeOf(path)}, nil
}

// megabytes is a size as a person reads it, such as 72.4 MB.
func megabytes(size int64) string {
	return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
}

// errFileChanged means a file changed size while it was being sent.
var errFileChanged = errors.New("it changed while it was being sent; try again")

// uploadError says in plain words why a file did not reach HelloSeal.
func uploadError(path string, err error) *bridge.CallError {
	var callErr *bridge.CallError
	var status *StatusError
	switch {
	case errors.As(err, &callErr):
		return callErr
	case errors.Is(err, context.DeadlineExceeded):
		return callError(bridge.ErrorTimeout, "Sending "+path+" took longer than HelloSeal waits; try a smaller file or a faster connection.")
	case missing(err):
		return asCallError(workspace.NotFound(path))
	case errors.As(err, &status) && status.Body != "":
		return callError(bridge.ErrorFailed, "Could not send "+path+" to HelloSeal: "+status.Body)
	}
	return callError(bridge.ErrorFailed, fmt.Sprintf("Could not send %s to HelloSeal: %v", path, err))
}
