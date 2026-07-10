// Package source discovers and resolves portable skill directories across
// ordered host roots without coupling activation to a particular filesystem.
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

type RootKind string

const (
	RootWorkspace    RootKind = "workspace"
	RootProjectAgent RootKind = "project-agent"
	RootPersonal     RootKind = "personal"
	RootManaged      RootKind = "managed"
	RootBundled      RootKind = "bundled"
	RootPlugin       RootKind = "plugin"
	RootExtra        RootKind = "extra"
)

type Root struct {
	ID                          string   `json:"id"`
	Kind                        RootKind `json:"kind"`
	Path                        string   `json:"path"`
	Precedence                  int      `json:"precedence,omitempty"`
	AllowSkillDirectorySymlinks bool     `json:"allowSkillDirectorySymlinks,omitempty"`
	AllowSymlinkTargets         []string `json:"allowSymlinkTargets,omitempty"`
}

type Candidate struct {
	Name         string                     `json:"name"`
	Description  string                     `json:"description,omitempty"`
	Version      string                     `json:"version"`
	Digest       string                     `json:"digest"`
	RootID       string                     `json:"rootId"`
	RootKind     RootKind                   `json:"rootKind"`
	Precedence   int                        `json:"precedence"`
	Directory    string                     `json:"directory"`
	RelativePath string                     `json:"relativePath"`
	Compilation  *skillopenclaw.Compilation `json:"-"`
	rootOrder    int
}

type ShadowedCandidate struct {
	Candidate  Candidate `json:"candidate"`
	ShadowedBy string    `json:"shadowedBy"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	RootID   string `json:"rootId,omitempty"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

type Snapshot struct {
	Revision    string              `json:"revision"`
	Effective   []Candidate         `json:"effective"`
	Shadowed    []ShadowedCandidate `json:"shadowed,omitempty"`
	Diagnostics []Diagnostic        `json:"diagnostics,omitempty"`
}

type Catalog struct{}

func NewCatalog() *Catalog { return &Catalog{} }

func (c *Catalog) Discover(ctx context.Context, roots []Root) (*Snapshot, error) {
	if c == nil {
		return nil, errors.New("skill source catalog is not configured")
	}
	normalized, err := normalizeRoots(roots)
	if err != nil {
		return nil, err
	}
	all := make([]Candidate, 0)
	diagnostics := make([]Diagnostic, 0)
	for order, root := range normalized {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates, rootDiagnostics := discoverRoot(ctx, root, order)
		all = append(all, candidates...)
		diagnostics = append(diagnostics, rootDiagnostics...)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		if all[i].Precedence != all[j].Precedence {
			return all[i].Precedence > all[j].Precedence
		}
		if all[i].rootOrder != all[j].rootOrder {
			return all[i].rootOrder < all[j].rootOrder
		}
		return all[i].RelativePath < all[j].RelativePath
	})
	snapshot := &Snapshot{Effective: make([]Candidate, 0)}
	winners := make(map[string]string)
	for _, candidate := range all {
		if winner := winners[candidate.Name]; winner != "" {
			snapshot.Shadowed = append(snapshot.Shadowed, ShadowedCandidate{Candidate: candidate, ShadowedBy: winner})
			continue
		}
		winners[candidate.Name] = candidate.RootID
		snapshot.Effective = append(snapshot.Effective, candidate)
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].RootID != diagnostics[j].RootID {
			return diagnostics[i].RootID < diagnostics[j].RootID
		}
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].Code < diagnostics[j].Code
	})
	snapshot.Diagnostics = diagnostics
	snapshot.Revision = snapshotRevision(snapshot.Effective)
	return snapshot, nil
}

