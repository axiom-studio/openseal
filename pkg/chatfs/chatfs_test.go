package chatfs

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

type catalog struct {
	*runtime.ArtifactCatalog
	store *runtime.MemoryStore
}

func (c catalog) RegisterArtifact(ctx context.Context, request runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error) {
	return c.Register(ctx, request)
}

func (c catalog) GetArtifact(ctx context.Context, scope runtime.Scope, id string, version int64) (*runtime.Artifact, error) {
	return c.store.GetArtifact(ctx, scope, id, version)
}

func (c catalog) ListArtifacts(ctx context.Context, filter runtime.ArtifactFilter) ([]*runtime.Artifact, error) {
	return c.store.ListArtifacts(ctx, filter)
}

func newTestStore(t *testing.T) (*Store, Chat, Author) {
	t.Helper()
	content, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	memory := runtime.NewMemoryStore()
	chat := Chat{Scope: runtime.Scope{Kind: "tenant", ID: "1"}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "finn"}, ConversationID: "c1"}
	author := Author{Producer: runtime.ActivityActor{Type: "agent", ID: "finn"}, RunID: "run-1"}
	return NewStore(catalog{runtime.NewArtifactCatalog(memory), memory}, content), chat, author
}

func run(t *testing.T, store *Store, chat Chat, author Author, operation string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	result, err := store.Execute(context.Background(), chat, author, operation, args)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	return result
}

func TestAgentsReadWriteEditAndMoveChatFiles(t *testing.T) {
	store, chat, author := newTestStore(t)
	written := run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "/apps/todo/src/App.tsx", "content": "export default function App() {\n  return <h1>Hi</h1>\n}\n"})
	if written["success"] != true || written["version"] != int64(1) {
		t.Fatalf("write = %#v", written)
	}
	run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "notes.md", "content": "# Notes\n"})

	listing := run(t, store, chat, author, workspace.OperationListDirectory, map[string]interface{}{"depth": float64(8)})
	var paths []string
	for _, entry := range listing["entries"].([]map[string]interface{}) {
		paths = append(paths, entry["path"].(string)+":"+entry["type"].(string))
	}
	if got := strings.Join(paths, " "); got != "apps:directory apps/todo:directory apps/todo/src:directory apps/todo/src/App.tsx:file notes.md:file" {
		t.Fatalf("listing = %s", got)
	}

	edited := run(t, store, chat, author, workspace.OperationEditFile, map[string]interface{}{"path": "apps/todo/src/App.tsx", "oldText": "<h1>Hi</h1>", "newText": "<h1>Hello</h1>"})
	if edited["success"] != true {
		t.Fatalf("edit = %#v", edited)
	}
	read := run(t, store, chat, author, workspace.OperationReadFile, map[string]interface{}{"path": "apps/todo/src/App.tsx"})
	if !strings.Contains(read["content"].(string), "<h1>Hello</h1>") {
		t.Fatalf("read = %#v", read)
	}

	missingText := run(t, store, chat, author, workspace.OperationEditFile, map[string]interface{}{"path": "apps/todo/src/App.tsx", "oldText": "<h2>", "newText": "<h3>"})
	if missingText["isError"] != true || !strings.Contains(missingText["error"].(string), "not found") {
		t.Fatalf("edit of absent text = %#v", missingText)
	}

	moved := run(t, store, chat, author, workspace.OperationMoveFile, map[string]interface{}{"path": "apps/todo", "destination": "apps/tasks"})
	if moved["success"] != true {
		t.Fatalf("move = %#v", moved)
	}
	found := run(t, store, chat, author, workspace.OperationSearchFiles, map[string]interface{}{"query": "Hello"})
	matches := found["matches"].([]map[string]interface{})
	if len(matches) != 1 || matches[0]["path"] != "apps/tasks/src/App.tsx" || matches[0]["line"] != 2 {
		t.Fatalf("search = %#v", found)
	}

	run(t, store, chat, author, workspace.OperationDeleteFile, map[string]interface{}{"path": "apps"})
	head, err := store.Head(context.Background(), chat)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(head.Tree.Paths(), ","); got != "notes.md" {
		t.Fatalf("after delete = %s", got)
	}
	// Every change is a snapshot: the first write is still there.
	first, err := store.At(context.Background(), chat, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.Tree.Files["apps/todo/src/App.tsx"]; !ok || len(first.Tree.Files) != 1 {
		t.Fatalf("version 1 = %v", first.Tree.Paths())
	}
}

func TestPathsStayInsideTheChat(t *testing.T) {
	store, chat, author := newTestStore(t)
	for _, bad := range []string{"../x", "a/../../x", "", "a\\b", "a\x00b"} {
		result := run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": bad, "content": "x"})
		if result["isError"] != true {
			t.Fatalf("write %q = %#v", bad, result)
		}
	}
	run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a/b.txt", "content": "x"})
	if result := run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a", "content": "x"}); result["isError"] != true {
		t.Fatalf("file over a directory = %#v", result)
	}
	if result := run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a/b.txt/c", "content": "x"}); result["isError"] != true {
		t.Fatalf("file inside a file = %#v", result)
	}
}

func TestConcurrentWritesToDifferentFilesAllLand(t *testing.T) {
	store, chat, author := newTestStore(t)
	var group sync.WaitGroup
	for index := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "f" + string(rune('a'+index)) + ".txt", "content": "x"})
		}()
	}
	group.Wait()
	head, err := store.Head(context.Background(), chat)
	if err != nil {
		t.Fatal(err)
	}
	if len(head.Tree.Files) != 8 || head.Version != 8 {
		t.Fatalf("head = v%d %v", head.Version, head.Tree.Paths())
	}
}

func TestAnEditNeverOverwritesAChangeMadeSinceItsRead(t *testing.T) {
	store, chat, author := newTestStore(t)
	ctx := context.Background()
	run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt", "content": "one"})
	head, _ := store.Head(ctx, chat)
	stale := head.Tree.Files["a.txt"].Digest
	run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt", "content": "two"})
	entry, err := store.Put(ctx, chat, strings.NewReader("three"), 5, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, chat, author, []Change{{Path: "a.txt", Entry: &entry, Expect: &stale}}); err == nil {
		t.Fatal("stale edit committed")
	}
}

func TestOtherOwnersCannotReadAChatsFiles(t *testing.T) {
	store, chat, author := newTestStore(t)
	run(t, store, chat, author, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt", "content": "secret"})
	intruder := chat
	intruder.Owner.ID = "someone-else"
	if _, err := store.Head(context.Background(), intruder); err == nil {
		t.Fatal("another owner read the chat's files")
	}
}

func TestToolsPickReadablePaths(t *testing.T) {
	tree := Tree{Files: map[string]Entry{"images/hero.png": {}}}
	if got := NewPath(tree, Images, "Hero!", "image/png"); got != "images/hero-2.png" {
		t.Fatalf("NewPath = %s", got)
	}
	if got := NewPath(tree, Docs, "Q3 Sales Report.pdf", "application/pdf"); got != "docs/q3-sales-report.pdf" {
		t.Fatalf("NewPath = %s", got)
	}
}
