package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/attachments"
	"github.com/axiom-studio/openseal/pkg/chatpath"
)

// The rules every machine applies to its files, so a chat's files and a
// person's computer read, search and change files alike. A machine finds
// and stores the files; these shape what the model sees.

// Errors the machine operations fail with. Their texts are what the model
// reads, so every machine builds them with the functions below.
var (
	ErrInvalidPath  = chatpath.ErrPath
	ErrNotFound     = errors.New("file not found")
	ErrInvalidInput = errors.New("invalid input")
)

// NotFound says path names nothing.
func NotFound(path string) error { return fmt.Errorf("%w: %s does not exist", ErrNotFound, path) }

// IsAFile refuses to list a file.
func IsAFile(path string) error {
	return fmt.Errorf("%w: %s is a file; read it with read_file", ErrInvalidPath, path)
}

// IsADirectory refuses to read or edit a directory.
func IsADirectory(path string) error {
	return fmt.Errorf("%w: %s is a directory; list it with list_directory", ErrInvalidPath, path)
}

// MoveIntoItself refuses to move a path onto or into itself.
func MoveIntoItself(path string) error {
	return fmt.Errorf("%w: cannot move %s into itself", ErrInvalidPath, path)
}

// AlreadyExists refuses to move onto an existing path.
func AlreadyExists(path string) error {
	return fmt.Errorf("%w: %s already exists; delete it first", ErrInvalidPath, path)
}

// NotText refuses to edit a file that is not text.
func NotText(path string) error {
	return fmt.Errorf("%w: %s is not a text file", ErrInvalidInput, path)
}

// Text is a string argument, "" when absent.
func Text(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return value
}

// Flag is a boolean argument, false when absent.
func Flag(args map[string]interface{}, key string) bool {
	value, _ := args[key].(bool)
	return value
}

// Integer is an integer argument: fallback when absent or below 1, at most
// maximum.
func Integer(args map[string]interface{}, key string, fallback, maximum int) int {
	var value int
	switch raw := args[key].(type) {
	case float64:
		value = int(raw)
	case int:
		value = raw
	case int64:
		value = int(raw)
	default:
		return fallback
	}
	if value < 1 {
		return fallback
	}
	return min(value, maximum)
}

// Listing builds list_directory's result from the leaves below the listed
// directory: its files, symlinks and empty directories. Leaves must come in
// path order, which ListOrder gives one level at a time. Each leaf adds the
// directories above it, up to depth levels, then itself if it is within
// depth.
type Listing struct {
	join         func(parts []string) string
	depth, limit int
	seen         map[string]bool
	result       ListDirectoryResult
	full         bool
}

// NewListing lists depth levels and at most maxEntries entries (both as
// list_directory's arguments, already bounded). join turns a leaf's path
// below the directory, as parts, into the path a result shows.
func NewListing(join func(parts []string) string, depth, maxEntries int) *Listing {
	return &Listing{join: join, depth: depth, limit: maxEntries, seen: map[string]bool{}, result: ListDirectoryResult{Entries: []Entry{}}}
}

// Add adds a leaf at parts below the directory. It returns false once the
// listing is full: the rest are left out and the result is truncated.
func (l *Listing) Add(parts []string, leaf Entry) bool {
	if l.full {
		return false
	}
	for level := 1; level <= len(parts) && level <= l.depth; level++ {
		shown := l.join(parts[:level])
		if l.seen[shown] {
			continue
		}
		l.seen[shown] = true
		if len(l.result.Entries) >= l.limit {
			l.result.Truncated, l.full = true, true
			return false
		}
		if level < len(parts) {
			l.result.Entries = append(l.result.Entries, Entry{Path: shown, Type: EntryDirectory})
			continue
		}
		leaf.Path = shown
		l.result.Entries = append(l.result.Entries, leaf)
	}
	return true
}

// Truncate ends the listing early, such as at a deadline: the rest are left
// out.
func (l *Listing) Truncate() { l.result.Truncated, l.full = true, true }

// Result is the listing.
func (l *Listing) Result() ListDirectoryResult { return l.result }