func normalizeRoots(roots []Root) ([]Root, error) {
	result := make([]Root, len(roots))
	seen := make(map[string]bool)
	for index, root := range roots {
		root.ID = strings.TrimSpace(root.ID)
		root.Path = strings.TrimSpace(root.Path)
		if root.ID == "" || root.Path == "" {
			return nil, fmt.Errorf("skill source root %d requires id and path", index)
		}
		if seen[root.ID] {
			return nil, fmt.Errorf("duplicate skill source root id %q", root.ID)
		}
		seen[root.ID] = true
		if root.Precedence == 0 {
			root.Precedence = defaultPrecedence(root.Kind)
		}
		if root.Precedence <= 0 {
			return nil, fmt.Errorf("skill source root %q requires a known kind or positive precedence", root.ID)
		}
		if root.Kind == RootManaged || root.Kind == RootPersonal {
			root.AllowSkillDirectorySymlinks = true
		}
		result[index] = root
	}
	return result, nil
}

func defaultPrecedence(kind RootKind) int {
	switch kind {
	case RootWorkspace:
		return 600
	case RootProjectAgent:
		return 500
	case RootPersonal:
		return 400
	case RootManaged:
		return 300
	case RootBundled:
		return 200
	case RootPlugin, RootExtra:
		return 100
	default:
		return 0
	}
}

func discoverRoot(ctx context.Context, root Root, order int) ([]Candidate, []Diagnostic) {
	absRoot, err := filepath.Abs(root.Path)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Code: "root.invalid", RootID: root.ID, Path: root.Path, Message: err.Error()}}
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if os.IsNotExist(err) {
		return nil, []Diagnostic{{Severity: "info", Code: "root.missing", RootID: root.ID, Path: absRoot, Message: "skill source root does not exist"}}
	}
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Code: "root.unreadable", RootID: root.ID, Path: absRoot, Message: err.Error()}}
	}
	directories, err := candidateDirectories(absRoot)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Code: "root.scan_failed", RootID: root.ID, Path: absRoot, Message: err.Error()}}
	}
	allowedTargets := make([]string, 0, len(root.AllowSymlinkTargets))
	for _, target := range root.AllowSymlinkTargets {
		absolute, targetErr := filepath.Abs(target)
		if targetErr != nil {
			continue
		}
		resolved, targetErr := filepath.EvalSymlinks(absolute)
		if targetErr == nil {
			allowedTargets = append(allowedTargets, resolved)
		}
	}
	result := make([]Candidate, 0, len(directories))
	diagnostics := make([]Diagnostic, 0)
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return result, append(diagnostics, Diagnostic{Severity: "error", Code: "scan.canceled", RootID: root.ID, Path: directory, Message: err.Error()})
		}
		realDirectory, resolveErr := resolveSkillDirectory(absRoot, realRoot, directory, root.AllowSkillDirectorySymlinks, allowedTargets)
		if resolveErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: "skill.unsafe_path", RootID: root.ID, Path: directory, Message: resolveErr.Error()})
			continue
		}
		bundle, loadErr := loadBundle(ctx, realDirectory)
		if loadErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: "skill.load_failed", RootID: root.ID, Path: directory, Message: loadErr.Error()})
			continue
		}
		relative, _ := filepath.Rel(absRoot, directory)
		compilation, compileErr := skillopenclaw.Compile(skillopenclaw.Bundle{
			SkillMD: bundle.SkillMD, Files: bundle.Files,
			Source: skillopenclaw.Source{Reference: string(root.Kind) + ":" + root.ID + ":" + filepath.ToSlash(relative)},
		})
		if compileErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: "skill.compile_failed", RootID: root.ID, Path: directory, Message: compileErr.Error()})
			continue
		}
		result = append(result, Candidate{
			Name: compilation.Definition.ID, Description: compilation.Definition.Description, Version: compilation.Definition.Version,
			Digest: compilation.SourceDigest, RootID: root.ID, RootKind: root.Kind, Precedence: root.Precedence,
			Directory: realDirectory, RelativePath: filepath.ToSlash(relative), Compilation: compilation, rootOrder: order,
		})
	}
	return result, diagnostics
}

