package chatfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// Conversations finds the chats an Agent's other chats' files belong to.
type Conversations interface {
	GetConversation(context.Context, runtime.Scope, string) (*runtime.Conversation, error)
	ListConversations(context.Context, runtime.ConversationFilter) ([]*runtime.Conversation, error)
}

const (
	// listedChats bounds how many recent chats list_chats looks through.
	listedChats = 200
	// listedChatFiles bounds the paths list_chats shows of one chat.
	listedChatFiles = 12
)

// ErrOtherChat means a chat an Agent named is not one of its own.
var ErrOtherChat = errors.New("that is not one of your chats")

// otherChat is a chat of the same Agent in the same scope, which a turn of
// from may read. Its identity comes from the persisted conversation.
func (s *Store) otherChat(ctx context.Context, from Chat, conversationID string) (Chat, error) {
	conversationID = strings.TrimSpace(conversationID)
	if s.Conversations == nil {
		return Chat{}, errors.New("other chats are unavailable")
	}
	if conversationID == "" || len(conversationID) > 256 {
		return Chat{}, fmt.Errorf("%w: %q", ErrOtherChat, conversationID)
	}
	conversation, err := s.Conversations.GetConversation(ctx, from.Scope, conversationID)
	if err != nil || conversation == nil || conversation.ID != conversationID || conversation.Scope != from.Scope || conversation.Owner != from.Owner {
		return Chat{}, fmt.Errorf("%w: %q", ErrOtherChat, conversationID)
	}
	return Chat{Scope: from.Scope, Owner: from.Owner, ConversationID: conversationID}, nil
}

// latest is a chat's latest tree without bringing in an older chat's files,
// which list_chats would otherwise do for every chat it looks at.
func (s *Store) latest(ctx context.Context, chat Chat) (Tree, bool) {
	artifacts, err := s.Artifacts.ListArtifacts(ctx, runtime.ArtifactFilter{Scope: chat.Scope, ID: ArtifactID(chat.ConversationID), LatestOnly: true, Limit: 1})
	if err != nil || len(artifacts) == 0 || artifacts[0] == nil {
		return Tree{}, false
	}
	snapshot, err := s.load(ctx, chat, artifacts[0])
	return snapshot.Tree, err == nil
}

// listChats lists the Agent's other chats, most recently active first, with
// the files in each. A query keeps the chats whose title or file paths
// contain all its words.
func (s *Store) listChats(ctx context.Context, from Chat, args map[string]interface{}) (map[string]interface{}, error) {
	if s.Conversations == nil {
		return nil, errors.New("other chats are unavailable")
	}
	owner := from.Owner
	conversations, err := s.Conversations.ListConversations(ctx, runtime.ConversationFilter{Scope: from.Scope, Owner: &owner, Limit: listedChats})
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	words := strings.Fields(strings.ToLower(text(args, "query")))
	limit := integer(args, "limit", 20, 50)
	chats := []interface{}{}
	for _, conversation := range conversations {
		if len(chats) == limit {
			break
		}
		if conversation == nil || conversation.ID == from.ConversationID || conversation.Scope != from.Scope || conversation.Owner != from.Owner {
			continue
		}
		tree, _ := s.latest(ctx, Chat{Scope: from.Scope, Owner: from.Owner, ConversationID: conversation.ID})
		paths := tree.Paths()
		title := strings.ToLower(conversation.Title)
		var shown []string
		matched := true
		for _, word := range words {
			if !strings.Contains(title, word) {
				matched = false
			}
		}
		for _, name := range paths {
			if len(shown) == listedChatFiles {
				break
			}
			lower := strings.ToLower(name)
			fits := true
			for _, word := range words {
				if !strings.Contains(lower, word) && !strings.Contains(title, word) {
					fits = false
				}
			}
			if fits {
				shown = append(shown, name)
			}
		}
		if !matched && len(shown) == 0 {
			continue
		}
		chat := map[string]interface{}{"id": conversation.ID, "title": conversation.Title, "updatedAt": conversation.UpdatedAt.UTC().Format(time.RFC3339), "fileCount": len(paths), "files": shown}
		if len(paths) > len(shown) {
			chat["moreFiles"] = len(paths) - len(shown)
		}
		chats = append(chats, chat)
	}
	return map[string]interface{}{"chats": chats}, nil
}

// copyFromChat copies a file, or a directory and everything in it, from
// another of the Agent's chats. Copies share the stored bytes.
func (s *Store) copyFromChat(ctx context.Context, chat Chat, author Author, args map[string]interface{}) (map[string]interface{}, error) {
	source, err := s.otherChat(ctx, chat, text(args, workspace.ChatArgument))
	if err != nil {
		return nil, err
	}
	from, err := Clean(text(args, "path"))
	if err != nil {
		return nil, err
	}
	to := from
	if destination := text(args, "destination"); strings.TrimSpace(destination) != "" {
		if to, err = Clean(destination); err != nil {
			return nil, err
		}
	}
	theirs, err := s.Head(ctx, source)
	if err != nil {
		return nil, err
	}
	names := matching(theirs.Tree, from)
	if len(names) == 0 {
		return nil, missing(theirs.Tree, from)
	}
	ours, err := s.Head(ctx, chat)
	if err != nil {
		return nil, err
	}
	modified := s.now().UTC()
	changes := make([]Change, 0, len(names))
	copied := make([]string, 0, len(names))
	for _, name := range names {
		if s.Private != nil && s.Private(name) {
			continue
		}
		target := to + strings.TrimPrefix(name, from)
		entry := theirs.Tree.Files[name]
		entry.ModifiedAt, entry.Origin = modified, fmt.Sprintf("chat:%s@%d/%s", source.ConversationID, theirs.Version, name)
		changes = append(changes, Change{Path: target, Entry: &entry, Expect: Expectation(ours.Tree, target)})
		copied = append(copied, target)
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("%w: %s cannot be copied", ErrPath, from)
	}
	snapshot, err := s.Commit(ctx, chat, author, changes)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"copied": copied, "version": snapshot.Version}, nil
}
