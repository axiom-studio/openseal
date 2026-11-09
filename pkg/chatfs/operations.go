package chatfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/workspace"
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

var errInput = workspace.ErrInvalidInput

func failure(message string) map[string]interface{} {
	return map[string]interface{}{"success": false, "isError": true, "error": message}
}

var (
	text    = workspace.Text
	integer = workspace.Integer
)

// withVersion is a result with the snapshot it read or made.
func withVersion(result map[string]interface{}, version int64) map[string]interface{} {
	result["version"] = version
	return result
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
	prefix := ""
	if directory != "" {
		if _, isFile := head.Tree.Files[directory]; isFile {
			return nil, workspace.IsAFile(directory)
		}
		prefix = directory + "/"
	}
	listing := workspace.NewListing(func(parts []string) string { return prefix + strings.Join(parts, "/") },
		integer(args, "depth", workspace.DefaultListDepth, workspace.MaximumListDepth), integer(args, "maxEntries", workspace.DefaultListEntries, workspace.MaximumListEntries))
	found := directory == ""
	for _, name := range head.Tree.Paths() {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		found = true
		entry := head.Tree.Files[name]
		if !listing.Add(strings.Split(strings.TrimPrefix(name, prefix), "/"), workspace.Entry{Type: workspace.EntryFile, Size: workspace.FileSize(entry.SizeBytes), MediaType: entry.MediaType}) {
			break
		}
	}
	if !found {
		return nil, workspace.NotFound(directory)
	}
	return withVersion(listing.Result().Map(), head.Version), nil
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
	result, err := workspace.ReadContent(ctx, name, entry.MediaType, entry.SizeBytes, integer(args, "maxBytes", workspace.DefaultReadBytes, workspace.MaximumReadBytes),
		func(limit int64) ([]byte, error) { return s.readPrefix(ctx, chat, entry, limit) })
	if err != nil {
		return nil, err
	}
	return result.Map(), nil
}

// readPrefix reads at most limit bytes from the start of a file.
func (s *Store) readPrefix(ctx context.Context, chat Chat, entry Entry, limit int64) ([]byte, error) {
	content, err := s.Open(ctx, chat, entry)
	if err != nil {
		return nil, err
	}
	defer content.Close()
	return io.ReadAll(io.LimitReader(content, min(limit, entry.SizeBytes)))
}

func missing(tree Tree, name string) error {
	prefix := name + "/"
	for existing := range tree.Files {
		if strings.HasPrefix(existing, prefix) {
			return workspace.IsADirectory(name)
		}
	}
	return workspace.NotFound(name)
}

func (s *Store) search(ctx context.Context, chat Chat, args map[string]interface{}) (map[string]interface{}, error) {
	search, err := workspace.NewSearch(args)
	if err != nil {
		return nil, err
	}
	directory, err := CleanDirectory(text(args, "path"))
	if err != nil {
		return nil, err
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	for _, name := range head.Tree.Paths() {
		if directory != "" && name != directory && !strings.HasPrefix(name, directory+"/") {
			continue
		}
		entry := head.Tree.Files[name]
		if !search.Wants(name) || !workspace.Searchable(entry.MediaType, entry.SizeBytes) {
			continue
		}
		data, err := s.ReadAll(ctx, chat, entry, workspace.SearchFileBytes)
		if err != nil {
			return nil, err
		}
		if !search.Scan(name, data) {
			break
		}
	}
	return search.Result().Map(), nil
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
	content, err := workspace.WriteContent(args)
	if err != nil {
		return nil, err
	}
	mediaType, err := workspace.WrittenMediaType(name, name, content)
	if err != nil {
		return nil, err
	}
	entry, err := s.Put(ctx, chat, strings.NewReader(content), int64(len(content)), mediaType)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.Commit(ctx, chat, author, []Change{{Path: name, Entry: &entry}})
	if err != nil {
		return nil, err
	}
	return withVersion(workspace.ChangeResult{Success: true, ChangedPaths: []string{name}}.Map(), snapshot.Version), nil
}

func (s *Store) edit(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	name, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	edit, err := workspace.NewEdit(args)
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
	if !Textual(entry.MediaType) {
		return nil, workspace.NotText(name)
	}
	if entry.SizeBytes > MaximumWriteBytes {
		return nil, workspace.TooLargeToEdit(name)
	}
	data, err := s.ReadAll(ctx, chat, entry, MaximumWriteBytes)
	if err != nil {
		return nil, err
	}
	content, replacements, err := edit.Apply(name, string(data))
	if err != nil {
		return nil, err
	}
	updated, err := s.Put(ctx, chat, strings.NewReader(content), int64(len(content)), entry.MediaType)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.Commit(ctx, chat, author, []Change{{Path: name, Entry: &updated, Expect: &entry.Digest}})
	if err != nil {
		return nil, err
	}
	return withVersion(workspace.ChangeResult{Success: true, ChangedPaths: []string{name}, Replacements: replacements}.Map(), snapshot.Version), nil
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
		return nil, workspace.NotFound(name)
	}
	changes := make([]Change, 0, len(paths))
	for _, existing := range paths {
		changes = append(changes, Change{Path: existing, Expect: Expectation(head.Tree, existing)})
	}
	snapshot, err := s.Commit(ctx, chat, author, changes)
	if err != nil {
		return nil, err
	}
	return withVersion(workspace.ChangeResult{Success: true, ChangedPaths: paths}.Map(), snapshot.Version), nil
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
		return nil, workspace.MoveIntoItself(from)
	}
	head, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	paths := matching(head.Tree, from)
	if len(paths) == 0 {
		return nil, workspace.NotFound(from)
	}
	if len(matching(head.Tree, to)) > 0 {
		return nil, workspace.AlreadyExists(to)
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
	return withVersion(workspace.ChangeResult{Success: true, ChangedPaths: changed}.Map(), snapshot.Version), nil
}
