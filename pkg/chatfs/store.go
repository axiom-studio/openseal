package chatfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// Artifacts is the artifact catalog a chat's snapshots are versions in.
type Artifacts interface {
	RegisterArtifact(context.Context, runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error)
	GetArtifact(context.Context, runtime.Scope, string, int64) (*runtime.Artifact, error)
	ListArtifacts(context.Context, runtime.ArtifactFilter) ([]*runtime.Artifact, error)
}

// Chat identifies one chat's filesystem. Its fields must come from the
// persisted conversation, never from model arguments.
type Chat struct {
	Scope          runtime.Scope
	Owner          runtime.ObjectiveOwner
	ConversationID string
}

func (c Chat) validate() error {
	if c.Scope.Validate() != nil || c.Owner.Validate() != nil || strings.TrimSpace(c.ConversationID) == "" {
		return errors.New("chat filesystem identity is incomplete")
	}
	return nil
}

// ArtifactID is the hidden artifact a chat's snapshots are versions of.
func ArtifactID(conversationID string) string { return "chatfs-" + strings.TrimSpace(conversationID) }

// Author records who committed a snapshot.
type Author struct {
	Producer runtime.ActivityActor
	RunID    string
	TurnID   string
}

// Onboarder lists the files a chat already has when its filesystem is first
// opened: those its messages carry and its Runs made before chats had files.
// It returns them as changes to an empty tree.
type Onboarder func(context.Context, Chat) ([]Change, error)

// Store reads and commits chat filesystems.
type Store struct {
	Artifacts Artifacts
	Content   runtime.ArtifactContentStore
	// Onboard, when set, gives a chat that has no snapshot yet its first one.
	Onboard Onboarder
	now     func() time.Time
}

// onboardingAuthor commits a chat's first snapshot.
var onboardingAuthor = Author{Producer: runtime.ActivityActor{Type: "service", ID: "chat-files-onboarding"}}

func NewStore(artifacts Artifacts, content runtime.ArtifactContentStore) *Store {
	return &Store{Artifacts: artifacts, Content: content, now: time.Now}
}

// ErrConflict means a file changed after the caller read it.
var ErrConflict = errors.New("file changed")

// ErrNotFound means a path names no file.
var ErrNotFound = errors.New("file not found")

// Head returns the chat's latest snapshot, or the empty version 0.
func (s *Store) Head(ctx context.Context, chat Chat) (Snapshot, error) {
	if err := chat.validate(); err != nil {
		return Snapshot{}, err
	}
	artifacts, err := s.Artifacts.ListArtifacts(ctx, runtime.ArtifactFilter{Scope: chat.Scope, ID: ArtifactID(chat.ConversationID), LatestOnly: true, Limit: 1})
	if err != nil {
		return Snapshot{}, fmt.Errorf("read chat files: %w", err)
	}
	if len(artifacts) == 0 || artifacts[0] == nil {
		if s.Onboard == nil {
			return Snapshot{Tree: emptyTree()}, nil
		}
		return s.onboard(ctx, chat)
	}
	return s.load(ctx, chat, artifacts[0])
}

// onboard commits a chat's first snapshot from the files it already has. It
// commits even an empty tree, so a chat is onboarded once; a concurrent
// onboarding of the same chat loses the race and reads the winner's.
func (s *Store) onboard(ctx context.Context, chat Chat) (Snapshot, error) {
	changes, err := s.Onboard(ctx, chat)
	if err != nil {
		return Snapshot{}, fmt.Errorf("bring this chat's files in: %w", err)
	}
	tree := emptyTree()
	if err := apply(tree, changes); err != nil {
		return Snapshot{}, err
	}
	snapshot, err := s.save(ctx, chat, onboardingAuthor, tree, 0)
	if errors.Is(err, runtime.ErrArtifactVersionConflict) || errors.Is(err, runtime.ErrArtifactImmutable) {
		artifacts, listErr := s.Artifacts.ListArtifacts(ctx, runtime.ArtifactFilter{Scope: chat.Scope, ID: ArtifactID(chat.ConversationID), LatestOnly: true, Limit: 1})
		if listErr != nil || len(artifacts) == 0 || artifacts[0] == nil {
			return Snapshot{}, errors.New("chat files are busy; try again")
		}
		return s.load(ctx, chat, artifacts[0])
	}
	return snapshot, err
}

