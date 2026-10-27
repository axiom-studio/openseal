package chatfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/attachments"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

const (
	defaultReadBytes = 256 << 10
	maximumReadBytes = 1 << 20
	// searchFileBytes bounds one file a search reads.
	searchFileBytes = 4 << 20
)

// Execute runs one Workspace operation against a chat's files. A result
// with "isError" reports a problem the Agent can act on, such as an edit
// whose text is not in the file; an error means the files are unavailable.
func (s *Store) Execute(ctx context.Context, chat Chat, author Author, operation string, args map[string]interface{}) (map[string]interface{}, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	var (
		result map[string]interface{}
		err    error
	)
	// A read that names another chat reads that chat's files.
	if _, other := args[workspace.ChatArgument]; other && operation != workspace.OperationCopyFromChat {
		switch operation {
		case workspace.OperationListDirectory, workspace.OperationReadFile, workspace.OperationSearchFiles:
		default:
			return failure("only list_directory, read_file and search_files can look at another chat; copy a file here with copy_from_chat to change it"), nil
		}
		source, err := s.otherChat(ctx, chat, text(args, workspace.ChatArgument))
		if err != nil {
			return failure(err.Error()), nil
		}
		rest := make(map[string]interface{}, len(args))
		for key, value := range args {
			if key != workspace.ChatArgument {
				rest[key] = value
			}
		}
		return s.Execute(ctx, source, author, operation, rest)
	}
	switch operation {
	case workspace.OperationListDirectory:
		result, err = s.list(ctx, chat, args)
	case workspace.OperationReadFile:
		result, err = s.read(ctx, chat, args)
	case workspace.OperationSearchFiles:
		result, err = s.search(ctx, chat, args)
	case workspace.OperationBatchRead:
		return s.batchRead(ctx, chat, author, args)
	case workspace.OperationWriteFile:
		result, err = s.write(ctx, chat, author, args)
	case workspace.OperationEditFile:
		result, err = s.edit(ctx, chat, author, args)
	case workspace.OperationDeleteFile:
		result, err = s.remove(ctx, chat, author, args)
	case workspace.OperationMoveFile:
		result, err = s.move(ctx, chat, author, args)
	case workspace.OperationListChats:
		result, err = s.listChats(ctx, chat, args)
	case workspace.OperationCopyFromChat:
		result, err = s.copyFromChat(ctx, chat, author, args)
	default:
		return nil, fmt.Errorf("unsupported chat files operation %q", operation)
	}
	if err != nil && (errors.Is(err, ErrPath) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, errInput) || errors.Is(err, ErrOtherChat)) {
		return failure(err.Error()), nil
	}
	return result, err
}

var errInput = errors.New("invalid input")

func failure(message string) map[string]interface{} {
	return map[string]interface{}{"success": false, "isError": true, "error": message}
}

func text(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return value
}

func integer(args map[string]interface{}, key string, fallback, maximum int) int {
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

func (s *Store) list(ctx context.Context, chat Chat, args map[string]interface{}) (map[string]interface{}, error) {
	directory, err := CleanDirectory(text(args, "path"))
	if err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	depth, limit := integer(args, "depth", 2, 8), integer(args, "maxEntries", 500, 2000)
	prefix := ""
	if directory != "" {
		if _, isFile := head.Tree.Files[directory]; isFile {
			return nil, fmt.Errorf("%w: %s is a file; read it with read_file", ErrPath, directory)
		}
		prefix = directory + "/"
	}
	seen := map[string]bool{}
	entries := []map[string]interface{}{}
	found := directory == ""
	truncated := false
	for _, name := range head.Tree.Paths() {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		found = true
		parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
		for level := 1; level <= len(parts) && level <= depth; level++ {
			entryPath := prefix + strings.Join(parts[:level], "/")
			if seen[entryPath] {
				continue
			}
			seen[entryPath] = true
			if len(entries) >= limit {
				truncated = true
				break
			}
			if level < len(parts) {
				entries = append(entries, map[string]interface{}{"path": entryPath, "type": "directory"})
				continue
			}
			entry := head.Tree.Files[name]
			entries = append(entries, map[string]interface{}{"path": entryPath, "type": "file", "size": entry.SizeBytes, "mediaType": entry.MediaType})
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNotFound, directory)
	}
	return map[string]interface{}{"entries": entries, "truncated": truncated, "version": head.Version}, nil
}

func (s *Store) read(ctx context.Context, chat Chat, args map[string]interface{}) (map[string]interface{}, error) {
	name, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	entry, ok := head.Tree.Files[name]
	if !ok {
		return nil, missing(head.Tree, name)
	}
	maximum := integer(args, "maxBytes", defaultReadBytes, maximumReadBytes)
	result := map[string]interface{}{"path": name, "mediaType": entry.MediaType, "size": entry.SizeBytes}
	switch media := BaseMediaType(entry.MediaType); {
	case Textual(media) || media == "application/octet-stream" && entry.SizeBytes <= maximumReadBytes:
		data, err := s.ReadAll(ctx, chat, entry, maximumReadBytes*4)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			result["content"] = ""
			result["note"] = "This is a binary file; use its path to show or embed it."
			return result, nil
		}
		truncated := len(data) > maximum
		if truncated {
			data = data[:maximum]
			for !utf8.Valid(data) {
				data = data[:len(data)-1]
			}
		}
		result["content"], result["truncated"] = string(data), truncated
	case media == "application/pdf" || media == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		if entry.SizeBytes > attachments.MaximumFileBytes {
			result["content"], result["note"] = "", "This document is too large to extract text from."
			return result, nil
		}
		data, err := s.ReadAll(ctx, chat, entry, attachments.MaximumFileBytes)
		if err != nil {
			return nil, err
		}
		var status, extracted string
		if media == "application/pdf" {
			status, extracted = attachments.PDFText(ctx, data)
		} else {
			status, extracted = attachments.DOCXText(ctx, data)
		}
		if status != "supplied" {
			result["content"], result["note"] = "", "No text could be extracted from this document ("+strings.ReplaceAll(status, "_", " ")+")."
			return result, nil
		}
		truncated := len(extracted) > maximum
		if truncated {
			extracted = extracted[:maximum]
			for !utf8.ValidString(extracted) {
				extracted = extracted[:len(extracted)-1]
			}
		}
		result["content"], result["truncated"], result["note"] = extracted, truncated, "Text extracted from the document."
	default:
		result["content"] = ""
		result["note"] = "This is a binary file; use its path to show or embed it."
	}
	return result, nil
}

