// Package workspace defines the files a hosted Agent turn works on: its chat's
// own files, which every Agent in the chat shares and the host stores with the
// chat (see pkg/chatfs), and the native tools that read and change them.
package workspace

import (
	"errors"
	"strings"
)

// Authority grants one hosted turn full access to its chat's files, read
// access to the other chats of the same Agent, and commands in the chat's
// sandbox, which reaches the internet but not the host's network. It is
// credential-free: the host proves the chat from the persisted Run.
type Authority struct {
	ConversationID string `json:"conversationId"`
}

// ChatCommandSeconds bounds one command in a chat's sandbox.
const ChatCommandSeconds = 600

// ChatAuthority grants a turn its chat's files.
func ChatAuthority(conversationID string) *Authority {
	return &Authority{ConversationID: strings.TrimSpace(conversationID)}
}

func (a Authority) Validate() error {
	if strings.TrimSpace(a.ConversationID) == "" || len(a.ConversationID) > 256 {
		return errors.New("chat Workspace authority is invalid")
	}
	return nil
}