// At returns the snapshot at version.
func (s *Store) At(ctx context.Context, chat Chat, version int64) (Snapshot, error) {
	if err := chat.validate(); err != nil {
		return Snapshot{}, err
	}
	if version == 0 {
		return Snapshot{Tree: emptyTree()}, nil
	}
	artifact, err := s.Artifacts.GetArtifact(ctx, chat.Scope, ArtifactID(chat.ConversationID), version)
	if err != nil || artifact == nil {
		return Snapshot{}, fmt.Errorf("chat files version %d is unavailable", version)
	}
	return s.load(ctx, chat, artifact)
}

func (s *Store) load(ctx context.Context, chat Chat, artifact *runtime.Artifact) (Snapshot, error) {
	if artifact.Scope != chat.Scope || artifact.Type != ArtifactType || artifact.Provenance.Owner == nil || *artifact.Provenance.Owner != chat.Owner {
		return Snapshot{}, errors.New("chat files belong to another owner")
	}
	reader, err := s.Content.Open(ctx, chat.Scope, artifact.ContentRef)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open chat files: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, 64<<20))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read chat files: %w", err)
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != artifact.Digest {
		return Snapshot{}, errors.New("chat files snapshot is corrupt")
	}
	var tree Tree
	if err := json.Unmarshal(data, &tree); err != nil || tree.Schema != Schema {
		return Snapshot{}, errors.New("chat files snapshot is unreadable")
	}
	if tree.Files == nil {
		tree.Files = map[string]Entry{}
	}
	return Snapshot{Version: artifact.Version, Tree: tree}, nil
}

// Put stores bytes for a file and returns its entry, ready to commit.
func (s *Store) Put(ctx context.Context, chat Chat, content io.Reader, size int64, mediaType string) (Entry, error) {
	if err := chat.validate(); err != nil {
		return Entry{}, err
	}
	owner := chat.Owner
	stored, err := s.Content.Put(ctx, runtime.ArtifactContentWrite{Scope: chat.Scope, MediaType: mediaType, Reader: content, SizeBytes: size, Owner: &owner})
	if err != nil {
		return Entry{}, fmt.Errorf("store file: %w", err)
	}
	return Entry{ContentRef: stored.ContentRef, Digest: stored.Digest, SizeBytes: stored.SizeBytes, MediaType: BaseMediaType(mediaType), ModifiedAt: s.now().UTC()}, nil
}

// Open reads a file's bytes.
func (s *Store) Open(ctx context.Context, chat Chat, entry Entry) (io.ReadCloser, error) {
	return s.Content.Open(ctx, chat.Scope, entry.ContentRef)
}

// ReadAll reads a whole file of at most limit bytes.
func (s *Store) ReadAll(ctx context.Context, chat Chat, entry Entry, limit int64) ([]byte, error) {
	if entry.SizeBytes > limit {
		return nil, fmt.Errorf("file is %d bytes, more than %d", entry.SizeBytes, limit)
	}
	reader, err := s.Open(ctx, chat, entry)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != entry.SizeBytes || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != entry.Digest {
		return nil, errors.New("file content does not match its record")
	}
	return data, nil
}

// Change is one file change in a commit. Entry nil deletes Path. When Expect
// is set, the commit fails with ErrConflict unless the file's digest at the
// head is *Expect ("" meaning the file must not exist), so an edit made from
// one read never overwrites a change made since.
type Change struct {
	Path   string
	Entry  *Entry
	Expect *string
}

// Digest returns the expectation that path is as tree has it.
func Expectation(tree Tree, path string) *string {
	value := ""
	if entry, ok := tree.Files[path]; ok {
		value = entry.Digest
	}
	return &value
}