func missing(tree Tree, name string) error {
	prefix := name + "/"
	for existing := range tree.Files {
		if strings.HasPrefix(existing, prefix) {
			return fmt.Errorf("%w: %s is a directory; list it with list_directory", ErrPath, name)
		}
	}
	return fmt.Errorf("%w: %s does not exist", ErrNotFound, name)
}

func (s *Store) search(ctx context.Context, chat Chat, args map[string]interface{}) (map[string]interface{}, error) {
	query := text(args, "query")
	if query == "" {
		return nil, fmt.Errorf("%w: query is required", errInput)
	}
	directory, err := CleanDirectory(text(args, "path"))
	if err != nil {
		return nil, err
	}
	var pattern *regexp.Regexp
	if useRegex, _ := args["regex"].(bool); useRegex {
		if pattern, err = regexp.Compile(query); err != nil {
			return nil, fmt.Errorf("%w: invalid search regular expression", errInput)
		}
	}
	glob := text(args, "glob")
	limit := integer(args, "maxResults", 200, 1000)
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	matches := []map[string]interface{}{}
	truncated := false
	for _, name := range head.Tree.Paths() {
		if directory != "" && name != directory && !strings.HasPrefix(name, directory+"/") {
			continue
		}
		if glob != "" {
			if matched, _ := path.Match(glob, path.Base(name)); !matched {
				if matched, _ = path.Match(glob, name); !matched {
					continue
				}
			}
		}
		entry := head.Tree.Files[name]
		if !Textual(entry.MediaType) || entry.SizeBytes > searchFileBytes {
			continue
		}
		data, err := s.ReadAll(ctx, chat, entry, searchFileBytes)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 64<<10), searchFileBytes)
		for line := 1; scanner.Scan(); line++ {
			value := scanner.Text()
			if pattern != nil && pattern.MatchString(value) || pattern == nil && strings.Contains(value, query) {
				if len(matches) >= limit {
					truncated = true
					break
				}
				if len(value) > 2000 {
					value = value[:2000]
				}
				matches = append(matches, map[string]interface{}{"path": name, "line": line, "text": value})
			}
		}
		if truncated {
			break
		}
	}
	return map[string]interface{}{"matches": matches, "truncated": truncated}, nil
}

func (s *Store) batchRead(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	calls, ok := args["calls"].([]interface{})
	if !ok || len(calls) < 2 || len(calls) > 8 {
		return failure("batch read requires two to eight independent calls"), nil
	}
	results := make([]map[string]interface{}, len(calls))
	errs := make([]error, len(calls))
	var workers sync.WaitGroup
	for index, raw := range calls {
		call, _ := raw.(map[string]interface{})
		operation, _ := call["operation"].(string)
		arguments, _ := call["arguments"].(map[string]interface{})
		if operation != workspace.OperationListDirectory && operation != workspace.OperationReadFile && operation != workspace.OperationSearchFiles {
			results[index] = map[string]interface{}{"operation": operation, "result": failure("only list, read and search calls can be batched")}
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := s.Execute(ctx, chat, author, operation, arguments)
			errs[index] = err
			results[index] = map[string]interface{}{"operation": operation, "result": result}
		}()
	}
	workers.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return map[string]interface{}{"results": results}, nil
}

