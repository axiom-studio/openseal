package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// folders are a call's allowed folders, resolved on this computer.
type folders []resolvedFolder

type resolvedFolder struct {
	path   string
	access string
}

// resolveFolders expands and resolves the folders a call carries. A folder
// that does not exist here is dropped: nothing inside it can be used.
func resolveFolders(list []bridge.Folder) folders {
	var out folders
	for _, folder := range list {
		path, err := expandPath(folder.Path)
		if err != nil || !filepath.IsAbs(path) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		access := bridge.AccessRead
		if folder.Access == bridge.AccessWrite {
			access = bridge.AccessWrite
		}
		out = append(out, resolvedFolder{path: resolved, access: access})
	}
	return out
}

// defaultFolder is where a command runs and relative paths start: the first
// write folder, else the first folder.
func (f folders) defaultFolder() (resolvedFolder, bool) {
	for _, folder := range f {
		if folder.access == bridge.AccessWrite {
			return folder, true
		}
	}
	if len(f) > 0 {
		return f[0], true
	}
	return resolvedFolder{}, false
}

// resolve turns a path from a call into a real path on this computer:
// ~ is expanded, a relative path starts at the default folder, the result
// is cleaned and every symlink in it is resolved. A path that does not exist
// yet resolves through its nearest existing parent. It must land inside an
// allowed folder; with write, that folder must allow writes.
func (f folders) resolve(raw string, write bool) (string, *bridge.CallError) {
	if strings.TrimSpace(raw) == "" {
		return "", callError(bridge.ErrorFailed, "A path is required.")
	}
	if len(raw) > bridge.MaxPathBytes || strings.ContainsRune(raw, 0) {
		return "", callError(bridge.ErrorFailed, "That path is not valid.")
	}
	if len(f) == 0 {
		return "", callError(bridge.ErrorOutsideFolder, "No folders are allowed on this computer. Add one in Settings → Computers.")
	}
	path, err := expandPath(raw)
	if err != nil {
		return "", callError(bridge.ErrorFailed, err.Error())
	}
	if !filepath.IsAbs(path) {
		base, _ := f.defaultFolder()
		path = filepath.Join(base.path, path)
	}
	resolved, err := resolveExisting(filepath.Clean(path))
	if err != nil {
		return "", callError(bridge.ErrorFailed, fmt.Sprintf("Could not resolve %s: %v", raw, err))
	}
	folder, ok := f.containing(resolved)
	if !ok {
		return "", callError(bridge.ErrorOutsideFolder, fmt.Sprintf("%s is outside the folders allowed on this computer (%s).", raw, f.names()))
	}
	if write && folder.access != bridge.AccessWrite {
		return "", callError(bridge.ErrorReadOnly, fmt.Sprintf("%s is in %s, which is read-only.", raw, folder.path))
	}
	return resolved, nil
}

// containing is the most specific allowed folder that holds path, so a
// read-only folder inside a write folder stays read-only.
func (f folders) containing(path string) (resolvedFolder, bool) {
	var best resolvedFolder
	found := false
	for _, folder := range f {
		if within(folder.path, path) && (!found || len(folder.path) > len(best.path)) {
			best, found = folder, true
		}
	}
	return best, found
}

func (f folders) names() string {
	names := make([]string, len(f))
	for i, folder := range f {
		names[i] = folder.path
	}
	return strings.Join(names, ", ")
}

// within reports whether path is root or inside it. Both are resolved and
// clean.
func within(root, path string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// Both default to case-insensitive file systems.
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// resolveExisting resolves symlinks in path. When path does not exist, it
// resolves the nearest existing parent and appends the rest, which cannot
// contain symlinks because it does not exist.
func resolveExisting(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// A dangling symlink must not resolve through its parent: its target
	// could be anywhere, and writing would follow it.
	if _, lerr := os.Lstat(path); lerr == nil {
		return "", errors.New("it is a link to something that does not exist")
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolvedParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

// expandPath expands a leading ~ to the home folder.
func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path != "~" && !strings.HasPrefix(path, "~/") && !(runtime.GOOS == "windows" && strings.HasPrefix(path, `~\`)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("could not find the home folder to expand ~")
	}
	return filepath.Join(home, path[1:]), nil
}

func callError(code, message string) *bridge.CallError {
	return &bridge.CallError{Code: code, Message: message}
}
