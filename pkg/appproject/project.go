// Package appproject defines an OpenSeal app: a small TypeScript project an
// Agent builds and edits file by file. A project is a set of UTF-8 text files
// at relative paths; src/App.tsx exports the default component the host
// renders. Each saved revision stores the whole project, so any revision can
// be previewed, published, downloaded or restored on its own.
package appproject

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// Entry is the module whose default export the host renders.
	Entry = "src/App.tsx"
	// WidgetEntry is the optional glanceable view a Desk shows; its default
	// export receives {size: "small" | "medium" | "large"}.
	WidgetEntry = "src/Widget.tsx"
	// MediaType identifies a stored project revision.
	MediaType = "application/vnd.openseal.app+json"
	// Schema is the stored revision format.
	Schema = "openseal.dev/app/v1"

	MaximumFiles      = 300
	MaximumFileBytes  = 256 * 1024
	MaximumTotalBytes = 2 * 1024 * 1024
	MaximumPathBytes  = 200
	MaximumEdits      = 200
)

// Extensions an app may contain. Source is TypeScript; plain JavaScript is not
// accepted so every app type-checks the same way.
var extensions = map[string]bool{".ts": true, ".tsx": true, ".css": true, ".json": true, ".svg": true, ".md": true, ".txt": true}

// Project is one complete revision of an app's source.
type Project struct {
	Schema string            `json:"schema"`
	Title  string            `json:"title"`
	Files  map[string]string `json:"files"`
}

// File is a path and its full content.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Replacement swaps one exact, unique occurrence of Find in Path.
type Replacement struct {
	Path    string `json:"path"`
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// Edits change a project. They apply in order: writes, replacements, deletes.
type Edits struct {
	Write   []File        `json:"write,omitempty"`
	Replace []Replacement `json:"replace,omitempty"`
	Delete  []string      `json:"delete,omitempty"`
}

// ValidationError explains, in words an Agent can act on, why a project or
// edit was rejected. Nothing is saved when one is returned.
type ValidationError struct{ Reasons []string }

func (e *ValidationError) Error() string { return "app not saved: " + strings.Join(e.Reasons, "; ") }

func invalid(reasons ...string) error { return &ValidationError{Reasons: reasons} }

// IsValidationError reports whether err describes a rejected project or edit.
func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

// CleanPath returns the canonical form of a project path, or an error for a
// path outside the project, hidden, or of an unsupported type.
func CleanPath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > MaximumPathBytes || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%q is not a relative project path", raw)
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%q is not a canonical relative path (use %q style)", raw, "src/App.tsx")
	}
	for _, segment := range strings.Split(clean, "/") {
		if strings.HasPrefix(segment, ".") || segment == "node_modules" {
			return "", fmt.Errorf("%q uses a hidden or reserved folder", raw)
		}
		for _, r := range segment {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return "", fmt.Errorf("%q may use only letters, digits, '-', '_', '.' and '/'", raw)
			}
		}
	}
	if !extensions[path.Ext(clean)] {
		return "", fmt.Errorf("%q has an unsupported type; use .ts, .tsx, .css, .json, .svg, .md or .txt", raw)
	}
	return clean, nil
}

// New creates a validated project from complete files.
func New(title string, files []File) (Project, error) {
	project := Project{Schema: Schema, Title: strings.TrimSpace(title), Files: map[string]string{}}
	var reasons []string
	for _, file := range files {
		clean, err := CleanPath(file.Path)
		if err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		if _, exists := project.Files[clean]; exists {
			reasons = append(reasons, fmt.Sprintf("%q appears more than once", clean))
			continue
		}
		project.Files[clean] = file.Content
	}
	if len(reasons) > 0 {
		return Project{}, invalid(reasons...)
	}
	return project, project.Validate()
}