// Commit applies changes to the chat's head as one new snapshot. Concurrent
// commits to other files both land; only an Expect mismatch fails.
func (s *Store) Commit(ctx context.Context, chat Chat, author Author, changes []Change) (Snapshot, error) {
	if err := chat.validate(); err != nil {
		return Snapshot{}, err
	}
	if len(changes) == 0 {
		return s.Head(ctx, chat)
	}
	if strings.TrimSpace(author.Producer.Type) == "" || strings.TrimSpace(author.Producer.ID) == "" {
		return Snapshot{}, errors.New("chat files commit requires an author")
	}
	for attempt := 0; attempt < 16; attempt++ {
		head, err := s.Head(ctx, chat)
		if err != nil {
			return Snapshot{}, err
		}
		tree := Tree{Schema: Schema, Files: make(map[string]Entry, len(head.Tree.Files)+len(changes))}
		for name, entry := range head.Tree.Files {
			tree.Files[name] = entry
		}
		if err := apply(tree, changes); err != nil {
			return Snapshot{}, err
		}
		snapshot, err := s.save(ctx, chat, author, tree, head.Version)
		if errors.Is(err, runtime.ErrArtifactVersionConflict) || errors.Is(err, runtime.ErrArtifactImmutable) {
			continue
		}
		return snapshot, err
	}
	return Snapshot{}, errors.New("chat files are busy; try again")
}

// apply makes changes to tree in order, checking each one's expectation.
func apply(tree Tree, changes []Change) error {
	for _, change := range changes {
		name, err := Clean(change.Path)
		if err != nil {
			return err
		}
		if change.Expect != nil && *Expectation(tree, name) != *change.Expect {
			return fmt.Errorf("%w: %s changed since it was read; read it again", ErrConflict, name)
		}
		if change.Entry == nil {
			delete(tree.Files, name)
			continue
		}
		if err := checkPlacement(tree, name); err != nil {
			return err
		}
		tree.Files[name] = *change.Entry
	}
	if len(tree.Files) > MaximumFiles {
		return fmt.Errorf("a chat holds at most %d files", MaximumFiles)
	}
	return nil
}

// save stores tree as the snapshot after previous.
func (s *Store) save(ctx context.Context, chat Chat, author Author, tree Tree, previous int64) (Snapshot, error) {
	encoded, err := json.Marshal(tree)
	if err != nil {
		return Snapshot{}, err
	}
	owner := chat.Owner
	stored, err := s.Content.Put(ctx, runtime.ArtifactContentWrite{Scope: chat.Scope, MediaType: MediaType, Reader: bytes.NewReader(encoded), SizeBytes: int64(len(encoded)), Owner: &owner})
	if err != nil {
		return Snapshot{}, fmt.Errorf("store chat files: %w", err)
	}
	_, err = s.Artifacts.RegisterArtifact(ctx, runtime.RegisterArtifactRequest{ExpectedLatestVersion: previous, Artifact: &runtime.Artifact{
		ID: ArtifactID(chat.ConversationID), Version: previous + 1, Scope: chat.Scope,
		Name: "Chat files", Type: ArtifactType, MediaType: MediaType,
		ContentRef: stored.ContentRef, Digest: stored.Digest, SizeBytes: stored.SizeBytes,
		Classification: runtime.ArtifactClassificationInternal,
		Metadata:       map[string]interface{}{"conversationId": chat.ConversationID},
		Provenance:     runtime.ArtifactProvenance{Producer: author.Producer, Owner: &owner, RunID: author.RunID, TurnID: author.TurnID},
	}})
	if errors.Is(err, runtime.ErrArtifactVersionConflict) || errors.Is(err, runtime.ErrArtifactImmutable) {
		return Snapshot{}, err
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("commit chat files: %w", err)
	}
	return Snapshot{Version: previous + 1, Tree: tree}, nil
}

// A file cannot sit where a directory is, or below another file.
func checkPlacement(tree Tree, name string) error {
	for parent := name; ; {
		index := strings.LastIndex(parent, "/")
		if index < 0 {
			break
		}
		parent = parent[:index]
		if _, ok := tree.Files[parent]; ok {
			return fmt.Errorf("%w: %s is a file, so %s cannot be inside it", ErrPath, parent, name)
		}
	}
	prefix := name + "/"
	for existing := range tree.Files {
		if strings.HasPrefix(existing, prefix) {
			return fmt.Errorf("%w: %s is a directory", ErrPath, name)
		}
	}
	return nil
}
