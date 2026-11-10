package chatfs

import (
	"bytes"
	"context"
	"errors"
	"path"
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

// Seed puts a file as a command in the chat's sandbox leaves it.
func (m chatMachine) Seed(t *testing.T, rel string, data []byte) {
	t.Helper()
	entry, err := m.store.Put(context.Background(), m.chat, bytes.NewReader(data), int64(len(data)), workspace.MachineMediaType(path.Base(rel), data))
	if err == nil {
		_, err = m.store.Commit(context.Background(), m.chat, m.author, []Change{{Path: rel, Entry: &entry}})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatFilesConformToTheMachineOperations(t *testing.T) {
	workspacetest.Conformance(t, func(t *testing.T) workspacetest.Machine {
		store, chat, author := newTestStore(t)
		return chatMachine{store: store, chat: chat, author: author}
	}, workspacetest.Options{Files: true})
}