// ListOrder is the key that orders the entries of one directory so that a
// walk visiting them in key order sees paths in the order of their full
// slash paths, as a chat's files are listed: a directory sorts as its name
// followed by "/".
func ListOrder(name string, directory bool) string {
	if directory {
		return name + "/"
	}
	return name
}

// FileSize is a file's size for an Entry.
func FileSize(size int64) *int64 { return &size }

// ReadContent shapes read_file's result for a file of mediaType and size,
// shown as shown. load reads the file's first limit bytes (fewer at its end).
func ReadContent(ctx context.Context, shown, mediaType string, size int64, maxBytes int, load func(limit int64) ([]byte, error)) (ReadFileResult, error) {
	result := ReadFileResult{Path: shown, MediaType: mediaType, Size: size}
	const binary = "This is a binary file; use its path to show or embed it."
	switch media := chatpath.BaseMediaType(mediaType); {
	case chatpath.Textual(media) || media == "application/octet-stream" && size <= MaximumReadBytes:
		limit := int64(maxBytes) + utf8.UTFMax
		data, err := load(limit)
		if err != nil {
			return ReadFileResult{}, err
		}
		if int64(len(data)) >= limit {
			data = withoutCutRune(data)
		}
		if !utf8.Valid(data) {
			result.Note = binary
			return result, nil
		}
		result.Content, result.Truncated = cutText(string(data), maxBytes)
		if !result.Truncated && size > int64(len(data)) {
			result.Truncated = true
		}
	case media == "application/pdf" || media == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		if size > attachments.MaximumFileBytes {
			result.Note = "This document is too large to extract text from."
			return result, nil
		}
		data, err := load(attachments.MaximumFileBytes)
		if err != nil {
			return ReadFileResult{}, err
		}
		var status, extracted string
		if media == "application/pdf" {
			status, extracted = attachments.PDFText(ctx, data)
		} else {
			status, extracted = attachments.DOCXText(ctx, data)
		}
		if status != "supplied" {
			result.Note = "No text could be extracted from this document (" + strings.ReplaceAll(status, "_", " ") + ")."
			return result, nil
		}
		result.Content, result.Truncated = cutText(extracted, maxBytes)
		result.Note = "Text extracted from the document."
	default:
		result.Note = binary
	}
	return result, nil
}

// cutText keeps at most maximum bytes of text, cut back to a whole
// character.
func cutText(text string, maximum int) (string, bool) {
	if len(text) <= maximum {
		return text, false
	}
	text = text[:maximum]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true
}

// withoutCutRune drops an incomplete character at the end of data that a
// read limit cut.
func withoutCutRune(data []byte) []byte {
	for back := 1; back <= utf8.UTFMax && back <= len(data); back++ {
		if at := len(data) - back; utf8.RuneStart(data[at]) {
			if !utf8.FullRune(data[at:]) {
				return data[:at]
			}
			return data
		}
	}
	return data
}

// Searchable reports whether search_files reads a file of mediaType and
// size.
func Searchable(mediaType string, size int64) bool {
	return chatpath.Textual(mediaType) && size <= SearchFileBytes
}

// Search collects search_files' matches. Files must come in path order.
type Search struct {
	query   string
	pattern *regexp.Regexp
	glob    string
	limit   int
	result  SearchFilesResult
}

// NewSearch reads search_files' query, regex, glob and maxResults
// arguments.
func NewSearch(args map[string]interface{}) (*Search, error) {
	search := &Search{query: Text(args, "query"), glob: Text(args, "glob"), limit: Integer(args, "maxResults", DefaultSearchResults, MaximumSearchResults),
		result: SearchFilesResult{Matches: []SearchMatch{}}}
	if search.query == "" {
		return nil, fmt.Errorf("%w: query is required", ErrInvalidInput)
	}
	if Flag(args, "regex") {
		pattern, err := regexp.Compile(search.query)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid search regular expression", ErrInvalidInput)
		}
		search.pattern = pattern
	}
	return search, nil
}

// Wants reports whether glob selects a file, by its name or by its slash
// path from the top of the machine's files (a chat's files, or the allowed
// folder that holds it).
func (s *Search) Wants(slashPath string) bool {
	if s.glob == "" {
		return true
	}
	if matched, _ := path.Match(s.glob, path.Base(slashPath)); matched {
		return true
	}
	matched, _ := path.Match(s.glob, slashPath)
	return matched
}

