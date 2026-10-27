package workspace

import (
	"errors"
	"strings"
)

// Access is the framework-owned access level granted to one hosted Agent turn.
// It is not a Skill permission: the host derives it from the Agent deployment
// and enforces it when scheduling work against the Workspace.
type Access string

const (
	AccessReadOnly  Access = "read_only"
	AccessReadWrite Access = "read_write"
)

// Authority is an immutable, credential-free grant from the OpenSeal kernel to
// a trusted Agent host. Storage handles, mount paths, cluster identities, and
// credentials remain host-owned and are never part of this portable contract.
type Authority struct {
	Workspace Spec `json:"workspace"`
	// ConversationID, when set, makes the Workspace that chat's own files
	// (see pkg/chatfs) rather than a volume of the Agent: every Agent in the
	// chat shares them, and the host stores them with the chat.
	ConversationID string `json:"conversationId,omitempty"`
}

// ChatID is the Workspace ID of every chat's files.
const ChatID = "chat"

// ChatAuthority grants a turn full access to its chat's files.
func ChatAuthority(conversationID string) *Authority {
	spec := DefaultSpec()
	spec.ID, spec.DisplayName = ChatID, "Chat files"
	return &Authority{Workspace: spec, ConversationID: strings.TrimSpace(conversationID)}
}

func (a Authority) Validate() error {
	if err := a.Workspace.Validate(); err != nil {
		return err
	}
	if a.ConversationID != "" && (a.Workspace.ID != ChatID || a.Workspace.Policy.Filesystem != AccessReadWrite || a.Workspace.Policy.Git.Enabled || len(a.Workspace.Policy.CredentialBindings) != 0) {
		return errors.New("chat Workspace authority is invalid")
	}
	return nil
}

// Select returns the deployment's exact default Workspace. Callers must not
// silently select another Workspace because the default identity is durable
// Agent state and determines the host-side storage authority.
func Select(defaultID string, workspaces []Spec) (*Spec, error) {
	defaultID = strings.TrimSpace(defaultID)
	if defaultID == "" {
		return nil, nil
	}
	for index := range workspaces {
		if workspaces[index].ID == defaultID {
			selected := workspaces[index]
			return &selected, nil
		}
	}
	return nil, errors.New("default workspace does not exist")
}
