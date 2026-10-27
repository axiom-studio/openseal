// Package chatfs is a chat's filesystem. Every chat has one directory tree
// that its Agents read and write freely, like a coding agent's working copy:
// the files people attach, the images, documents and apps Agents make, and
// anything else an Agent writes. Each change commits a new immutable snapshot
// of the whole tree, so any earlier state can be read, served or restored.
//
// A snapshot is stored as a version of one hidden artifact per chat; file
// bytes are content-addressed in the host's artifact content store, so an
// unchanged file costs nothing to keep in later snapshots.
package chatfs

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/chatpath"
)

const (
	// ArtifactType is the hidden artifact type a chat's snapshots are stored as.
	ArtifactType = "chat_filesystem"
	// MediaType identifies a stored snapshot.
	MediaType = "application/vnd.openseal.chat-filesystem+json"
	// Schema is the stored snapshot format.
	Schema = "openseal.dev/chat-filesystem/v1"

	MaximumFiles = 20000
	// MaximumWriteBytes bounds one text file an Agent writes.
	MaximumWriteBytes = 4 << 20
)

// Entry is one file in a snapshot.
type Entry struct {
	ContentRef string    `json:"contentRef"`
	Digest     string    `json:"digest"`
	SizeBytes  int64     `json:"sizeBytes"`
	MediaType  string    `json:"mediaType"`
	ModifiedAt time.Time `json:"modifiedAt"`
	// Origin is what made the file when a tool did, such as the action call
	// that generated an image or the attachment a person sent, so the same
	// thing is never added twice.
	Origin string `json:"origin,omitempty"`
}

// Tree is one complete snapshot: every file by path.
type Tree struct {
	Schema string           `json:"schema"`
	Files  map[string]Entry `json:"files"`
}

// Snapshot is a tree at its version. Version 0 is the empty tree of a chat
// nothing was written to yet.
type Snapshot struct {
	Version int64
	Tree    Tree
}

func emptyTree() Tree { return Tree{Schema: Schema, Files: map[string]Entry{}} }

// Paths returns the snapshot's file paths in order.
func (t Tree) Paths() []string {
	paths := make([]string, 0, len(t.Files))
	for name := range t.Files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

// FindOrigin returns the path of the file origin made.
func (t Tree) FindOrigin(origin string) (string, bool) {
	for _, name := range t.Paths() {
		if t.Files[name].Origin == origin {
			return name, true
		}
	}
	return "", false
}

// NewPath picks a free path for a new file a tool made: folder + slug of name
// + extension, numbered when taken ("hero.png", "hero-2.png"). A known media
// type decides the extension; otherwise the name's own is kept.
func NewPath(tree Tree, folder, name, mediaType string) string {
	base := path.Base(strings.TrimSpace(name))
	ext := chatpath.Extension(base, mediaType)
	if folder == Apps {
		ext = ""
	}
	stem := Slug(strings.TrimSuffix(base, path.Ext(base)), strings.TrimSuffix(strings.TrimSuffix(folder, "/"), "s"))
	taken := func(candidate string) bool {
		if _, ok := tree.Files[candidate]; ok {
			return true
		}
		prefix := candidate + "/"
		for name := range tree.Files {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	candidate := folder + stem + ext
	for number := 2; taken(candidate); number++ {
		candidate = fmt.Sprintf("%s%s-%d%s", folder, stem, number, ext)
	}
	return candidate
}