// Scan adds the matching lines of one file, shown as shown. It returns false
// once the search is full.
func (s *Search) Scan(shown string, data []byte) bool {
	if s.result.Truncated {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), SearchFileBytes)
	for line := 1; scanner.Scan(); line++ {
		value := scanner.Text()
		if s.pattern != nil && s.pattern.MatchString(value) || s.pattern == nil && strings.Contains(value, s.query) {
			if len(s.result.Matches) >= s.limit {
				s.result.Truncated = true
				return false
			}
			value, _ = cutText(value, SearchLineBytes)
			s.result.Matches = append(s.result.Matches, SearchMatch{Path: shown, Line: line, Text: value})
		}
	}
	return true
}

// Truncate ends the search early, such as at a deadline.
func (s *Search) Truncate() { s.result.Truncated = true }

// Result is the search's result.
func (s *Search) Result() SearchFilesResult { return s.result }

// WriteContent is write_file's content argument, which is required.
func WriteContent(args map[string]interface{}) (string, error) {
	content, ok := args["content"].(string)
	if !ok {
		return "", fmt.Errorf("%w: content is required", ErrInvalidInput)
	}
	return content, nil
}

// WrittenMediaType is the media type write_file gives a file: from its
// name, with text for a name that says nothing. It refuses content too large
// and a name of a type that is not text.
func WrittenMediaType(shown, name, content string) (string, error) {
	if len(content) > MaximumFileBytes {
		return "", fmt.Errorf("%w: a written file holds at most %d bytes", ErrInvalidInput, MaximumFileBytes)
	}
	mediaType := chatpath.MediaTypeOf(name)
	if mediaType == "application/octet-stream" {
		mediaType = "text/plain"
	}
	if !chatpath.Textual(mediaType) {
		return "", fmt.Errorf("%w: %s is a %s file; write_file writes text files only", ErrInvalidInput, shown, mediaType)
	}
	return mediaType, nil
}

// Edit is edit_file's request.
type Edit struct {
	OldText, NewText string
	ReplaceAll       bool
}

// NewEdit reads edit_file's oldText, newText and replaceAll arguments.
func NewEdit(args map[string]interface{}) (Edit, error) {
	edit := Edit{OldText: Text(args, "oldText"), NewText: Text(args, "newText"), ReplaceAll: Flag(args, "replaceAll")}
	if edit.OldText == "" {
		return Edit{}, fmt.Errorf("%w: oldText is required; use write_file to create a file", ErrInvalidInput)
	}
	if edit.OldText == edit.NewText {
		return Edit{}, fmt.Errorf("%w: oldText and newText are the same", ErrInvalidInput)
	}
	return edit, nil
}

// Apply edits a file's content, shown as shown, and says how many places
// changed.
func (e Edit) Apply(shown, content string) (string, int, error) {
	count := strings.Count(content, e.OldText)
	switch {
	case count == 0:
		return "", 0, fmt.Errorf("%w: oldText was not found in %s; read the file and copy the exact text, including whitespace", ErrInvalidInput, shown)
	case count > 1 && !e.ReplaceAll:
		return "", 0, fmt.Errorf("%w: oldText appears %d times in %s; include more surrounding text to make it unique, or set replaceAll", ErrInvalidInput, count, shown)
	}
	replaced := 1
	if e.ReplaceAll {
		content, replaced = strings.ReplaceAll(content, e.OldText, e.NewText), count
	} else {
		content = strings.Replace(content, e.OldText, e.NewText, 1)
	}
	if len(content) > MaximumFileBytes {
		return "", 0, fmt.Errorf("%w: a file holds at most %d bytes", ErrInvalidInput, MaximumFileBytes)
	}
	return content, replaced, nil
}

// TooLargeToEdit refuses to edit a file larger than MaximumFileBytes.
func TooLargeToEdit(shown string) error {
	return fmt.Errorf("%w: %s is larger than edit_file can change (%d bytes); use write_file or run_command", ErrInvalidInput, shown, MaximumFileBytes)
}