// Validate checks limits, encoding and the entry module.
func (p Project) Validate() error {
	var reasons []string
	if p.Schema != Schema {
		reasons = append(reasons, "unknown app format")
	}
	if p.Title == "" || utf8.RuneCountInString(p.Title) > 300 {
		reasons = append(reasons, "title must be 1 to 300 characters")
	}
	if len(p.Files) == 0 {
		reasons = append(reasons, "an app needs at least "+Entry)
	}
	if len(p.Files) > MaximumFiles {
		reasons = append(reasons, fmt.Sprintf("an app may have at most %d files", MaximumFiles))
	}
	total := 0
	for _, name := range p.Paths() {
		content := p.Files[name]
		if clean, err := CleanPath(name); err != nil || clean != name {
			reasons = append(reasons, fmt.Sprintf("%q is not a valid project path", name))
		}
		if len(content) > MaximumFileBytes {
			reasons = append(reasons, fmt.Sprintf("%s is larger than %d KB", name, MaximumFileBytes/1024))
		}
		if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
			reasons = append(reasons, name+" must be UTF-8 text")
		}
		if path.Ext(name) == ".json" && !json.Valid([]byte(content)) {
			reasons = append(reasons, name+" is not valid JSON")
		}
		total += len(content)
	}
	if total > MaximumTotalBytes {
		reasons = append(reasons, fmt.Sprintf("app source is larger than %d MB", MaximumTotalBytes/(1024*1024)))
	}
	if entry, ok := p.Files[Entry]; !ok {
		reasons = append(reasons, Entry+" is required and must export the default App component")
	} else if !strings.Contains(entry, "export default") {
		reasons = append(reasons, Entry+" must export the default App component")
	}
	if len(reasons) > 0 {
		return invalid(reasons...)
	}
	return nil
}

// Paths returns the project's file paths in a stable order.
func (p Project) Paths() []string {
	paths := make([]string, 0, len(p.Files))
	for name := range p.Files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

// Apply returns a new validated project with edits applied. The receiver is
// never modified, and on any error nothing is applied.
func (p Project) Apply(title string, edits Edits) (Project, error) {
	if len(edits.Write)+len(edits.Replace)+len(edits.Delete) == 0 {
		return Project{}, invalid("no changes were given")
	}
	if len(edits.Write)+len(edits.Replace)+len(edits.Delete) > MaximumEdits {
		return Project{}, invalid(fmt.Sprintf("at most %d changes per save", MaximumEdits))
	}
	next := Project{Schema: Schema, Title: p.Title, Files: make(map[string]string, len(p.Files))}
	if strings.TrimSpace(title) != "" {
		next.Title = strings.TrimSpace(title)
	}
	for name, content := range p.Files {
		next.Files[name] = content
	}
	var reasons []string
	for _, file := range edits.Write {
		clean, err := CleanPath(file.Path)
		if err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		next.Files[clean] = file.Content
	}
	for _, change := range edits.Replace {
		clean, err := CleanPath(change.Path)
		if err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		content, ok := next.Files[clean]
		switch {
		case !ok:
			reasons = append(reasons, clean+" does not exist; write it instead")
		case change.Find == "":
			reasons = append(reasons, "replace in "+clean+" needs non-empty find text")
		case strings.Count(content, change.Find) != 1:
			reasons = append(reasons, fmt.Sprintf("find text occurs %d times in %s; it must occur exactly once (read the file and include more surrounding text)", strings.Count(content, change.Find), clean))
		default:
			next.Files[clean] = strings.Replace(content, change.Find, change.Replace, 1)
		}
	}
	for _, raw := range edits.Delete {
		clean, err := CleanPath(raw)
		if err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		if _, ok := next.Files[clean]; !ok {
			reasons = append(reasons, clean+" does not exist")
			continue
		}
		delete(next.Files, clean)
	}
	if len(reasons) > 0 {
		return Project{}, invalid(reasons...)
	}
	return next, next.Validate()
}

// Marshal encodes a validated project for storage.
func (p Project) Marshal() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// Parse decodes and validates a stored project revision.
func Parse(data []byte) (Project, error) {
	var project Project
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&project); err != nil {
		return Project{}, fmt.Errorf("stored app is unreadable: %w", err)
	}
	return project, project.Validate()
}