func candidateDirectories(root string) ([]string, error) {
	result := make([]string, 0)
	seen := make(map[string]bool)
	add := func(directory string) {
		if !seen[directory] {
			seen[directory] = true
			result = append(result, directory)
		}
	}
	if fileExists(filepath.Join(root, "SKILL.md")) {
		add(root)
	}
	first, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range first {
		if skipSourceDirectory(entry.Name()) || !directoryOrSymlink(filepath.Join(root, entry.Name())) {
			continue
		}
		child := filepath.Join(root, entry.Name())
		if fileExists(filepath.Join(child, "SKILL.md")) {
			add(child)
			continue
		}
		second, readErr := os.ReadDir(child)
		if readErr != nil {
			continue
		}
		for _, nested := range second {
			if skipSourceDirectory(nested.Name()) || !directoryOrSymlink(filepath.Join(child, nested.Name())) {
				continue
			}
			candidate := filepath.Join(child, nested.Name())
			if fileExists(filepath.Join(candidate, "SKILL.md")) {
				add(candidate)
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func resolveSkillDirectory(root, realRoot, directory string, allowDirectorySymlink bool, allowedTargets []string) (string, error) {
	if !within(root, directory) {
		return "", errors.New("skill directory escapes configured root")
	}
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	if !within(realRoot, realDirectory) && !allowDirectorySymlink && !withinAny(allowedTargets, realDirectory) {
		return "", errors.New("skill directory symlink target is not allowed")
	}
	skillFile, err := filepath.EvalSymlinks(filepath.Join(directory, "SKILL.md"))
	if err != nil {
		return "", err
	}
	if !within(realDirectory, skillFile) {
		return "", errors.New("SKILL.md realpath escapes resolved skill directory")
	}
	return realDirectory, nil
}

func loadBundle(ctx context.Context, root string) (skillopenclaw.Bundle, error) {
	files := make([]skillopenclaw.File, 0)
	var skillMD []byte
	count := 0
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && skipSourceDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("supporting resource symlink %q is not allowed", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 32*1024*1024 {
			return fmt.Errorf("skill file %q exceeds 32 MiB", relative)
		}
		count++
		total += info.Size()
		if count > 2048 || total > 256*1024*1024 {
			return errors.New("skill directory exceeds file or size limits")
		}
		stream, err := os.Open(path)
		if err != nil {
			return err
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, 32*1024*1024+1))
		closeErr := stream.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if relative == "SKILL.md" {
			skillMD = content
		} else {
			files = append(files, skillopenclaw.File{Path: relative, Content: content})
		}
		return nil
	})
	if err != nil {
		return skillopenclaw.Bundle{}, err
	}
	if len(skillMD) == 0 {
		return skillopenclaw.Bundle{}, errors.New("skill directory is missing SKILL.md")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return skillopenclaw.Bundle{SkillMD: skillMD, Files: files}, nil
}

func snapshotRevision(effective []Candidate) string {
	type identity struct {
		Name       string   `json:"name"`
		Version    string   `json:"version"`
		Digest     string   `json:"digest"`
		RootID     string   `json:"rootId"`
		RootKind   RootKind `json:"rootKind"`
		Directory  string   `json:"directory"`
		Precedence int      `json:"precedence"`
	}
	values := make([]identity, len(effective))
	for index, candidate := range effective {
		values[index] = identity{candidate.Name, candidate.Version, candidate.Digest, candidate.RootID, candidate.RootKind, candidate.Directory, candidate.Precedence}
	}
	encoded, _ := json.Marshal(values)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func directoryOrSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		resolved, err := os.Stat(path)
		return err == nil && resolved.IsDir()
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func skipSourceDirectory(name string) bool {
	return name == ".git" || name == ".clawhub" || name == "node_modules" || name == ".DS_Store"
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func withinAny(roots []string, path string) bool {
	for _, root := range roots {
		if within(root, path) {
			return true
		}
	}
	return false
}
