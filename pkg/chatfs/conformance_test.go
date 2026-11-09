package chatfs

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/workspace"
	"github.com/axiom-studio/openseal/pkg/workspace/workspacetest"
)

// chatMachine is a chat's files as a machine: chat paths in and out, and a
// failure as the error result a chat returns.
type chatMachine struct {
	store  *Store
	chat   Chat
	author Author
}

func (chatMachine) Path(rel string) string  { return rel }
func (chatMachine) Shown(rel string) string { return rel }

func (m chatMachine) Do(ctx context.Context, op string, args map[string]interface{}) (map[string]interface{}, error) {
	result, err := m.store.Execute(ctx, m.chat, m.author, workspace.OperationPrefix+op, args)
	if err != nil {
		return nil, err
	}
	if failed, _ := result["isError"].(bool); failed {
		return nil, errors.New(result["error"].(string))
	}
	return result, nil
}

func TestChatFilesConformToTheMachineOperations(t *testing.T) {
	workspacetest.Conformance(t, func(t *testing.T) workspacetest.Machine {
		store, chat, author := newTestStore(t)
		return chatMachine{store: store, chat: chat, author: author}
	}, workspacetest.Options{Files: true})
}
