// Package chatpath holds the rules for paths in a chat's files (see
// pkg/chatfs): what a path may be, and what kind of file a name holds. It has
// no dependencies so that every package naming chat files can use it.
package chatpath

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaximumPathBytes = 1024
	MaximumNameBytes = 255
)

// Folders Agents' tools put what they make in. They are conventions, not
// rules: an Agent may put any file anywhere.
const (
	Apps    = "apps/"
	Uploads = "uploads/"
	Slides  = "slides/"
	Pages   = "pages/"
	Sheets  = "sheets/"
	Images  = "images/"
	Docs    = "docs/"
	Media   = "media/"
	Files   = "files/"
)

// ErrPath explains a path that cannot name a file.
var ErrPath = errors.New("invalid path")

// Clean returns the canonical form of a file path: relative, slash
// separated, without "." or ".." segments. A leading "/" is accepted and
// dropped, since apps import chat files by absolute path.
func Clean(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimLeft(value, "/")
	if value == "" || value == "." {
		return "", fmt.Errorf("%w: a file path is required", ErrPath)
	}
	if len(value) > MaximumPathBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") {
		return "", fmt.Errorf("%w: %q is not a valid path", ErrPath, raw)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: %q contains a control character", ErrPath, raw)
		}
	}
	clean := path.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q is outside the chat's files", ErrPath, raw)
	}
	for _, segment := range strings.Split(clean, "/") {
		if len(segment) > MaximumNameBytes {
			return "", fmt.Errorf("%w: a name in %q is longer than %d bytes", ErrPath, raw, MaximumNameBytes)
		}
	}
	return clean, nil
}

// CleanDirectory is Clean for a directory, where "" and "." name the root.
func CleanDirectory(raw string) (string, error) {
	value := strings.Trim(strings.TrimSpace(raw), "/")
	if value == "" || value == "." {
		return "", nil
	}
	return Clean(value)
}

// FromImport returns the file an app's module or stylesheet specifier names:
// an absolute path such as "/images/hero.png".
func FromImport(specifier string) (string, bool) {
	if !strings.HasPrefix(specifier, "/") || strings.HasPrefix(specifier, "//") {
		return "", false
	}
	clean, err := Clean(specifier)
	return clean, err == nil
}

var extensions = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "image/svg+xml": ".svg",
	"application/pdf": ".pdf", "text/html": ".html", "text/plain": ".txt", "text/markdown": ".md", "text/csv": ".csv",
	"application/json": ".json", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/webm": ".weba", "video/mp4": ".mp4", "video/webm": ".webm",
}

var mediaTypes = map[string]string{
	".ts": "text/typescript", ".tsx": "text/typescript", ".jsx": "text/javascript", ".js": "text/javascript", ".mjs": "text/javascript",
	".css": "text/css", ".html": "text/html", ".htm": "text/html", ".md": "text/markdown", ".txt": "text/plain", ".csv": "text/csv",
	".json": "application/json", ".svg": "image/svg+xml", ".xml": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml",
	".py": "text/x-python", ".go": "text/x-go", ".sh": "text/x-shellscript", ".sql": "application/sql", ".toml": "application/toml",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif", ".pdf": "application/pdf",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".mp4": "video/mp4", ".webm": "video/webm",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// MediaTypeOf guesses a file's media type from its name.
func MediaTypeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if known, ok := mediaTypes[ext]; ok {
		return known
	}
	if guessed := mime.TypeByExtension(ext); guessed != "" {
		return BaseMediaType(guessed)
	}
	return "application/octet-stream"
}

// BaseMediaType drops parameters such as charset.
func BaseMediaType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return parsed
}

// Textual reports whether a media type holds text an Agent can read and edit.
func Textual(mediaType string) bool {
	media := BaseMediaType(mediaType)
	switch {
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/yaml", "application/toml", "application/sql", "application/javascript", "image/svg+xml":
		return true
	}
	return false
}

// Folder is the conventional folder for a file a tool made.
func Folder(artifactType, mediaType string, uploaded bool) string {
	media := BaseMediaType(mediaType)
	switch {
	case artifactType == "app":
		return Apps
	case uploaded:
		return Uploads
	case artifactType == "slides" || artifactType == "slide_deck":
		return Slides
	case artifactType == "page":
		return Pages
	case artifactType == "sheet":
		return Sheets
	case strings.HasPrefix(media, "image/"):
		return Images
	case media == "application/pdf":
		return Docs
	case strings.HasPrefix(media, "audio/"), strings.HasPrefix(media, "video/"):
		return Media
	}
	return Files
}

var (
	slugRun          = regexp.MustCompile(`[^a-z0-9]+`)
	extensionPattern = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)
)

// Slug turns a title into a file name stem: "Q3 Sales Report" → "q3-sales-report".
func Slug(title, fallback string) string {
	stem := strings.Trim(slugRun.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), "-"), "-")
	if len(stem) > 60 {
		stem = strings.Trim(stem[:60], "-")
	}
	if stem == "" {
		stem = fallback
	}
	return stem
}

// Extension is the extension a file of mediaType named name should have: the
// media type's own when known, else the name's when it is a plain one.
func Extension(name, mediaType string) string {
	if known, ok := extensions[BaseMediaType(mediaType)]; ok {
		return known
	}
	ext := strings.ToLower(path.Ext(path.Base(strings.TrimSpace(name))))
	if extensionPattern.MatchString(ext) {
		return ext
	}
	return ""
}