func (s *Store) write(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	name, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("%w: content is required", errInput)
	}
	if len(content) > MaximumWriteBytes {
		return nil, fmt.Errorf("%w: a written file holds at most %d bytes", errInput, MaximumWriteBytes)
	}
	mediaType := MediaTypeOf(name)
	if mediaType == "application/octet-stream" {
		mediaType = "text/plain"
	}
	if !Textual(mediaType) {
		return nil, fmt.Errorf("%w: %s is a %s file; write_file writes text files only", errInput, name, mediaType)
	}
	entry, err := s.Put(ctx, chat, strings.NewReader(content), int64(len(content)), mediaType)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.Commit(ctx, chat, author, []Change{{Path: name, Entry: &entry}})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "changedPaths": []string{name}, "version": snapshot.Version}, nil
}

func (s *Store) edit(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	name, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	oldText, newText := text(args, "oldText"), text(args, "newText")
	if oldText == "" {
		return nil, fmt.Errorf("%w: oldText is required; use write_file to create a file", errInput)
	}
	if oldText == newText {
		return nil, fmt.Errorf("%w: oldText and newText are the same", errInput)
	}
	replaceAll, _ := args["replaceAll"].(bool)
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	entry, ok := head.Tree.Files[name]
	if !ok {
		return nil, missing(head.Tree, name)
	}
	if !Textual(entry.MediaType) {
		return nil, fmt.Errorf("%w: %s is not a text file", errInput, name)
	}
	data, err := s.ReadAll(ctx, chat, entry, MaximumWriteBytes)
	if err != nil {
		return nil, err
	}
	content := string(data)
	count := strings.Count(content, oldText)
	switch {
	case count == 0:
		return nil, fmt.Errorf("%w: oldText was not found in %s; read the file and copy the exact text, including whitespace", errInput, name)
	case count > 1 && !replaceAll:
		return nil, fmt.Errorf("%w: oldText appears %d times in %s; include more surrounding text to make it unique, or set replaceAll", errInput, count, name)
	}
	if replaceAll {
		content = strings.ReplaceAll(content, oldText, newText)
	} else {
		content = strings.Replace(content, oldText, newText, 1)
	}
	if len(content) > MaximumWriteBytes {
		return nil, fmt.Errorf("%w: a file holds at most %d bytes", errInput, MaximumWriteBytes)
	}
	updated, err := s.Put(ctx, chat, strings.NewReader(content), int64(len(content)), entry.MediaType)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.Commit(ctx, chat, author, []Change{{Path: name, Entry: &updated, Expect: &entry.Digest}})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "changedPaths": []string{name}, "replacements": max(1, count*boolInt(replaceAll)), "version": snapshot.Version}, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// matching returns the files at name: the file itself, or every file below
// the directory it names.
func matching(tree Tree, name string) []string {
	if _, ok := tree.Files[name]; ok {
		return []string{name}
	}
	var found []string
	for _, existing := range tree.Paths() {
		if strings.HasPrefix(existing, name+"/") {
			found = append(found, existing)
		}
	}
	return found
}

func (s *Store) remove(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	name, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	paths := matching(head.Tree, name)
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNotFound, name)
	}
	changes := make([]Change, 0, len(paths))
	for _, existing := range paths {
		changes = append(changes, Change{Path: existing, Expect: Expectation(head.Tree, existing)})
	}
	snapshot, err := s.Commit(ctx, chat, author, changes)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "changedPaths": paths, "version": snapshot.Version}, nil
}

func (s *Store) move(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	from, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	to, err := Clean(text(args, "destination"))
	if err != nil {
		return nil, err
	}
	if from == to || strings.HasPrefix(to, from+"/") {
		return nil, fmt.Errorf("%w: cannot move %s into itself", ErrPath, from)
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	paths := matching(head.Tree, from)
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNotFound, from)
	}
	if len(matching(head.Tree, to)) > 0 {
		return nil, fmt.Errorf("%w: %s already exists; delete it first", ErrPath, to)
	}
	changes := make([]Change, 0, len(paths)*2)
	changed := make([]string, 0, len(paths)*2)
	for _, existing := range paths {
		entry := head.Tree.Files[existing]
		target := to + strings.TrimPrefix(existing, from)
		changes = append(changes, Change{Path: existing, Expect: &entry.Digest}, Change{Path: target, Entry: &entry, Expect: new(string)})
		changed = append(changed, existing, target)
	}
	sort.Strings(changed)
	snapshot, err := s.Commit(ctx, chat, author, changes)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "changedPaths": changed, "version": snapshot.Version}, nil
}
